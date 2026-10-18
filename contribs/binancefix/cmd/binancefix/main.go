// Command binancefix checks, against Binance's Spot testnet, the
// assumptions the binancefix client makes where Binance's FIX SBE
// documentation is silent.
//
//	binancefix logon [flags]           log on, show the LogonAck, log out
//	binancefix probe [flags] [PROBE...] run the probes (all if none named)
//	binancefix probes                  list the probes and their assumptions
//	binancefix version
//
// logon logs on to the order entry endpoint, shows Binance's LogonAck and
// logs out (M0, settled: Binance checks the microsecond form, us).
// --signature-time chooses the text of the SendingTime the Logon's
// signature is made of (us, the client's default, ms, s, ns, unix-us,
// unix-ms), or auto to try them in turn, twice HeartBtInt apart, until
// Binance accepts one: 0 when it is the default, 3 when another. It logs
// on to the testnet unless --production is given.
//
// probe places small orders below the market on the testnet, and only
// there, --production or not: each probe exchanges a few messages and
// judges an assumption confirmed, contradicted or inconclusive (all
// confirmed on 2026-10-17: the probes are the regression check). Whatever
// it placed is cancelled when it is done, on error or interrupt too, and it
// sends at most one request per --interval. --price-far, the price of the
// limit orders, has no default: below the market, inside both
// PERCENT_PRICE_BY_SIDE bands of the symbol (70000 for BTCUSDT at about
// 81,500); only logon-seqnum, fill and limits run without it.
//
// The API key is --api-key, --api-key-file (a file holding it) or
// BINANCE_API_KEY; its Ed25519 private key the PEM file --key-file or
// BINANCE_PRIVATE_KEY names. Order entry needs the FIX_API permission.
//
// The exit code is 0 when all went as the client assumes, 3 when an
// assumption is contradicted or a probe inconclusive, 1 on failure (a
// refused Logon, a lost connection, orders or a position left), 2 on a usage
// error. SIGINT or SIGTERM stops the command, after its cleanup; a second
// one kills it.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/sylr/fix2/contribs/binancefix"
	"github.com/sylr/fix2/contribs/binancefix/internal/probe"
	"github.com/sylr/fix2/transport"
)

// Version is the version of binancefix, set when built by the Makefile.
var Version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// The first signal starts the cleanup; the next kills, as signals do
	// by default.
	context.AfterFunc(ctx, stop)
	code := run(ctx, env{stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}, os.Args[1:])
	stop()
	os.Exit(code)
}

// env is what a command runs with: tests replace the dialer and the sleep.
type env struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	// dial, if set, returns the dialer of endpoint instead of TLS to it.
	dial func(endpoint string) transport.Dialer
	// sleep, if set, waits between Logon attempts.
	sleep func(context.Context, time.Duration) error
}

// usageError is a usage error: exit code 2.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }

// verdictError tells of an assumption contradicted, or a probe
// inconclusive: exit code 3.
type verdictError struct{ msg string }

func (e verdictError) Error() string { return e.msg }

// runError is the failure of a command that ran: exit code 1.
type runError struct{ err error }

func (e runError) Error() string { return e.err.Error() }
func (e runError) Unwrap() error { return e.err }

