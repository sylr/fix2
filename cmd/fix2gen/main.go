// Command fix2gen generates typed Go code for the messages of fix2's
// protocols, one subcommand per encoding.
//
// Usage:
//
//	fix2gen tagvalue --dict <xml-path> --pkg <package-name> [--out <dir>] [--dispatch] [--bytes]
//	fix2gen sbe --schema <xml-path> --pkg <package-name> [--out <dir>] [--tolerate-foreign-metadata] [--tolerate-undeclared-primitive-types] [--tolerate-group-redefinitions] [--tolerate-field-redefinitions] [--tolerate-unfilled-counts] [--assume-same-encodings]
//	fix2gen --version
//
// sbe generates codecs of the messages of an SBE 1.0 message schema: a Go
// struct per message, encoded and decoded whole (see docs/sbe.md). By
// default it follows SBE strictly: a schema with elements or attributes of
// other namespaces is refused, unless --tolerate-foreign-metadata; a field
// whose type names a primitive no encoding is named, unless
// --tolerate-undeclared-primitive-types; a group name or id standing for
// different groups, unless --tolerate-group-redefinitions; a field name or
// id standing for different fields, unless --tolerate-field-redefinitions;
// decoders
// return ErrMalformed for a header's or dimension's numGroups or
// numVarDataFields other than the received version's, unless
// --tolerate-unfilled-counts, for counts left zero; and ErrNewer for the
// groups and data of a newer schema version, which --assume-same-encodings
// lets them skip.
//
// tagvalue generates readers and builders of FIX tagvalue messages from a
// QuickFIX XML dictionary, or from a FIX Orchestra repository such as those
// the FIX Trading Community publishes for FIX Latest, FIX 4.2, FIX 4.4 and
// the FIXT session layer (see xmldict.Load), for the fix package. With
// --dispatch, it also generates a typed Handler interface and a Dispatch
// function routing messages to its methods. With --bytes, the getters of
// numbers, booleans and times come with <Field>Bytes getters of the raw
// values, such as exact decimal prices.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"

	"github.com/spf13/cobra"
	"github.com/sylr/fix2/fix/dictionary/xmldict"
	"github.com/sylr/fix2/internal/gen/sbe"
	"github.com/sylr/fix2/internal/gen/tagvalue"
)

// Version is the version of fix2gen, if set at build time; otherwise
// version tells the version of fix2 it was built from.
var Version = ""

// module is fix2's module path.
const module = "github.com/sylr/fix2"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// version returns the version of fix2gen: Version, or else the version of
// fix2 in info, the build's (go install, go tool, go build), "dev" when
// unknown, as when built from a fix2 replaced by a directory.
func version(info *debug.BuildInfo, ok bool) string {
	if Version != "" {
		return Version
	}
	if !ok {
		return "dev"
	}
	m := &info.Main
	if m.Path != module {
		m = nil
		for _, d := range info.Deps {
			if d.Path == module {
				m = d
			}
		}
	}
	switch {
	case m == nil:
		return "dev"
	case m.Replace != nil && m.Replace.Version == "":
		return "dev"
	case m.Replace != nil:
		m = m.Replace
	}
	if m.Version == "" || m.Version == "(devel)" {
		return "dev"
	}
	return m.Version
}

// runError is an error of a command that ran, as opposed to a usage error.
type runError struct{ err error }

func (e runError) Error() string { return e.err.Error() }
func (e runError) Unwrap() error { return e.err }

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

// run runs fix2gen with args, the command line without the program name,
// and returns its exit code: 2 for a usage error, 1 for a command that
// failed.
func run(args []string, stdout, stderr io.Writer) int {
	// A failed write of stdout, cobra's own (help, --version, completion)
	// included, fails the command: exit 1.
	out := &errWriter{w: stdout}
	root := newCommand(out, stderr)
	// Given nil, cobra would read os.Args.
	root.SetArgs(append([]string{}, args...))
	err := root.Execute()
	werr := out.failed()
	if err == nil && werr == nil {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "fix2gen: %v\n", err)
	}
	// A failed write of stdout, unless it is the error told, is told too.
	if werr != nil && !errors.Is(err, werr) {
		fmt.Fprintf(stderr, "fix2gen: %v\n", werr)
	}
	if errors.As(err, new(runError)) || werr != nil {
		return 1
	}
	fmt.Fprintf(stderr, "Run 'fix2gen --help' for usage.\n")
	return 2
}

