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
//	depth SYMBOL...         order book snapshot, then its changes every 100ms (20ms with --sbe)
//	book SYMBOL...          order book kept up to date, its best levels shown
//	instruments [SYMBOL...] instruments' trading rules, all if none is given
//	limits                  message and subscription limits of the connection
//
// The FIX API authenticates with an Ed25519 API key: --api-key,
// --api-key-file (a file holding it) or BINANCE_API_KEY is the key,
// --key-file (or BINANCE_PRIVATE_KEY) the PEM file of its private key.
// Market data needs the FIX_API or FIX_API_READ_ONLY permission.
//
// # FIX SBE
//
// --sbe uses Binance's FIX SBE API instead, through binancefix's market
// data sessions: SBE messages over TLS on port 9002 of fix-md.binance.com
// (fix-md.testnet.binance.vision with --testnet), the same key, the same
// commands and the same events, written by the same output. Where SBE
// differs from FIX:
//
//   - depth updates come every 20ms, not 100ms, and carry no update action:
//     the depth command keeps a book from the snapshot and infers each, as
//     Binance's rule says (a missing size deletes the level; a size changes
//     it if the book has it, adds it if not). It shows the updates that
//     follow the snapshot, not those it covers;
//   - a book (depth or book command) whose updates have a gap, or whose
//     snapshot has no LastBookUpdateID, cannot be subscribed again on its
//     connection, binancefix's rule (an old subscription's data could not
//     be told from a new one's): binancemd reconnects, and subscribes to
//     every symbol again once logged on, each getting a new snapshot;
//   - --validate, the FIX dictionary's check, does not apply: binancefix
//     checks every SBE message before binancemd sees it, a malformed one
//     ending the connection, which binancemd then opens again;
//   - --trace logs every SBE message, decoded field by field, the Logon's
//     API key and signature redacted.
//
// Still to check on the testnet (MD3): which streams get a snapshot on
// activation (book streams do, as documented; trades are not expected
// to), the form of an unsubscribe Binance takes, the 1000 streams per
// connection, and the actions inferred side by side with the FIX path's.
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

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"github.com/sylr/fix2/contribs/binancefix"
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
	sbe        bool // FIX SBE, through binancefix
	// Command flags.
	levels int // depth and book: levels of the snapshot
	show   int // book: levels shown
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
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

// runError is an error of a command that ran, as opposed to a usage error.
type runError struct{ err error }

func (e runError) Error() string { return e.err.Error() }
func (e runError) Unwrap() error { return e.err }

// run runs the command line args and returns the exit code: 2 for a usage
// error, 1 for a command that failed.
func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signalContext()
	defer stop()
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
		fmt.Fprintf(stderr, "binancemd: %v\n", err)
	}
	// A failed write of stdout, unless it is the error told, is told too.
	if werr != nil && !errors.Is(err, werr) {
		fmt.Fprintf(stderr, "binancemd: %v\n", werr)
	}
	if errors.As(err, new(runError)) || werr != nil {
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
Market data needs the FIX_API or FIX_API_READ_ONLY permission.

--sbe uses Binance's FIX SBE API instead (port 9002), with the same key,
commands and output: depth updates every 20ms, their actions inferred from
the book kept since the snapshot; a book that loses updates makes the
command reconnect and subscribe again, a stream never being subscribed
twice on an SBE connection.`,
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
	pf.StringVar(&o.endpoint, "endpoint", "", "market data endpoint, host:port (default "+prodEndpoint+", with --sbe "+binancefix.MarketDataEndpoint+")")
	pf.BoolVar(&o.testnet, "testnet", false, "use the Spot testnet, "+testnetEndpoint+" ("+binancefix.TestnetMarketDataEndpoint+" with --sbe)")
	pf.DurationVar(&o.heartbeat, "heartbeat", o.heartbeat, "heartbeat interval, 5s to 60s")
	pf.BoolVar(&o.json, "json", false, "write JSON lines")
	pf.BoolVar(&o.validate, "validate", false, "validate received messages against Binance's FIX dictionary: invalid ones are rejected, not shown (not with --sbe)")
	pf.BoolVarP(&o.verbose, "verbose", "v", false, "log the session's events in detail")
	pf.BoolVar(&o.trace, "trace", false, "log every FIX (or SBE) message, field by field, credentials redacted")
	pf.BoolVar(&o.sbe, "sbe", false, "use Binance's FIX SBE API (binancefix), port 9002, instead of FIX")
	completeFlags(root, map[string]cobra.CompletionFunc{
		"api-key":      cobra.NoFileCompletions,
		"api-key-file": fileCompletion(),
		"key-file":     fileCompletion(),
		"sender":       cobra.NoFileCompletions,
		"endpoint":     cobra.NoFileCompletions,
		"heartbeat":    cobra.NoFileCompletions,
	})

	add := func(use, short string, k kind, args cobra.PositionalArgs) *cobra.Command {
		c := &cobra.Command{
			Use:   use,
			Short: short,
			Args:  args,
			// Symbols, if any: never files, and not looked up.
			ValidArgsFunction: cobra.NoFileCompletions,
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
	depth := add("depth SYMBOL...", "Order book snapshot, then its changes every 100ms (20ms with --sbe)", kindDepth, symbols)
	depth.Flags().IntVar(&o.levels, "levels", o.levels, "levels of each side of the initial snapshot, 2 to 5000")
	completeFlags(depth, map[string]cobra.CompletionFunc{"levels": cobra.NoFileCompletions})
	book := add("book SYMBOL...", "Order book kept up to date, its best levels shown on every change", kindBook, symbols)
	book.Flags().IntVar(&o.levels, "levels", o.levels, "levels of each side of the initial snapshot, 2 to 5000")
	book.Flags().IntVar(&o.show, "show", o.show, "levels of each side shown")
	completeFlags(book, map[string]cobra.CompletionFunc{"levels": cobra.NoFileCompletions, "show": cobra.NoFileCompletions})
	add("instruments [SYMBOL...]", "Instruments' trading rules, all if no symbol is given", kindInstruments, cobra.ArbitraryArgs)
	add("limits", "Message and subscription limits of the connection", kindLimits, cobra.NoArgs)
	return root
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
		switch {
		case o.sbe && o.testnet:
			o.endpoint = binancefix.TestnetMarketDataEndpoint
		case o.sbe:
			o.endpoint = binancefix.MarketDataEndpoint
		case o.testnet:
			o.endpoint = testnetEndpoint
		default:
			o.endpoint = prodEndpoint
		}
	}
	var errs []error
	if o.sbe && o.validate {
		errs = append(errs, errors.New("--validate checks FIX messages against Binance's FIX dictionary: not with --sbe, whose messages binancefix checks"))
	}
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

// isTerminal reports whether w, or the writer of an errWriter, is a
// terminal.
func isTerminal(w io.Writer) bool {
	if ew, ok := w.(*errWriter); ok {
		w = ew.w
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