// ran returns the error of a command that ran: a runError, unless it is a
// usage error or a verdict.
func ran(err error) error {
	if err == nil || errors.As(err, new(usageError)) || errors.As(err, new(verdictError)) {
		return err
	}
	return runError{err}
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

// run runs the command line args, and returns the exit code.
func run(ctx context.Context, e env, args []string) int {
	// A failed write of stdout, cobra's own (help, --version, completion)
	// included, fails the command: exit 1.
	out := &errWriter{w: e.stdout}
	e.stdout = out
	root := newCommand(e)
	// Given nil, cobra would read os.Args.
	root.SetArgs(append([]string{}, args...))
	err := root.ExecuteContext(ctx)
	werr := out.failed()
	if err == nil && werr == nil {
		return 0
	}
	if err != nil {
		fmt.Fprintln(e.stderr, "binancefix:", err)
	}
	// A failed write of stdout, unless it is the error told, is told too.
	if werr != nil && !errors.Is(err, werr) {
		fmt.Fprintln(e.stderr, "binancefix:", werr)
	}
	switch {
	case errors.As(err, new(runError)) || werr != nil:
		return 1
	case errors.As(err, new(verdictError)):
		return 3
	}
	fmt.Fprintln(e.stderr, "Run 'binancefix --help' for usage.")
	return 2
}

// newCommand returns the binancefix command, running with e.
func newCommand(e env) *cobra.Command {
	root := &cobra.Command{
		Use:   "binancefix",
		Short: "Check the binancefix client's assumptions against Binance's Spot testnet",
		Long: `Check, against Binance's Spot testnet, the assumptions the binancefix
client makes where Binance's FIX SBE documentation is silent.

logon logs on to Binance's FIX SBE order entry endpoint (the testnet's
unless --production), shows the LogonAck and logs out; --signature-time
auto tries each form of the signed SendingTime until one is accepted.
probe checks the client's assumptions against the testnet, and only it;
--price-far, the price of its limit orders, is required (below the
market, inside both PERCENT_PRICE_BY_SIDE bands).

The API key is --api-key, --api-key-file (a file holding it) or
BINANCE_API_KEY; its Ed25519 private key the PEM file --key-file or
BINANCE_PRIVATE_KEY names. Order entry needs the FIX_API permission.

The exit code is 0 when all went as the client assumes, 3 when an
assumption is contradicted or a probe inconclusive, 1 on failure, 2 on a
usage error.`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Without a command, nothing asked for was done: a usage error.
		RunE: func(*cobra.Command, []string) error {
			return errors.New("no command given")
		},
	}
	root.SetOut(e.stdout)
	root.SetErr(e.stderr)
	// -v is logon's and probe's --verbose: no shorthand for --version.
	root.Flags().Bool("version", false, "version for binancefix")
	root.AddCommand(logonCommand(e), probeCommand(e), &cobra.Command{
		Use:               "probes",
		Short:             "List the probes and their assumptions",
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		Run:               func(*cobra.Command, []string) { listProbes(e.stdout) },
	}, &cobra.Command{
		Use:               "version",
		Short:             "Print the version",
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		Run:               func(*cobra.Command, []string) { fmt.Fprintln(e.stdout, Version) },
	})
	return root
}

// common are the flags of logon and probe.
type common struct {
	endpoint   string
	production bool
	apiKey     string
	apiKeyFile string
	keyFile    string
	sender     string
	heartbeat  time.Duration
	sigTime    string
	json       bool
	verbose    bool
	timeout    time.Duration
}

func (c *common) register(cmd *cobra.Command, timeout time.Duration) {
	fs := cmd.Flags()
	fs.StringVar(&c.endpoint, "endpoint", "", "order entry endpoint, host:port (default "+binancefix.TestnetEndpoint+", "+binancefix.Endpoint+" with --production)")
	fs.BoolVar(&c.production, "production", false, "allow Binance's production endpoint (logon only: probe refuses it)")
	fs.StringVar(&c.apiKey, "api-key", "", "Ed25519 API key, with the FIX_API permission (default $BINANCE_API_KEY)")
	fs.StringVar(&c.apiKeyFile, "api-key-file", "", "file holding the API key, instead of --api-key")
	fs.StringVar(&c.keyFile, "key-file", "", "PEM file of the API key's Ed25519 private key (default $BINANCE_PRIVATE_KEY)")
	fs.StringVar(&c.sender, "sender", "", "SenderCompID, 1 to 8 letters, digits, - or _ (default random)")
	fs.DurationVar(&c.heartbeat, "heartbeat", probe.DefaultHeartBtInt, "heartbeat interval, whole seconds from 5s to 60s")
	fs.StringVar(&c.sigTime, "signature-time", binancefix.SignatureTimeMicros.String(), "text form of the SendingTime signed: "+strings.Join(forms(), ", "))
	fs.BoolVar(&c.json, "json", false, "write the result as JSON")
	fs.BoolVarP(&c.verbose, "verbose", "v", false, "log the session's events")
	fs.DurationVar(&c.timeout, "timeout", timeout, "time limit of the command, its cleanup apart")
	// --signature-time's, which logon and probe take differently, apart.
	completeFlags(cmd, map[string]cobra.CompletionFunc{
		"endpoint":     cobra.NoFileCompletions,
		"api-key":      cobra.NoFileCompletions,
		"api-key-file": fileCompletion(),
		"key-file":     fileCompletion(),
		"sender":       cobra.NoFileCompletions,
		"heartbeat":    cobra.NoFileCompletions,
		"timeout":      cobra.NoFileCompletions,
	})
}

// forms returns the text forms of the SendingTime signed.
func forms() []string {
	var names []string
	for _, f := range binancefix.SignatureTimes() {
		names = append(names, f.String())
	}
	return names
}

// completeFlags registers the completion of the value of each flag of cmd
// fns names: every flag but a boolean one has one, files only where a file
// is expected.
func completeFlags(cmd *cobra.Command, fns map[string]cobra.CompletionFunc) {
	for name, fn := range fns {
		if err := cmd.RegisterFlagCompletionFunc(name, fn); err != nil {
			panic(err) // a flag misnamed or completed twice
		}
	}
}

// fileCompletion completes files of extensions exts, any file without: keys
// and certificates complete any, their loaders checking what a file holds,
// not its name.
func fileCompletion(exts ...string) cobra.CompletionFunc {
	return func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(exts) == 0 {
			return nil, cobra.ShellCompDirectiveDefault
		}
		return exts, cobra.ShellCompDirectiveFilterFileExt
	}
}

