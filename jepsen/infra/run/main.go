// Command run is a Pulumi program setting up hosts reachable over SSH for
// the Jepsen suite (docs/jepsen.md): the nodes' tools, kernel modules and
// LazyFS; the control host's JDK, Leiningen and Go; the key the nodes
// authorize. It then copies this repository's sources to the control host
// and builds the node binary and the test's dependencies there. Running
// the test is not its business: see jepsen/Makefile.
//
// The hosts come from a stack providing machines (config inventory, such
// as ../multipass), or from this stack's config: hosts, a list of
// inventory.Host, and privateKey, the key they authorize.
package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/pulumi/pulumi-command/sdk/go/command/remote"
	"github.com/pulumi/pulumi-tls/sdk/v5/go/tls"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"

	"github.com/sylr/fix2/jepsen/infra/inventory"
)

var (
	//go:embed scripts/node.sh
	nodeScript string
	//go:embed scripts/control.sh
	controlScript string
	//go:embed scripts/build.sh
	buildScript string
)

// Where the build leaves the sources' jepsen directory on the control
// host, and where the archive is copied: not /tmp, which a reboot may
// empty while the copy stays recorded, but a directory the SSH user owns,
// the copy being made without sudo.
const (
	jepsenDir     = "/opt/fix2-jepsen/src/jepsen"
	remoteArchive = "/var/lib/fix2-jepsen/upload/src.tar.gz"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg := config.New(ctx, "")
		hosts, adminKey, err := inventoryOf(ctx, cfg)
		if err != nil {
			return err
		}
		control, nodes := inventory.Split(hosts)

		jepsenKey, err := tls.NewPrivateKey(ctx, "jepsen", &tls.PrivateKeyArgs{
			// Jepsen's SSH client (JSch) predates ed25519.
			Algorithm: pulumi.String("RSA"),
			RsaBits:   pulumi.Int(4096),
		})
		if err != nil {
			return err
		}

		var lines, names []string
		for _, h := range hosts {
			lines = append(lines, h.Peer()+" "+h.Name)
		}
		for _, n := range nodes {
			names = append(names, n.Name)
		}
		hostsBlock := strings.Join(lines, "\n")

		var nodeSetups []pulumi.Resource
		var nodeIDs pulumi.Array
		for _, n := range nodes {
			env := pulumi.StringMap{
				"HOSTS":          pulumi.String(hostsBlock),
				"CONTROL_KEY":    jepsenKey.PublicKeyOpenssh.ApplyT(strings.TrimSpace).(pulumi.StringOutput),
				"LAZYFS_VERSION": pulumi.String(cfg.Get("lazyfsVersion")),
			}
			setup, err := remote.NewCommand(ctx, "setup-"+n.Name, script(n, adminKey, env, nodeScript))
			if err != nil {
				return err
			}
			nodeSetups = append(nodeSetups, setup)
			nodeIDs = append(nodeIDs, pulumi.String(n.ID))
		}

		controlArgs := script(control, adminKey, pulumi.StringMap{
			"HOSTS":        pulumi.String(hostsBlock),
			"NODES":        pulumi.String(strings.Join(names, " ")),
			"JEPSEN_KEY":   jepsenKey.PrivateKeyOpenssh,
			"GO_VERSION":   pulumi.String(cfg.Get("goVersion")),
			"LEIN_VERSION": pulumi.String(cfg.Get("leinVersion")),
			"UPLOAD_USER":  pulumi.String(control.User),
		}, controlScript)
		// The nodes' host keys are taken anew when one is made again.
		controlArgs.Triggers = append(controlArgs.Triggers.(pulumi.Array), nodeIDs...)
		controlSetup, err := remote.NewCommand(ctx, "setup-control", controlArgs, pulumi.DependsOn(nodeSetups))
		if err != nil {
			return err
		}

		archive, hash, err := sourceArchive()
		if err != nil {
			return err
		}
		copied, err := remote.NewCopyToRemote(ctx, "sources", &remote.CopyToRemoteArgs{
			Connection: connection(control, adminKey),
			Source:     pulumi.NewFileAsset(archive),
			RemotePath: pulumi.String(remoteArchive),
			// A control host made again, or moved, has none: copied again.
			Triggers: pulumi.Array{pulumi.String(hash), pulumi.String(control.ID), pulumi.String(control.Address)},
		}, pulumi.DependsOn([]pulumi.Resource{controlSetup}))
		if err != nil {
			return err
		}
		buildArgs := script(control, adminKey, pulumi.StringMap{
			"ARCHIVE": pulumi.String(remoteArchive),
			"HASH":    pulumi.String(hash),
		}, buildScript)
		// Set up again, the control host builds again.
		buildArgs.Triggers = append(buildArgs.Triggers.(pulumi.Array), controlSetup.ID())
		if _, err := remote.NewCommand(ctx, "build", buildArgs, pulumi.DependsOn([]pulumi.Resource{copied})); err != nil {
			return err
		}

		ctx.Export("control", pulumi.String(control.Address))
		ctx.Export("user", pulumi.String(control.User))
		ctx.Export("nodes", pulumi.String(strings.Join(names, ",")))
		ctx.Export("jepsenDir", pulumi.String(jepsenDir))
		ctx.Export("sourceHash", pulumi.String(hash))
		ctx.Export("privateKey", adminKey)
		return nil
	})
}

