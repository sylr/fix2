// Command binancemd streams Binance Spot market data over the Binance FIX
// API: trades, best bid and offer, order book depth, instruments and limits.
//
// Usage:
//
//	binancemd <command> [flags] [SYMBOL...]
//
// Commands:
//
//	trades SYMBOL...        trades, as they happen
//	ticker SYMBOL...        best bid and offer, as they change
//	depth SYMBOL...         order book snapshot, then its changes every 100ms
//	book SYMBOL...          order book kept up to date, its best levels shown
//	instruments [SYMBOL...] instruments' trading rules, all if none is given
//	limits                  message and subscription limits of the connection
//
// The FIX API authenticates with an Ed25519 API key: --api-key,
// --api-key-file (a file holding it) or BINANCE_API_KEY is the key,
// --key-file (or BINANCE_PRIVATE_KEY) the PEM file of its private key.
// Market data needs the FIX_API or FIX_API_READ_ONLY permission.
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
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

// Version is the version of binancemd, set when built by the Makefile.
var Version = "dev"

const (
	prodEndpoint    = "fix-md.binance.com:9000"
	testnetEndpoint = "fix-md.testnet.binance.vision:9000"
)

// options are the flags of every command.
type options struct {
	apiKey     string
	apiKeyFile string
	keyFile    string
	sender     string
	endpoint   string
	testnet    bool
	heartbeat  time.Duration
	json       bool
	validate   bool
	verbose    bool
	trace      bool
	// Command flags.
	levels int // depth and book: levels of the snapshot
	show   int // book: levels shown
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// runError is an error of a command that ran, as opposed to a usage error.
type runError struct{ err error }

func (e runError) Error() string { return e.err.Error() }
func (e runError) Unwrap() error { return e.err }

// run runs the command line args and returns the exit code: 2 for a usage
// error, 1 for a command that failed.
func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signalContext()
	defer stop()
	root := newCommand(stdout, stderr)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "binancemd: %v\n", err)
	if errors.As(err, new(runError)) {
		return 1
	}
	fmt.Fprintf(stderr, "Run 'binancemd --help' for usage.\n")
	return 2
}

// signalContext returns a context done on the first SIGINT or SIGTERM, which
// starts the shutdown. A second one terminates the process, as signals do by
// default: the shutdown may wait for a blocked output.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