// valueCompletion completes values, never files.
func valueCompletion(values ...string) cobra.CompletionFunc {
	return cobra.FixedCompletions(values, cobra.ShellCompDirectiveNoFileComp)
}

var senderPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,8}$`)

// options checks the common flags of cmd and returns the Options they give,
// the API key and private key read.
func (c *common) options(e env, cmd *cobra.Command) (probe.Options, error) {
	fs := cmd.Flags()
	var errs []error
	if fs.Changed("api-key") && fs.Changed("api-key-file") {
		errs = append(errs, errors.New("--api-key and --api-key-file: one or the other"))
	}
	if !fs.Changed("api-key") && !fs.Changed("api-key-file") {
		c.apiKey = e.getenv("BINANCE_API_KEY")
	}
	if c.keyFile == "" {
		c.keyFile = e.getenv("BINANCE_PRIVATE_KEY")
	}
	if c.apiKey == "" && c.apiKeyFile == "" {
		errs = append(errs, errors.New("no API key: set --api-key, --api-key-file or BINANCE_API_KEY"))
	}
	if c.keyFile == "" {
		errs = append(errs, errors.New("no private key: set --key-file or BINANCE_PRIVATE_KEY to the PEM file of the API key's Ed25519 private key"))
	}
	if c.sender != "" && !senderPattern.MatchString(c.sender) {
		errs = append(errs, fmt.Errorf("--sender %q: 1 to 8 letters, digits, - or _", c.sender))
	}
	if c.heartbeat < binancefix.MinHeartBtInt || c.heartbeat > binancefix.MaxHeartBtInt || c.heartbeat%time.Second != 0 {
		errs = append(errs, fmt.Errorf("--heartbeat %v: whole seconds from 5s to 60s", c.heartbeat))
	}
	if c.timeout <= 0 {
		errs = append(errs, fmt.Errorf("--timeout %v: positive", c.timeout))
	}
	if len(errs) > 0 {
		return probe.Options{}, usageError{errors.Join(errs...)}
	}
	if c.apiKeyFile != "" {
		data, err := os.ReadFile(c.apiKeyFile)
		if err != nil {
			return probe.Options{}, fmt.Errorf("API key: %w", err)
		}
		if c.apiKey = strings.TrimSpace(string(data)); c.apiKey == "" {
			return probe.Options{}, fmt.Errorf("API key: %s is empty", c.apiKeyFile)
		}
	}
	key, err := binancefix.LoadPrivateKey(c.keyFile)
	if err != nil {
		return probe.Options{}, err
	}
	o := probe.Options{Endpoint: c.endpoint, APIKey: c.apiKey, PrivateKey: key, SenderCompID: c.sender, HeartBtInt: c.heartbeat,
		Sleep: e.sleep, Logf: func(format string, args ...any) { fmt.Fprintf(e.stderr, "binancefix: "+format+"\n", args...) }}
	if e.dial != nil {
		o.Dialer = e.dial(c.endpoint)
	}
	if c.verbose {
		o.Logger = slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return o, nil
}

// resolveEndpoint completes --endpoint: the testnet's, or production's
// with --production.
func (c *common) resolveEndpoint() {
	if c.endpoint == "" {
		c.endpoint = binancefix.TestnetEndpoint
		if c.production {
			c.endpoint = binancefix.Endpoint
		}
	}
}

func logonCommand(e env) *cobra.Command {
	var c common
	cmd := &cobra.Command{
		Use:   "logon",
		Short: "Log on, show the LogonAck, log out",
		Long: `Log on to Binance's FIX SBE order entry endpoint, show Binance's LogonAck
