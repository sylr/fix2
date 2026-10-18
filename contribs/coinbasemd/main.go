// Command coinbasemd streams Coinbase Exchange market data over the
// Coinbase Exchange FIX API: trades, best bid and offer, order book depth
// and instruments.
//
// Usage:
//
//	coinbasemd <command> [flags] [SYMBOL...]
//
// Commands:
//
//	trades SYMBOL...        trades, as they happen
//	ticker SYMBOL...        best bid and offer, as they change
//	depth SYMBOL...         the 10 best levels of each side, then their changes
//	book SYMBOL...          the 10 best levels of each side kept up to date, shown on every change
//	instruments [SYMBOL...] instruments' trading rules, all if none is given
//
// The FIX API authenticates with an Exchange API key: its key, its secret
// and its passphrase, each given by a flag, a file flag or an environment
// variable (COINBASE_API_KEY, COINBASE_API_SECRET, COINBASE_API_PASSPHRASE).
// Keys of the public sandbox work with --sandbox.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

// Version is the version of coinbasemd, set when built by the Makefile.
var Version = "dev"

const (
	prodEndpoint    = "fix-md.exchange.coinbase.com:6121"
	sandboxEndpoint = "fix-md.sandbox.exchange.coinbase.com:6121"
)

// secret is a credential given by a flag, a file or the environment.
type secret struct {
	name  string // its flag, "api-key" for one
	env   string
	value string
	file  string
}

// resolve completes s from the environment, if neither of its flags is
// given.
func (s *secret) resolve() {
	if s.value == "" && s.file == "" {
		s.value = os.Getenv(s.env)
	}
}

// read reads s's file, if given: its content, without surrounding white
// space.
func (s *secret) read() error {
	if s.file == "" {
		return nil
	}
	data, err := os.ReadFile(s.file)
	if err != nil {
		return fmt.Errorf("--%s-file: %w", s.name, err)
	}
	s.value = strings.TrimSpace(string(data))
	if s.value == "" {
		return fmt.Errorf("--%s-file: %s is empty", s.name, s.file)
	}
	return nil
}

// options are the flags of every command.
type options struct {
	apiKey, apiSecret, passphrase secret
	endpoint                      string
	sandbox                       bool
	caFile                        string
	heartbeat                     time.Duration
	json                          bool
	validate                      bool
	verbose                       bool
	trace                         bool
	show                          int // book: levels shown
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
		fmt.Fprintf(stderr, "coinbasemd: %v\n", err)
	}
	// A failed write of stdout, unless it is the error told, is told too.
	if werr != nil && !errors.Is(err, werr) {
		fmt.Fprintf(stderr, "coinbasemd: %v\n", werr)
	}
	if errors.As(err, new(runError)) || werr != nil {
		return 1
	}
	fmt.Fprintf(stderr, "Run 'coinbasemd --help' for usage.\n")
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

