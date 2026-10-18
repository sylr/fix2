// fixpecho is a FIXP echo server and its load client, on fix2's
// fixp/session: the server answers each application message with itself,
// the client sends messages, at most a window of them awaiting their
// echo, and reports the round trips. Both keep their sessions in memory.
//
//	fixpecho server --listen 127.0.0.1:7760
//	fixpecho client --connect 127.0.0.1:7760 --count 100000 --window 100
//
// SIGINT or SIGTERM stops either: the server terminates its sessions
// first, the client its connection.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
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

// errWriter writes to w, and keeps the first error a write returned.
type errWriter struct {
	w   io.Writer
	mu  sync.Mutex
	err error
}

func (w *errWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if err != nil {
		w.mu.Lock()
		if w.err == nil {
			w.err = err
		}
		w.mu.Unlock()
	}
	return n, err
}

// failed returns the first error a write returned, nil if none did.
func (w *errWriter) failed() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// run runs a command, and returns the exit code: 2 for a usage error, 1 for
// a command that failed.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// A failed write of stdout, cobra's own (help, --version, completion)
	// included, fails the command: exit 1.
	out := &errWriter{w: stdout}
	root := newCommand(out, stderr)
	// Given nil, cobra would read os.Args.
	root.SetArgs(append([]string{}, args...))
	err := root.ExecuteContext(ctx)
	werr := out.failed()
	if err == nil && werr == nil {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "fixpecho:", err)
	}
	// A failed write of stdout, unless it is the error told, is told too.
	if werr != nil && !errors.Is(err, werr) {
		fmt.Fprintln(stderr, "fixpecho:", werr)
	}
	if errors.As(err, new(runError)) || werr != nil {
		return 1
	}
	fmt.Fprintln(stderr, "Run 'fixpecho --help' for usage.")
	return 2
}

// newCommand returns the fixpecho command, writing reports to stdout and
// logs to stderr.
func newCommand(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "fixpecho",
		Short: "A FIXP echo server and its load client",
		Long: `A FIXP echo server and its load client, on fix2's fixp/session: the
server answers each application message with itself, the client sends
messages, at most a window of them awaiting their echo, and reports the
round trips. Both keep their sessions in memory.

SIGINT or SIGTERM stops either: the server terminates its sessions
first, the client its connection.`,
		Example: `  fixpecho server --listen 127.0.0.1:7760
  fixpecho client --connect 127.0.0.1:7760 --count 100000 --window 100`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Without a command, nothing asked for was done: a usage error.
		RunE: func(*cobra.Command, []string) error {
			return errors.New("no command given")
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	// -v is --verbose in the other contribs: no shorthand for --version.
	root.Flags().Bool("version", false, "version for fixpecho")
	root.AddCommand(serverCommand(stderr), clientCommand(stdout, stderr), &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run:   func(*cobra.Command, []string) { fmt.Fprintln(stdout, Version) },
	})
	return root
}

// usageError is a bad flag value.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }

// runError is the failure of a command that ran: exit code 1.
type runError struct{ err error }

func (e runError) Error() string { return e.err.Error() }
func (e runError) Unwrap() error { return e.err }

// ran returns the error of a command that ran: a runError, unless it is a
// usage error.
func ran(err error) error {
	if err == nil || errors.As(err, new(usageError)) {
		return err
	}
	return runError{err}
}

// common are the flags of both commands.
type common struct {
	token     string
	keepalive time.Duration
	logLevel  string
}

func (c *common) register(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.StringVar(&c.token, "token", "", "credentials: the server requires them, the client sends them (none if empty)")
	fs.DurationVar(&c.keepalive, "keepalive", time.Second, "keepalive interval")
	fs.StringVar(&c.logLevel, "log-level", "info", "log level: debug, info, warn or error")
}

func (c *common) logger(stderr io.Writer) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.logLevel)); err != nil {
		return nil, usagef("--log-level: %v", err)
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

// serverFlags are the flags of server alone.
type serverFlags struct {
	listen, flow, clientFlows string
	echoWait                  time.Duration
	maxPending                int
	cert, key                 string
}

func serverCommand(stderr io.Writer) *cobra.Command {
	var c common
	var f serverFlags
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Answer each application message with itself",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ran(runServer(cmd.Context(), &c, &f, stderr))
		},
	}
	c.register(cmd)
	fs := cmd.Flags()
	fs.StringVar(&f.listen, "listen", "127.0.0.1:7760", "address to listen on")
	fs.StringVar(&f.flow, "flow", "Recoverable", "the server's flow: Recoverable, Idempotent or Unsequenced")
	fs.StringVar(&f.clientFlows, "client-flows", "Recoverable,Idempotent,Unsequenced", "client flows accepted, comma-separated")
	fs.DurationVar(&f.echoWait, "echo-wait", time.Second, "how long an echo waits for room in the session's queue before the connection terminates")
	fs.IntVar(&f.maxPending, "max-pending", 0, "the session's queue, in messages (the engine's default if 0)")
	fs.StringVar(&f.cert, "tls-cert", "", "TLS certificate file (PEM), with --tls-key: TLS")
	fs.StringVar(&f.key, "tls-key", "", "TLS key file (PEM)")
	return cmd
}

