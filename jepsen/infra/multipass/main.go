// Command multipass is a Pulumi program creating the Jepsen suite's
// machines as Multipass VMs on this machine: a control host and the nodes,
// root reachable over SSH with a key of its own. What runs on them is
// jepsen-run's (../run). Its outputs are an inventory: hosts and
// privateKey.
//
// Pulumi has no Multipass provider: each VM is a local command around the
// multipass CLI, and its address is read anew on every update, a VM
// started again perhaps having another.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi-tls/sdk/v5/go/tls"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"

	"github.com/sylr/fix2/jepsen/infra/inventory"
)

// ownerFile is where a VM records the stack that made it.
const ownerFile = "/etc/fix2-jepsen-owner"

// launch creates the VM and records its owner, or takes the one of that
// name already there if this stack made it (a launch whose state Pulumi
// failed to record), and authorizes the key for root. Run again on an
// update, it replaces its own key alone: the control host's, which
// jepsen-run adds, stays.
const launch = `set -eu
if ! multipass info "$VM" >/dev/null 2>&1; then
	multipass launch "$IMAGE" --name "$VM" --cpus "$CPUS" --memory "$MEMORY" --disk "$DISK" --timeout 900 >&2
	multipass exec "$VM" -- sudo sh -c 'printf "%s\n" "$1" >` + ownerFile + `' - "$OWNER"
else
	owner=$(multipass exec "$VM" -- sh -c 'cat ` + ownerFile + ` 2>/dev/null || true')
	if [ "$owner" != "$OWNER" ]; then
		echo "VM $VM exists, made by ${owner:-no stack}, not $OWNER: purge it or set prefix" >&2
		exit 1
	fi
fi
multipass exec "$VM" -- sudo sh -c '
set -eu
install -d -m 700 /root/.ssh
touch /root/.ssh/authorized_keys
{
	grep -v " fix2-jepsen-admin\$" /root/.ssh/authorized_keys || true
	printf "%s fix2-jepsen-admin\n" "$1"
} >/root/.ssh/authorized_keys.new
chmod 600 /root/.ssh/authorized_keys.new
mv /root/.ssh/authorized_keys.new /root/.ssh/authorized_keys' - "$(printf %s "$PUBKEY" | tr -d '\n')"
echo "$VM"
`

// purge deletes the VM if this stack made it, else leaves it. A VM gone
// is deleted; one whose owner cannot be read is an error, not forgotten.
const purge = `set -eu
if ! out=$(multipass info "$VM" 2>&1); then
	case $out in
	*"does not exist"*) exit 0 ;;
	*) echo "$out" >&2; exit 1 ;;
	esac
fi
if ! owner=$(multipass exec "$VM" -- sh -c 'cat ` + ownerFile + ` 2>/dev/null || true'); then
	echo "VM $VM: its owner cannot be read: start it, or purge it by hand" >&2
	exit 1
fi
if [ "$owner" != "$OWNER" ]; then
	echo "VM $VM made by ${owner:-no stack}, not $OWNER: left" >&2
	exit 0
fi
multipass delete --purge "$VM"`

type vm struct {
	name, cpus, memory, disk string
}

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg := config.New(ctx, "")
		image := cfg.Get("image")
		prefix := cfg.Get("prefix")
		if prefix == "" {
			prefix = "fix2-jepsen-" + ctx.Stack()
		}
		vms := []vm{{inventory.Control, fmt.Sprint(cfg.GetInt("controlCpus")), cfg.Get("controlMemory"), cfg.Get("controlDisk")}}
		for i := 1; i <= cfg.GetInt("nodes"); i++ {
			vms = append(vms, vm{fmt.Sprintf("n%d", i), fmt.Sprint(cfg.GetInt("nodeCpus")), cfg.Get("nodeMemory"), cfg.Get("nodeDisk")})
		}

		key, err := tls.NewPrivateKey(ctx, "admin", &tls.PrivateKeyArgs{Algorithm: pulumi.String("ED25519")})
		if err != nil {
			return err
		}
		// The stack's name, which another backend may give a stack too,
		// and its key's, which is its own.
		owner := key.PublicKeyOpenssh.ApplyT(func(pub string) string {
			sum := sha256.Sum256([]byte(pub))
			return fmt.Sprintf("%s/%s/%x", ctx.Project(), ctx.Stack(), sum[:8])
		}).(pulumi.StringOutput)

		var hosts pulumi.Array
		for _, m := range vms {
			name := prefix + "-" + m.name
			env := pulumi.StringMap{
				"VM":     pulumi.String(name),
				"IMAGE":  pulumi.String(image),
				"CPUS":   pulumi.String(m.cpus),
				"MEMORY": pulumi.String(m.memory),
				"DISK":   pulumi.String(m.disk),
				"PUBKEY": key.PublicKeyOpenssh,
				"OWNER":  owner,
			}
			launched, err := local.NewCommand(ctx, "vm-"+m.name, &local.CommandArgs{
				Create:      pulumi.String(launch),
				Delete:      pulumi.String(purge),
				Environment: env,
				// Another name, image or size makes another VM.
				Triggers: pulumi.Array{pulumi.String(name), pulumi.String(image), pulumi.String(m.cpus), pulumi.String(m.memory), pulumi.String(m.disk)},
				// The new VM has the old one's name: created first, it
				// would take the old one, which deleting it then purges.
			}, pulumi.DeleteBeforeReplace(true))
			if err != nil {
				return err
			}
			// Its name, once launched.
			launchedName := launched.Stdout.ApplyT(func(string) string { return name }).(pulumi.StringOutput)
			info := local.RunOutput(ctx, local.RunOutputArgs{
				Command:     pulumi.String(`multipass info "$VM" --format json`),
				Environment: pulumi.StringMap{"VM": launchedName},
			})
			address := info.Stdout().ApplyT(func(out string) (string, error) { return address(name, out) }).(pulumi.StringOutput)
			hosts = append(hosts, pulumi.Map{
				"name":    pulumi.String(m.name),
				"address": address,
				"user":    pulumi.String("root"),
				"id":      launched.ID(),
			})
		}

		ctx.Export("hosts", hosts)
		ctx.Export("privateKey", pulumi.ToSecret(key.PrivateKeyOpenssh))
		return nil
	})
}

// address is the first IPv4 address multipass info gives the VM.
func address(name, out string) (string, error) {
	var info struct {
		Info map[string]struct {
			State string   `json:"state"`
			IPv4  []string `json:"ipv4"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return "", fmt.Errorf("multipass info %s: %w", name, err)
	}
	vm := info.Info[name]
	if len(vm.IPv4) == 0 {
		return "", fmt.Errorf("multipass info %s: no address, the VM %s: run `multipass start %s`", name, vm.State, name)
	}
	return vm.IPv4[0], nil
}