// newCommand returns the coinbasemd command, writing data to stdout and
// logs to stderr.
func newCommand(stdout, stderr io.Writer) *cobra.Command {
	o := &options{
		apiKey:     secret{name: "api-key", env: "COINBASE_API_KEY"},
		apiSecret:  secret{name: "api-secret", env: "COINBASE_API_SECRET"},
		passphrase: secret{name: "passphrase", env: "COINBASE_API_PASSPHRASE"},
		heartbeat:  30 * time.Second,
		show:       10,
	}
	root := &cobra.Command{
		Use:   "coinbasemd",
		Short: "Stream Coinbase Exchange market data over the FIX API",
		Long: `Stream Coinbase Exchange market data over the Coinbase Exchange FIX API.

The FIX API authenticates with an Exchange API key: its key, its secret and
its passphrase, each given by a flag, a file flag or an environment variable
(COINBASE_API_KEY, COINBASE_API_SECRET, COINBASE_API_PASSPHRASE). Keys of
the public sandbox work with --sandbox.`,
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
	for _, s := range []*secret{&o.apiKey, &o.apiSecret, &o.passphrase} {
		what := map[string]string{"api-key": "API key", "api-secret": "API secret, base64", "passphrase": "API passphrase"}[s.name]
		pf.StringVar(&s.value, s.name, "", what+" (default $"+s.env+")")
		pf.StringVar(&s.file, s.name+"-file", "", "file holding the "+what+", instead of --"+s.name)
		// Given at all, even empty: one or the other.
		root.MarkFlagsMutuallyExclusive(s.name, s.name+"-file")
	}
	pf.StringVar(&o.endpoint, "endpoint", "", "FIX market data endpoint, host:port (default "+prodEndpoint+")")
	pf.BoolVar(&o.sandbox, "sandbox", false, "use the public sandbox, "+sandboxEndpoint)
	pf.StringVar(&o.caFile, "ca-file", "", "PEM file of the roots trusted to verify Coinbase's certificate (default the system's; "+macOSRoots+" on macOS)")
	pf.DurationVar(&o.heartbeat, "heartbeat", o.heartbeat, "heartbeat interval, 1s to 300s")
	pf.BoolVar(&o.json, "json", false, "write JSON lines")
	pf.BoolVar(&o.validate, "validate", false, "validate received messages against Coinbase's dictionaries: invalid ones are rejected, not shown")
	pf.BoolVarP(&o.verbose, "verbose", "v", false, "log the session's events in detail")
	pf.BoolVar(&o.trace, "trace", false, "log every FIX message, field by field, credentials redacted")
	completeFlags(root, map[string]cobra.CompletionFunc{
		"api-key":         cobra.NoFileCompletions,
		"api-key-file":    fileCompletion(),
		"api-secret":      cobra.NoFileCompletions,
		"api-secret-file": fileCompletion(),
		"passphrase":      cobra.NoFileCompletions,
		"passphrase-file": fileCompletion(),
		"endpoint":        cobra.NoFileCompletions,
		"ca-file":         fileCompletion(),
		"heartbeat":       cobra.NoFileCompletions,
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
	add("depth SYMBOL...", "The 10 best levels of each side, then their changes", kindDepth, symbols)
	book := add("book SYMBOL...", "The 10 best levels of each side kept up to date, shown on every change", kindBook, symbols)
	book.Flags().IntVar(&o.show, "show", o.show, "levels of each side shown, 1 to 10")
	completeFlags(book, map[string]cobra.CompletionFunc{"show": cobra.NoFileCompletions})
	add("instruments [SYMBOL...]", "Instruments' trading rules, all if no symbol is given", kindInstruments, cobra.ArbitraryArgs)
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
	symbols := symbolsOf(args)
	if err := o.check(symbols); err != nil {
		return err
	}
	for _, s := range []*secret{&o.apiKey, &o.apiSecret, &o.passphrase} {
		if err := s.read(); err != nil {
			return runError{err}
		}
	}
	log := newLogger(stderr, o.verbose || o.trace)
	if err := stream(ctx, o, cmd, symbols, newOutput(stdout, o.json, isTerminal(stdout)), log); err != nil {
		return runError{err}
	}
	return nil
}

// symbolsOf returns the symbols of args, upper case, each once: Coinbase
// refuses a second subscription to one.
func symbolsOf(args []string) []string {
	var symbols []string
	for _, s := range args {
		if s = strings.ToUpper(s); !slices.Contains(symbols, s) {
			symbols = append(symbols, s)
		}
	}
	return symbols
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

// check completes o from the environment and checks it.
func (o *options) check(symbols []string) error {
	var errs []error
	for _, s := range []*secret{&o.apiKey, &o.apiSecret, &o.passphrase} {
		s.resolve()
		if s.value == "" && s.file == "" {
			errs = append(errs, fmt.Errorf("no %s: set --%s, --%s-file or %s", strings.ReplaceAll(s.name, "-", " "), s.name, s.name, s.env))
		}
	}
	if o.endpoint == "" {
		o.endpoint = prodEndpoint
		if o.sandbox {
			o.endpoint = sandboxEndpoint
		}
	}
	if o.heartbeat < time.Second || o.heartbeat > 300*time.Second || o.heartbeat%time.Second != 0 {
		errs = append(errs, fmt.Errorf("--heartbeat %v: whole seconds from 1s to 300s", o.heartbeat))
	}
	if o.show < 1 || o.show > 10 {
		errs = append(errs, fmt.Errorf("--show %d: 1 to 10, the levels Coinbase sends", o.show))
	}
	for _, s := range symbols {
		// Symbols end up in request IDs.
		if !validSymbol(s) {
			errs = append(errs, fmt.Errorf("symbol %q: 1 to 20 letters, digits or -, as BTC-USD", s))
		}
	}
	return errors.Join(errs...)
}

// validSymbol reports whether s is a product ID: 1 to 20 letters, digits
// or -.
func validSymbol(s string) bool {
	if len(s) == 0 || len(s) > 20 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
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