// newCommand returns the binancemd command, writing data to stdout and logs
// to stderr.
func newCommand(stdout, stderr io.Writer) *cobra.Command {
	o := &options{heartbeat: 30 * time.Second, levels: 100, show: 10}
	root := &cobra.Command{
		Use:   "binancemd",
		Short: "Stream Binance Spot market data over the FIX API",
		Long: `Stream Binance Spot market data over the Binance FIX API.

The FIX API authenticates with an Ed25519 API key: --api-key,
--api-key-file (a file holding it) or BINANCE_API_KEY is the key,
--key-file (or BINANCE_PRIVATE_KEY) the PEM file of its private key.
Market data needs the FIX_API or FIX_API_READ_ONLY permission.`,
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
	pf := root.PersistentFlags()
	pf.StringVar(&o.apiKey, "api-key", "", "API key (default $BINANCE_API_KEY)")
	pf.StringVar(&o.apiKeyFile, "api-key-file", "", "file holding the API key, instead of --api-key")
	pf.StringVar(&o.keyFile, "key-file", "", "PEM file of the API key's Ed25519 private key (default $BINANCE_PRIVATE_KEY)")
	// Given at all, even empty: one or the other.
	root.MarkFlagsMutuallyExclusive("api-key", "api-key-file")
	pf.StringVar(&o.sender, "sender", "", "SenderCompID, 1 to 8 letters, digits, - or _ (default random)")
	pf.StringVar(&o.endpoint, "endpoint", "", "FIX market data endpoint, host:port (default "+prodEndpoint+")")
	pf.BoolVar(&o.testnet, "testnet", false, "use the Spot testnet, "+testnetEndpoint)
	pf.DurationVar(&o.heartbeat, "heartbeat", o.heartbeat, "heartbeat interval, 5s to 60s")
	pf.BoolVar(&o.json, "json", false, "write JSON lines")
	pf.BoolVar(&o.validate, "validate", false, "validate received messages against Binance's dictionary: invalid ones are rejected, not shown")
	pf.BoolVarP(&o.verbose, "verbose", "v", false, "log the session's events in detail")
	pf.BoolVar(&o.trace, "trace", false, "log every FIX message, field by field, credentials redacted")

	add := func(use, short string, k kind, args cobra.PositionalArgs) *cobra.Command {
		c := &cobra.Command{
			Use:   use,
			Short: short,
			Args:  args,
			RunE: func(c *cobra.Command, args []string) error {
				return execute(c.Context(), o, command{k}, args, stdout, stderr)
			},
		}
		root.AddCommand(c)
		return c
	}
	symbols := cobra.MinimumNArgs(1)
	add("trades SYMBOL...", "Trades, as they happen", kindTrades, symbols)
	add("ticker SYMBOL...", "Best bid and offer, as they change", kindTicker, symbols)
	depth := add("depth SYMBOL...", "Order book snapshot, then its changes every 100ms", kindDepth, symbols)
	depth.Flags().IntVar(&o.levels, "levels", o.levels, "levels of each side of the initial snapshot, 2 to 5000")
	book := add("book SYMBOL...", "Order book kept up to date, its best levels shown on every change", kindBook, symbols)
	book.Flags().IntVar(&o.levels, "levels", o.levels, "levels of each side of the initial snapshot, 2 to 5000")
	book.Flags().IntVar(&o.show, "show", o.show, "levels of each side shown")
	add("instruments [SYMBOL...]", "Instruments' trading rules, all if no symbol is given", kindInstruments, cobra.ArbitraryArgs)
	add("limits", "Message and subscription limits of the connection", kindLimits, cobra.NoArgs)
	return root
}

// execute runs command cmd for symbols.
func execute(ctx context.Context, o *options, cmd command, args []string, stdout, stderr io.Writer) error {
	symbols := make([]string, len(args))
	for i, s := range args {
		symbols[i] = strings.ToUpper(s)
	}
	if err := o.check(cmd, symbols); err != nil {
		return err
	}
	if err := o.readAPIKey(); err != nil {
		return runError{err}
	}
	log := newLogger(stderr, o.verbose || o.trace)
	if err := stream(ctx, o, cmd, symbols, newOutput(stdout, o.json, isTerminal(stdout)), log); err != nil {
		return runError{err}
	}
	return nil
}

// newLogger returns a logger writing to w with zerolog's console writer, in
// colour on a terminal.
func newLogger(w io.Writer, debug bool) *slog.Logger {
	level := zerolog.InfoLevel
	if debug {
		level = zerolog.DebugLevel
	}
	// Times keep their milliseconds, durations read as Go writes them.
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.DurationFieldFormat = zerolog.DurationFormatString
	cw := zerolog.ConsoleWriter{Out: w, TimeFormat: "15:04:05.000", NoColor: !isTerminal(w)}
	return slog.New(zerolog.NewSlogHandler(zerolog.New(cw).Level(level)))
}

// check completes o from the environment and checks it for cmd.
func (o *options) check(cmd command, symbols []string) error {
	if o.apiKey == "" && o.apiKeyFile == "" {
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
	if o.apiKey == "" && o.apiKeyFile == "" {
		errs = append(errs, errors.New("no API key: set --api-key, --api-key-file or BINANCE_API_KEY"))
	}
	if o.keyFile == "" {
		errs = append(errs, errors.New("no private key: set --key-file or BINANCE_PRIVATE_KEY to the PEM file of the API key's Ed25519 private key"))
	}
	if o.sender != "" && !validID(o.sender, 8) {
		errs = append(errs, fmt.Errorf("--sender %q: 1 to 8 letters, digits, - or _", o.sender))
	}
	if o.heartbeat < 5*time.Second || o.heartbeat > 60*time.Second || o.heartbeat%time.Second != 0 {
		errs = append(errs, fmt.Errorf("--heartbeat %v: whole seconds from 5s to 60s", o.heartbeat))
	}
	if o.levels < 2 || o.levels > 5000 {
		errs = append(errs, fmt.Errorf("--levels %d: 2 to 5000", o.levels))
	}
	if o.show < 1 {
		errs = append(errs, fmt.Errorf("--show %d: at least 1", o.show))
	}
	for _, s := range symbols {
		// Symbols end up in request IDs, of at most 36 characters.
		if !validID(s, 20) {
			errs = append(errs, fmt.Errorf("symbol %q: 1 to 20 letters, digits, - or _", s))
		}
	}
	return errors.Join(errs...)
}

// readAPIKey reads the API key of --api-key-file, if given: the file's
// content, without surrounding white space.
func (o *options) readAPIKey() error {
	if o.apiKeyFile == "" {
		return nil
	}
	data, err := os.ReadFile(o.apiKeyFile)
	if err != nil {
		return fmt.Errorf("API key: %w", err)
	}
	o.apiKey = strings.TrimSpace(string(data))
	if o.apiKey == "" {
		return fmt.Errorf("API key: %s is empty", o.apiKeyFile)
	}
	return nil
}

// validID reports whether id has 1 to n letters, digits, - or _, as Binance
// requires of CompIDs and request IDs.
func validID(id string, n int) bool {
	if len(id) == 0 || len(id) > n {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
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