and log out (M0, settled: Binance checks the microsecond form, us).

--signature-time chooses the text of the SendingTime the Logon's
signature is made of (us, the client's default, ms, s, ns, unix-us,
unix-ms), or auto to try them in turn, twice HeartBtInt apart, until
Binance accepts one: exit code 0 when it is the default, 3 when another.
It logs on to the testnet unless --production is given.`,
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ran(runLogon(cmd.Context(), e, cmd, &c))
		},
	}
	c.register(cmd, 5*time.Minute)
	completeFlags(cmd, map[string]cobra.CompletionFunc{
		"signature-time": valueCompletion(append(forms(), "auto")...),
	})
	return cmd
}

func runLogon(ctx context.Context, e env, cmd *cobra.Command, c *common) error {
	c.resolveEndpoint()
	if !probe.IsTestnet(c.endpoint) && !c.production {
		return usagef("%s is not the testnet's endpoint, %s: --production to log on to it", c.endpoint, binancefix.TestnetEndpoint)
	}
	auto := strings.EqualFold(c.sigTime, "auto")
	var form binancefix.SignatureTime
	if !auto {
		f, err := binancefix.ParseSignatureTime(c.sigTime)
		if err != nil {
			return usagef("--signature-time: %v, or auto", err)
		}
		form = f
	}
	o, err := c.options(e, cmd)
	if err != nil {
		return err
	}
	o.SignatureTime = form
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if auto {
		res := probe.LogonAuto(ctx, o, binancefix.SignatureTimes())
		if err := writeAuto(e.stdout, res, c.json); err != nil {
			return err
		}
		switch res.Accepted {
		case "":
			return errors.New(res.Error)
		case binancefix.SignatureTimeMicros.String():
			return nil
		}
		return verdictError{fmt.Sprintf("Binance accepted signature time %s, not %s, the client's default", res.Accepted, binancefix.SignatureTimeMicros)}
	}
	lr := probe.Logon(ctx, o)
	if err := writeLogon(e.stdout, lr, c.json); err != nil {
		return err
	}
	switch {
	case lr.Accepted && lr.Error != "":
		return verdictError{"Binance accepted the Logon, the client refused its LogonAck: " + lr.Error}
	case !lr.Accepted:
		return fmt.Errorf("logon failed: %s", lr.Error)
	}
	return nil
}

// probeFlags are the flags of probe alone.
type probeFlags struct {
	symbol, price, qty     string
	interval, wait, settle time.Duration
	list                   bool
}

func probeCommand(e env) *cobra.Command {
	var c common
	var p probeFlags
	cmd := &cobra.Command{
		Use:   "probe [PROBE...]",
		Short: "Run the probes, all if none is named",
		Long: `Run the probes named, by name or letter, all if none is (binancefix probes
lists them): each places small orders below the market on the testnet, and
only there, --production or not, exchanges a few messages and judges an
assumption confirmed, contradicted or inconclusive (all confirmed on
2026-10-17: the probes are the regression check). Whatever it placed is
cancelled when it is done, on error or interrupt too, and it sends at most
one request per --interval.