func runServer(ctx context.Context, c *common, f *serverFlags, stderr io.Writer) error {
	logger, err := c.logger(stderr)
	if err != nil {
		return err
	}
	o := serverOptions{token: c.token, keepalive: c.keepalive, echoWait: f.echoWait, maxPending: f.maxPending, logger: logger}
	if o.flow, err = parseFlow(f.flow); err != nil {
		return err
	}
	if o.clientFlows, err = parseFlows(f.clientFlows); err != nil {
		return err
	}
	if f.echoWait <= 0 {
		return usagef("--echo-wait must be positive")
	}
	if f.maxPending < 0 {
		return usagef("--max-pending must not be negative")
	}
	tlsConfig, err := serverTLS(f.cert, f.key)
	if err != nil {
		return usageError{err}
	}
	if c.token == "" {
		logger.Warn("no --token: any client is accepted")
	}
	l, err := tcp.Listen(f.listen, tlsConfig)
	if err != nil {
		return err
	}
	logger.Info("listening", "addr", l.Addr().String(), "tls", tlsConfig != nil, "flow", o.flow.String())
	return serve(ctx, o, l)
}

// clientFlags are the flags of client alone.
type clientFlags struct {
	connect, flow, serverFlows string
	count, window, size        int
	timeout                    time.Duration
	tls                        bool
	ca, serverName             string
}

func clientCommand(stdout, stderr io.Writer) *cobra.Command {
	var c common
	var f clientFlags
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Send messages, a window of them awaiting their echo, and report the round trips",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ran(runClient(cmd.Context(), &c, &f, stdout, stderr))
		},
	}
	c.register(cmd)
	fs := cmd.Flags()
	fs.StringVar(&f.connect, "connect", "127.0.0.1:7760", "server address")
	fs.StringVar(&f.flow, "flow", "Idempotent", "the client's flow: Recoverable, Idempotent or Unsequenced")
	fs.StringVar(&f.serverFlows, "server-flows", "", "server flows accepted, comma-separated (any if empty)")
	fs.IntVar(&f.count, "count", 10000, "messages to send")
	fs.IntVar(&f.window, "window", 100, "messages awaiting their echo, at most")
	fs.IntVar(&f.size, "size", 64, fmt.Sprintf("message size, its SBE header included, from %d to %d bytes", minSize, maxSize))
	fs.DurationVar(&f.timeout, "timeout", time.Minute, "time limit of the whole run")
	fs.BoolVar(&f.tls, "tls", false, "connect with TLS")
	fs.StringVar(&f.ca, "tls-ca", "", "CA certificate file (PEM) to verify the server with, instead of the system's")
	fs.StringVar(&f.serverName, "tls-server-name", "", "server name to verify, the address's host if empty")
	return cmd
}

func runClient(ctx context.Context, c *common, f *clientFlags, stdout, stderr io.Writer) error {
	logger, err := c.logger(stderr)
	if err != nil {
		return err
	}
	o := clientOptions{token: c.token, keepalive: c.keepalive, count: f.count, window: f.window, size: f.size, timeout: f.timeout, logger: logger}
	if o.flow, err = parseFlow(f.flow); err != nil {
		return err
	}
	if o.serverFlows, err = parseFlows(f.serverFlows); err != nil {
		return err
	}
	switch {
	case o.count < 1:
		return usagef("--count must be positive")
	case o.window < 1:
		return usagef("--window must be positive")
	case o.size < minSize || o.size > maxSize:
		return usagef("--size must be from %d to %d", minSize, maxSize)
	case o.timeout <= 0:
		return usagef("--timeout must be positive")
	}
	tlsConfig, err := clientTLS(f.tls, f.ca, f.serverName)
	if err != nil {
		return usageError{err}
	}
	r, err := load(ctx, o, &tcp.Dialer{Addresses: []string{f.connect}, TLS: tlsConfig})
	report(stdout, r)
	return err
}
