// fixpecho is a FIXP echo server and its load client, on fix2's
// fixp/session: the server answers each application message with itself,
// the client sends messages, at most a window of them awaiting their
// echo, and reports the round trips. Both keep their sessions in memory.
//
//	fixpecho server -listen 127.0.0.1:7760
//	fixpecho client -connect 127.0.0.1:7760 -count 100000 -window 100
//
// SIGINT or SIGTERM stops either: the server terminates its sessions
// first, the client its connection.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/transport/tcp"
)

// Version is set at build time.
var Version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usage = `usage: fixpecho server [flags]
       fixpecho client [flags]
       fixpecho version

Run "fixpecho server -h" or "fixpecho client -h" for the flags.
`

// run runs a command, and returns the exit code: 2 for a usage error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "server":
		err = runServer(ctx, args[1:], stderr)
	case "client":
		err = runClient(ctx, args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, Version)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, new(usageError)):
		fmt.Fprintln(stderr, "fixpecho:", err)
		return 2
	default:
		fmt.Fprintln(stderr, "fixpecho:", err)
		return 1
	}
}

// usageError is a bad flag value.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }

// common are the flags of both commands.
type common struct {
	token     string
	keepalive time.Duration
	logLevel  string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.token, "token", "", "credentials: the server requires them, the client sends them (none if empty)")
	fs.DurationVar(&c.keepalive, "keepalive", time.Second, "keepalive interval")
	fs.StringVar(&c.logLevel, "log-level", "info", "log level: debug, info, warn or error")
}

func (c *common) logger(stderr io.Writer) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.logLevel)); err != nil {
		return nil, usagef("-log-level: %v", err)
	}
	return slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})), nil
}

func parseFlow(s string) (fixp.FlowType, error) {
	for _, f := range []fixp.FlowType{fixp.Recoverable, fixp.Idempotent, fixp.Unsequenced, fixp.None} {
		if strings.EqualFold(s, f.String()) {
			return f, nil
		}
	}
	return 0, usagef("unknown flow %q: Recoverable, Idempotent, Unsequenced or None", s)
}

func parseFlows(s string) ([]fixp.FlowType, error) {
	if s == "" {
		return nil, nil
	}
	var flows []fixp.FlowType
	for _, name := range strings.Split(s, ",") {
		f, err := parseFlow(strings.TrimSpace(name))
		if err != nil {
			return nil, err
		}
		flows = append(flows, f)
	}
	return flows, nil
}

func runServer(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("fixpecho server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	c.register(fs)
	listen := fs.String("listen", "127.0.0.1:7760", "address to listen on")
	flow := fs.String("flow", "Recoverable", "the server's flow: Recoverable, Idempotent or Unsequenced")
	clientFlows := fs.String("client-flows", "Recoverable,Idempotent,Unsequenced", "client flows accepted, comma-separated")
	echoWait := fs.Duration("echo-wait", time.Second, "how long an echo waits for room in the session's queue before the connection terminates")
	maxPending := fs.Int("max-pending", 0, "the session's queue, in messages (the engine's default if 0)")
	cert := fs.String("tls-cert", "", "TLS certificate file (PEM), with -tls-key: TLS")
	key := fs.String("tls-key", "", "TLS key file (PEM)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("unexpected arguments %q", fs.Args())
	}
	logger, err := c.logger(stderr)
	if err != nil {
		return err
	}
	o := serverOptions{token: c.token, keepalive: c.keepalive, echoWait: *echoWait, maxPending: *maxPending, logger: logger}
	if o.flow, err = parseFlow(*flow); err != nil {
		return err
	}
	if o.clientFlows, err = parseFlows(*clientFlows); err != nil {
		return err
	}
	if *echoWait <= 0 {
		return usagef("-echo-wait must be positive")
	}
	if *maxPending < 0 {
		return usagef("-max-pending must not be negative")
	}
	tlsConfig, err := serverTLS(*cert, *key)
	if err != nil {
		return usageError{err}
	}
	if c.token == "" {
		logger.Warn("no -token: any client is accepted")
	}
	l, err := tcp.Listen(*listen, tlsConfig)
	if err != nil {
		return err
	}
	logger.Info("listening", "addr", l.Addr().String(), "tls", tlsConfig != nil, "flow", o.flow.String())
	return serve(ctx, o, l)
}

func runClient(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("fixpecho client", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	c.register(fs)
	connect := fs.String("connect", "127.0.0.1:7760", "server address")
	flow := fs.String("flow", "Idempotent", "the client's flow: Recoverable, Idempotent or Unsequenced")
	serverFlows := fs.String("server-flows", "", "server flows accepted, comma-separated (any if empty)")
	count := fs.Int("count", 10000, "messages to send")
	window := fs.Int("window", 100, "messages awaiting their echo, at most")
	size := fs.Int("size", 64, fmt.Sprintf("message size, its SBE header included, from %d to %d bytes", minSize, maxSize))
	timeout := fs.Duration("timeout", time.Minute, "time limit of the whole run")
	useTLS := fs.Bool("tls", false, "connect with TLS")
	ca := fs.String("tls-ca", "", "CA certificate file (PEM) to verify the server with, instead of the system's")
	serverName := fs.String("tls-server-name", "", "server name to verify, the address's host if empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("unexpected arguments %q", fs.Args())
	}
	logger, err := c.logger(stderr)
	if err != nil {
		return err
	}
	o := clientOptions{token: c.token, keepalive: c.keepalive, count: *count, window: *window, size: *size, timeout: *timeout, logger: logger}
	if o.flow, err = parseFlow(*flow); err != nil {
		return err
	}
	if o.serverFlows, err = parseFlows(*serverFlows); err != nil {
		return err
	}
	switch {
	case o.count < 1:
		return usagef("-count must be positive")
	case o.window < 1:
		return usagef("-window must be positive")
	case o.size < minSize || o.size > maxSize:
		return usagef("-size must be from %d to %d", minSize, maxSize)
	case o.timeout <= 0:
		return usagef("-timeout must be positive")
	}
	tlsConfig, err := clientTLS(*useTLS, *ca, *serverName)
	if err != nil {
		return usageError{err}
	}
	r, err := load(ctx, o, &tcp.Dialer{Addresses: []string{*connect}, TLS: tlsConfig})
	report(stdout, r)
	return err
}