// newCommand returns the fix2gen command, writing help to stdout and errors
// to stderr.
func newCommand(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "fix2gen",
		Short: "Generate typed Go code for the messages of fix2's protocols",
		Long: `Generate typed Go code for the messages of fix2's protocols, one
subcommand per encoding: tagvalue for FIX tagvalue, sbe for Simple Binary
Encoding.`,
		Version:       version(debug.ReadBuildInfo()),
		SilenceUsage:  true,
		SilenceErrors: true,
		// Without a command, nothing asked for was done: a usage error.
		RunE: func(*cobra.Command, []string) error {
			return errors.New("no command given")
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	// -v is --verbose in the contribs: no shorthand for --version.
	root.Flags().Bool("version", false, "version for fix2gen")
	root.AddCommand(tagValueCommand(), sbeCommand())
	return root
}

func tagValueCommand() *cobra.Command {
	var dictPath, pkg, outDir string
	var opts tagvalue.Options
	cmd := &cobra.Command{
		Use:   "tagvalue --dict <xml-path> --pkg <package-name>",
		Short: "Generate readers and builders of FIX tagvalue messages from a dictionary",
		Long: `Generate readers and builders of FIX tagvalue messages from a QuickFIX
XML dictionary, or from a FIX Orchestra repository such as those the FIX
Trading Community publishes for FIX Latest, FIX 4.2, FIX 4.4 and the FIXT
session layer, for the fix package. With --dispatch, it also generates a
typed Handler interface and a Dispatch function routing messages to its
methods. With --bytes, the getters of numbers, booleans and times come with
<Field>Bytes getters of the raw values, such as exact decimal prices.`,
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(*cobra.Command, []string) error {
			// Required, given empty: missing all the same.
			if dictPath == "" || pkg == "" {
				return errors.New("--dict and --pkg must not be empty")
			}
			dict, err := xmldict.LoadFile(dictPath)
			if err != nil {
				return runError{err}
			}
			if err := tagvalue.Generate(dict, pkg, outDir, opts); err != nil {
				return runError{err}
			}
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&dictPath, "dict", "", "QuickFIX XML dictionary or Orchestra repository file (required)")
	fs.StringVar(&pkg, "pkg", "", "Go package name for output (required)")
	fs.StringVar(&outDir, "out", ".", "output directory")
	fs.BoolVar(&opts.Dispatch, "dispatch", false, "generate a typed Handler interface and Dispatch")
	fs.BoolVar(&opts.Bytes, "bytes", false, "generate <Field>Bytes getters of raw values for numbers, booleans and times")
	cmd.MarkFlagRequired("dict")
	cmd.MarkFlagRequired("pkg")
	completeFlags(cmd, map[string]cobra.CompletionFunc{
		"dict": fileCompletion("xml"),
		"pkg":  cobra.NoFileCompletions,
		"out":  dirCompletion,
	})
	return cmd
}

func sbeCommand() *cobra.Command {
	var schemaPath, pkg, outDir string
	var load sbe.LoadOptions
	var opts sbe.Options
	cmd := &cobra.Command{
		Use:   "sbe --schema <xml-path> --pkg <package-name>",
		Short: "Generate codecs of the messages of an SBE 1.0 message schema",
		Long: `Generate codecs of the messages of an SBE 1.0 message schema: a Go struct
per message, encoded and decoded whole (see docs/sbe.md).

By default it follows SBE strictly: a schema with elements or attributes of
other namespaces is refused, unless --tolerate-foreign-metadata; a field
whose type names a primitive no encoding is named, unless
--tolerate-undeclared-primitive-types; a group name or id standing for
different groups, unless --tolerate-group-redefinitions; a field name or id
standing for different fields, unless --tolerate-field-redefinitions;
decoders return ErrMalformed for a header's or dimension's numGroups or
numVarDataFields other than the received version's, unless
--tolerate-unfilled-counts, for counts left zero; and ErrNewer for the
groups and data of a newer schema version, which --assume-same-encodings
lets them skip.`,
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(*cobra.Command, []string) error {
			// Required, given empty: missing all the same.
			if schemaPath == "" || pkg == "" {
				return errors.New("--schema and --pkg must not be empty")
			}
			s, err := sbe.LoadFileWith(schemaPath, load)
			if err != nil {
				return runError{err}
			}
			if err := sbe.GenerateWith(s, pkg, outDir, opts); err != nil {
				return runError{err}
			}
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&schemaPath, "schema", "", "SBE message schema file (required)")
	fs.StringVar(&pkg, "pkg", "", "Go package name for output (required)")
	fs.StringVar(&outDir, "out", ".", "output directory")
	fs.BoolVar(&load.TolerateForeignMetadata, "tolerate-foreign-metadata", false,
		"ignore elements and attributes of other namespaces than SBE's, which sbe.xsd does not allow")
	fs.BoolVar(&load.TolerateUndeclaredPrimitiveTypes, "tolerate-undeclared-primitive-types", false,
		"resolve a field's type naming a primitive no encoding of <types> is named as that primitive, which a field's type may not name")
	fs.BoolVar(&load.TolerateGroupRedefinitions, "tolerate-group-redefinitions", false,
		"accept a group name or id standing for different groups, which SBE forbids, generating a type for each")
	fs.BoolVar(&load.TolerateFieldRedefinitions, "tolerate-field-redefinitions", false,
		"accept a field name or id standing for fields of different ids, names or encodings, which SBE forbids")
	fs.BoolVar(&opts.AssumeSameEncodings, "assume-same-encodings", false,
		"decoders skip a newer version's groups and data by the schema's groupSizeEncoding and data length, which SBE does not guarantee")
	fs.BoolVar(&opts.TolerateUnfilledCounts, "tolerate-unfilled-counts", false,
		"take numGroups and numVarDataFields of 0 for counts left unfilled, as Real Logic's encoders leave them")
	cmd.MarkFlagRequired("schema")
	cmd.MarkFlagRequired("pkg")
	completeFlags(cmd, map[string]cobra.CompletionFunc{
		"schema": fileCompletion("xml"),
		"pkg":    cobra.NoFileCompletions,
		"out":    dirCompletion,
	})
	return cmd
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

// fileCompletion completes files of extensions exts, any file without.
func fileCompletion(exts ...string) cobra.CompletionFunc {
	return func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(exts) == 0 {
			return nil, cobra.ShellCompDirectiveDefault
		}
		return exts, cobra.ShellCompDirectiveFilterFileExt
	}
}

// dirCompletion completes directories.
func dirCompletion(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveFilterDirs
}
