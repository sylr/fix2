// Command binancemd streams Binance Spot market data over the Binance FIX
// API: trades, best bid and offer, order book depth, instruments and limits.
//
// Usage:
//
//	binancemd [flags] <command> [flags] [SYMBOL...]
//
// Commands:
//
//	trades SYMBOL...       trades, as they happen
//	ticker SYMBOL...       best bid and offer, as they change
//	depth SYMBOL...        order book snapshot, then its changes every 100ms
//	book SYMBOL...         order book kept up to date, its best levels shown
//	instruments [SYMBOL...] instruments' trading rules, all if none is given
//	limits                 message and subscription limits of the connection
//
// The FIX API authenticates with an Ed25519 API key: -api-key (or
// BINANCE_API_KEY) is the key, -key (or BINANCE_PRIVATE_KEY) the PEM file
// of its private key. Market data needs the FIX_API or FIX_API_READ_ONLY
// permission.
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
)

const (
	prodEndpoint    = "fix-md.binance.com:9000"
	testnetEndpoint = "fix-md.testnet.binance.vision:9000"
)

// options are the flags of every command.
type options struct {
	apiKey    string
	keyFile   string
	sender    string
	endpoint  string
	testnet   bool
	heartbeat time.Duration
	json      bool
	validate  bool
	verbose   bool
	trace     bool
	// Command flags.
	levels int // depth and book: levels of the snapshot
	show   int // book: levels shown
}

func (o *options) global(fs *flag.FlagSet) {
	fs.StringVar(&o.apiKey, "api-key", o.apiKey, "API key (default $BINANCE_API_KEY)")
	fs.StringVar(&o.keyFile, "key", o.keyFile, "PEM file of the API key's Ed25519 private key (default $BINANCE_PRIVATE_KEY)")
	fs.StringVar(&o.sender, "sender", o.sender, "SenderCompID, 1 to 8 letters, digits, - or _ (default random)")
	fs.StringVar(&o.endpoint, "endpoint", o.endpoint, "FIX market data endpoint, host:port (default "+prodEndpoint+")")
	fs.BoolVar(&o.testnet, "testnet", o.testnet, "use the Spot testnet, "+testnetEndpoint)
	fs.DurationVar(&o.heartbeat, "heartbeat", o.heartbeat, "heartbeat interval, 5s to 60s")
	fs.BoolVar(&o.json, "json", o.json, "write JSON lines")
	fs.BoolVar(&o.validate, "validate", o.validate, "validate received messages against Binance's dictionary: invalid ones are rejected, not shown")
	fs.BoolVar(&o.verbose, "v", o.verbose, "log the session's events in detail")
	fs.BoolVar(&o.trace, "trace", o.trace, "log every FIX message, credentials redacted")
}

const usage = `Usage: binancemd [flags] <command> [flags] [SYMBOL...]

Streams Binance Spot market data over the FIX API.

Commands:
  trades SYMBOL...         trades, as they happen
  ticker SYMBOL...         best bid and offer, as they change
  depth [-levels N] SYMBOL...
                           order book snapshot of N levels, then its changes every 100ms
  book [-levels N] [-show K] SYMBOL...
                           order book kept up to date from a snapshot of N levels,
                           its K best levels shown on every change
  instruments [SYMBOL...]  instruments' trading rules, all if none is given
  limits                   message and subscription limits of the connection

Flags (before or after the command):
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run runs the command line args and returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	o := &options{heartbeat: 30 * time.Second, levels: 100, show: 10}
	root := flag.NewFlagSet("binancemd", flag.ContinueOnError)
	root.SetOutput(stderr)
	o.global(root)
	root.Usage = func() {
		fmt.Fprint(stderr, usage)
		root.PrintDefaults()
	}
	if err := root.Parse(args); err != nil {
		return 2
	}
	if root.NArg() == 0 {
		root.Usage()
		return 2
	}
	name := root.Arg(0)
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(stderr, "binancemd: unknown command %q\n\n", name)
		root.Usage()
		return 2
	}
	fs := flag.NewFlagSet("binancemd "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	o.global(fs)
	if cmd.kind == kindDepth || cmd.kind == kindBook {
		fs.IntVar(&o.levels, "levels", o.levels, "levels of each side of the initial snapshot, 2 to 5000")
	}
	if cmd.kind == kindBook {
		fs.IntVar(&o.show, "show", o.show, "levels of each side shown")
	}
	fs.Usage = root.Usage
	if err := fs.Parse(root.Args()[1:]); err != nil {
		return 2
	}
	symbols := make([]string, fs.NArg())
	for i, s := range fs.Args() {
		symbols[i] = strings.ToUpper(s)
	}
	if err := o.check(cmd, symbols); err != nil {
		fmt.Fprintf(stderr, "binancemd: %v\n", err)
		return 2
	}

	level := slog.LevelInfo
	if o.verbose || o.trace {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := stream(ctx, o, cmd, symbols, newOutput(stdout, o.json, isTerminal(stdout)), logger)
	if err != nil {
		fmt.Fprintf(stderr, "binancemd: %v\n", err)
		return 1
	}
	return 0
}

// check completes o from the environment and checks it for cmd.
func (o *options) check(cmd command, symbols []string) error {
	if o.apiKey == "" {
		o.apiKey = os.Getenv("BINANCE_API_KEY")
	}
	if o.keyFile == "" {
		o.keyFile = os.Getenv("BINANCE_PRIVATE_KEY")
	}
	if o.endpoint == "" {
		o.endpoint = prodEndpoint
		if o.testnet {
			o.endpoint = testnetEndpoint
		}
	}
	var errs []error
	if o.apiKey == "" {
		errs = append(errs, errors.New("no API key: set -api-key or BINANCE_API_KEY"))
	}
	if o.keyFile == "" {
		errs = append(errs, errors.New("no private key: set -key or BINANCE_PRIVATE_KEY to the PEM file of the API key's Ed25519 private key"))
	}
	if o.sender != "" && !validID(o.sender, 8) {
		errs = append(errs, fmt.Errorf("-sender %q: 1 to 8 letters, digits, - or _", o.sender))
	}
	if o.heartbeat < 5*time.Second || o.heartbeat > 60*time.Second || o.heartbeat%time.Second != 0 {
		errs = append(errs, fmt.Errorf("-heartbeat %v: whole seconds from 5s to 60s", o.heartbeat))
	}
	if o.levels < 2 || o.levels > 5000 {
		errs = append(errs, fmt.Errorf("-levels %d: 2 to 5000", o.levels))
	}
	if o.show < 1 {
		errs = append(errs, fmt.Errorf("-show %d: at least 1", o.show))
	}
	switch {
	case cmd.stream() && len(symbols) == 0:
		errs = append(errs, errors.New("no symbol given"))
	case cmd.kind == kindLimits && len(symbols) > 0:
		errs = append(errs, errors.New("limits takes no symbol"))
	}
	for _, s := range symbols {
		// Symbols end up in request IDs, of at most 36 characters.
		if !validID(s, 20) {
			errs = append(errs, fmt.Errorf("symbol %q: 1 to 20 letters, digits, - or _", s))
		}
	}
	return errors.Join(errs...)
}

// validID reports whether id has 1 to n letters, digits, - or _, as Binance
// requires of CompIDs and request IDs.
func validID(id string, n int) bool {
	if len(id) == 0 || len(id) > n {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