// inventoryOf reads the hosts and the key they authorize, from the stack
// config inventory names, else from this stack's config.
func inventoryOf(ctx *pulumi.Context, cfg *config.Config) ([]inventory.Host, pulumi.StringOutput, error) {
	ref := cfg.Get("inventory")
	if ref == "" {
		var raw any
		cfg.RequireObject("hosts", &raw)
		hosts, err := inventory.Parse(raw)
		return hosts, cfg.RequireSecret("privateKey"), err
	}
	sr, err := pulumi.NewStackReference(ctx, ref, nil)
	if err != nil {
		return nil, pulumi.StringOutput{}, err
	}
	hostsOut, err := sr.GetOutputDetails("hosts")
	if err != nil {
		return nil, pulumi.StringOutput{}, err
	}
	keyOut, err := sr.GetOutputDetails("privateKey")
	if err != nil {
		return nil, pulumi.StringOutput{}, err
	}
	key, _ := keyOut.SecretValue.(string)
	if key == "" {
		return nil, pulumi.StringOutput{}, fmt.Errorf("stack %s: no privateKey output: is it up?", ref)
	}
	hosts, err := inventory.Parse(hostsOut.Value)
	return hosts, pulumi.ToSecret(pulumi.String(key)).(pulumi.StringOutput), err
}

func connection(h inventory.Host, key pulumi.StringOutput) remote.ConnectionArgs {
	return remote.ConnectionArgs{
		Host:           pulumi.String(h.Address),
		User:           pulumi.String(h.User),
		PrivateKey:     key,
		DialErrorLimit: pulumi.Int(30),
	}
}

// script runs body on h as root, the variables env set before it. Its
// output is what the script says, its logs left on the host: command
// output is kept in the stack's state.
func script(h inventory.Host, key pulumi.StringOutput, env pulumi.StringMap, body string) *remote.CommandArgs {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var vals []any
	for _, k := range keys {
		vals = append(vals, env[k])
	}
	stdin := pulumi.All(vals...).ApplyT(func(vs []any) string {
		var b strings.Builder
		for i, k := range keys {
			fmt.Fprintf(&b, "%s=%s\nexport %s\n", k, quote(vs[i].(string)), k)
		}
		b.WriteString(body)
		return b.String()
	}).(pulumi.StringOutput)
	sum := sha256.Sum256([]byte(body))
	sudo := ""
	if h.User != "root" {
		sudo = "sudo "
	}
	return &remote.CommandArgs{
		Connection: connection(h, key),
		Create:     pulumi.String(sudo + "sh -s"),
		// The keys it may carry are secrets.
		Stdin:    pulumi.ToSecret(stdin).(pulumi.StringOutput),
		Triggers: pulumi.Array{pulumi.String(hex.EncodeToString(sum[:])), pulumi.String(h.ID)},
	}
}

// quote quotes s for the shell.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