--price-far, the price of the limit orders, has no default: below the
market, inside both PERCENT_PRICE_BY_SIDE bands of the symbol (70000 for
BTCUSDT at about 81,500); only logon-seqnum, fill and limits run without
it. The exit code is 0 when all are confirmed, 3 when one is not.`,
		Args:              probeNames,
		ValidArgsFunction: completeProbes,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ran(runProbe(cmd.Context(), e, cmd, &c, &p, args))
		},
	}
	c.register(cmd, 5*time.Minute)
	fs := cmd.Flags()
	fs.StringVar(&p.symbol, "symbol", probe.DefaultSymbol, "symbol traded")
	fs.StringVar(&p.price, "price-far", "", "price of the limit orders, required by the probes resting them (order, amend, cancel-replace, list-cancel, mass-cancel): "+
		"below the market, inside both PERCENT_PRICE_BY_SIDE bands, e.g. 70000 for BTCUSDT at about 81,500")
	fs.StringVar(&p.qty, "qty", probe.DefaultQty, "quantity of the orders, small, above the symbol's minimum notional at --price-far")
	fs.DurationVar(&p.interval, "interval", probe.DefaultInterval, "least time between two requests, 10ms at least")
	fs.DurationVar(&p.wait, "wait", probe.DefaultWait, "how long an answer is waited for")
	fs.DurationVar(&p.settle, "settle", probe.DefaultSettle, "how long late messages are waited for once a request is answered")
	fs.BoolVar(&p.list, "list", false, "list the probes and exit")
	completeFlags(cmd, map[string]cobra.CompletionFunc{
		"signature-time": valueCompletion(forms()...),
		"symbol":         cobra.NoFileCompletions,
		"price-far":      cobra.NoFileCompletions,
		"qty":            cobra.NoFileCompletions,
		"interval":       cobra.NoFileCompletions,
		"wait":           cobra.NoFileCompletions,
		"settle":         cobra.NoFileCompletions,
	})
	return cmd
}

// completeProbes completes probe's arguments: the names of the probes not
// given yet, by name or letter.
func completeProbes(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	var names []cobra.Completion
	for _, p := range probe.Probes() {
		if !slices.ContainsFunc(args, func(n string) bool { return strings.EqualFold(n, p.Name) || strings.EqualFold(n, p.Letter) }) {
			names = append(names, cobra.CompletionWithDesc(p.Name, "("+p.Letter+")"))
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// probeNames checks the arguments of probe: names or letters of probes.
func probeNames(_ *cobra.Command, args []string) error {
	var unknown []string
	for _, n := range args {
		if !slices.ContainsFunc(probe.Probes(), func(p probe.Probe) bool { return strings.EqualFold(n, p.Name) || strings.EqualFold(n, p.Letter) }) {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("unknown probe %s: run binancefix probes for the list", strings.Join(unknown, ", "))
	}
	return nil
}

func runProbe(ctx context.Context, e env, cmd *cobra.Command, c *common, p *probeFlags, args []string) error {
	if p.list {
		listProbes(e.stdout)
		return nil
	}
	c.resolveEndpoint()
	if c.production || !probe.IsTestnet(c.endpoint) {
		return usagef("probe runs against the Spot testnet only, %s: not %s", binancefix.TestnetEndpoint, c.endpoint)
	}
	form, err := binancefix.ParseSignatureTime(c.sigTime)
	if err != nil {
		return usagef("--signature-time: %v (probe takes one form: settle it with logon --signature-time auto)", err)
	}
	if p.price == "" && probe.PriceRequired(args...) {
		return usagef("--price-far is required by the probes resting limit orders (order, amend, cancel-replace, list-cancel, mass-cancel; "+
			"logon-seqnum, fill and limits run without it): %s", probe.PriceFarHelp)
	}
	if p.interval < 10*time.Millisecond {
		return usagef("--interval %v: 10ms at least", p.interval)
	}
	if p.wait <= 0 || p.settle <= 0 {
		return usagef("--wait and --settle: positive")
	}
	o, err := c.options(e, cmd)
	if err != nil {
		return err
	}
	o.SignatureTime, o.Symbol, o.PriceFar, o.Qty = form, strings.ToUpper(p.symbol), p.price, p.qty
	o.Interval, o.Wait, o.Settle = p.interval, p.wait, p.settle
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	rep, err := probe.Run(ctx, o, args...)
	if werr := writeReport(e.stdout, rep, c.json); werr != nil {
		return werr
	}
	if err != nil {
		if errors.Is(err, probe.ErrNotTestnet) {
			return usageError{err}
		}
		return err
	}
	n := 0
	for _, r := range rep.Results {
		if r.Verdict != probe.Confirmed {
			n++
		}
	}
	if n > 0 {
		return verdictError{fmt.Sprintf("%d of %d probes not confirmed", n, len(rep.Results))}
	}
	return nil
}

func listProbes(w io.Writer) {
	for _, p := range probe.Probes() {
		fmt.Fprintf(w, "(%s) %s\n    %s\n", p.Letter, p.Name, p.Assumption)
	}
}
