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
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

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

const usage = `usage: binancefix logon [flags]
       binancefix probe [flags] [PROBE...]
       binancefix probes
       binancefix version

logon logs on to Binance's FIX SBE order entry endpoint (the testnet's
unless --production), shows the LogonAck and logs out; --signature-time
auto tries each form of the signed SendingTime until one is accepted.
probe checks the client's assumptions against the testnet, and only it;
--price-far, the price of its limit orders, is required (below the
market, inside both PERCENT_PRICE_BY_SIDE bands).
Run "binancefix logon -h" or "binancefix probe -h" for the flags.
`

// usageError is a usage error: exit code 2.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }

// verdictError tells of an assumption contradicted, or a probe
// inconclusive: exit code 3.
type verdictError struct{ msg string }

func (e verdictError) Error() string { return e.msg }

// run runs the command line args, and returns the exit code.
func run(ctx context.Context, e env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "logon":
		err = runLogon(ctx, e, args[1:])
	case "probe":
		err = runProbe(ctx, e, args[1:])
	case "probes":
		listProbes(e.stdout)
	case "version":
		fmt.Fprintln(e.stdout, Version)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(e.stdout, usage)
	default:
		fmt.Fprint(e.stderr, usage)
		return 2
	}
	var ve verdictError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &ve):
		fmt.Fprintln(e.stderr, "binancefix:", err)
		return 3
	case errors.As(err, new(usageError)):
		fmt.Fprintln(e.stderr, "binancefix:", err)
		return 2
	default:
		fmt.Fprintln(e.stderr, "binancefix:", err)
		return 1
	}
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

func (c *common) register(fs *flag.FlagSet, timeout time.Duration) {
	fs.StringVar(&c.endpoint, "endpoint", "", "order entry endpoint, host:port (default "+binancefix.TestnetEndpoint+", "+binancefix.Endpoint+" with --production)")
	fs.BoolVar(&c.production, "production", false, "allow Binance's production endpoint (logon only: probe refuses it)")
	fs.StringVar(&c.apiKey, "api-key", "", "Ed25519 API key, with the FIX_API permission (default $BINANCE_API_KEY)")
	fs.StringVar(&c.apiKeyFile, "api-key-file", "", "file holding the API key, instead of --api-key")
	fs.StringVar(&c.keyFile, "key-file", "", "PEM file of the API key's Ed25519 private key (default $BINANCE_PRIVATE_KEY)")
	fs.StringVar(&c.sender, "sender", "", "SenderCompID, 1 to 8 letters, digits, - or _ (default random)")
	fs.DurationVar(&c.heartbeat, "heartbeat", probe.DefaultHeartBtInt, "heartbeat interval, whole seconds from 5s to 60s")
	fs.StringVar(&c.sigTime, "signature-time", binancefix.SignatureTimeMicros.String(), "text form of the SendingTime signed: "+forms())
	fs.BoolVar(&c.json, "json", false, "write the result as JSON")
	fs.BoolVar(&c.verbose, "v", false, "log the session's events")
	fs.DurationVar(&c.timeout, "timeout", timeout, "time limit of the command, its cleanup apart")
}

func forms() string {
	var names []string
	for _, f := range binancefix.SignatureTimes() {
		names = append(names, f.String())
	}
	return strings.Join(names, ", ")
}

var senderPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,8}$`)

// options checks the common flags of fs and returns the Options they give,
// the API key and private key read.
func (c *common) options(e env, fs *flag.FlagSet) (probe.Options, error) {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var errs []error
	if set["api-key"] && set["api-key-file"] {
		errs = append(errs, errors.New("--api-key and --api-key-file: one or the other"))
	}
	if !set["api-key"] && !set["api-key-file"] {
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

func runLogon(ctx context.Context, e env, args []string) error {
	fs := flag.NewFlagSet("binancefix logon", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	var c common
	c.register(fs, 5*time.Minute)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() > 0 {
		return usagef("logon takes no argument: %q", fs.Args())
	}
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
	o, err := c.options(e, fs)
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

func runProbe(ctx context.Context, e env, args []string) error {
	fs := flag.NewFlagSet("binancefix probe", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	var c common
	c.register(fs, 5*time.Minute)
	symbol := fs.String("symbol", probe.DefaultSymbol, "symbol traded")
	price := fs.String("price-far", "", "price of the limit orders, required by the probes resting them (order, amend, cancel-replace, list-cancel, mass-cancel): "+
		"below the market, inside both PERCENT_PRICE_BY_SIDE bands, e.g. 70000 for BTCUSDT at about 81,500")
	qty := fs.String("qty", probe.DefaultQty, "quantity of the orders, small, above the symbol's minimum notional at --price-far")
	interval := fs.Duration("interval", probe.DefaultInterval, "least time between two requests, 10ms at least")
	wait := fs.Duration("wait", probe.DefaultWait, "how long an answer is waited for")
	settle := fs.Duration("settle", probe.DefaultSettle, "how long late messages are waited for once a request is answered")
	list := fs.Bool("list", false, "list the probes and exit")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if *list {
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
	var unknown []string
	for _, n := range fs.Args() {
		if !slices.ContainsFunc(probe.Probes(), func(p probe.Probe) bool { return strings.EqualFold(n, p.Name) || strings.EqualFold(n, p.Letter) }) {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		return usagef("unknown probe %s: run binancefix probes for the list", strings.Join(unknown, ", "))
	}
	if *price == "" && probe.PriceRequired(fs.Args()...) {
		return usagef("--price-far is required by the probes resting limit orders (order, amend, cancel-replace, list-cancel, mass-cancel; "+
			"logon-seqnum, fill and limits run without it): %s", probe.PriceFarHelp)
	}
	if *interval < 10*time.Millisecond {
		return usagef("--interval %v: 10ms at least", *interval)
	}
	if *wait <= 0 || *settle <= 0 {
		return usagef("--wait and --settle: positive")
	}
	o, err := c.options(e, fs)
	if err != nil {
		return err
	}
	o.SignatureTime, o.Symbol, o.PriceFar, o.Qty = form, strings.ToUpper(*symbol), *price, *qty
	o.Interval, o.Wait, o.Settle = *interval, *wait, *settle
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	rep, err := probe.Run(ctx, o, fs.Args()...)
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
