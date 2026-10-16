// Command fix2gen generates typed Go code for the messages of fix2's
// protocols, one subcommand per encoding.
//
// Usage:
//
//	fix2gen tagvalue -dict <xml-path> -pkg <package-name> [-out <dir>] [-dispatch] [-bytes]
//
// tagvalue generates readers and builders of FIX tagvalue messages from a
// QuickFIX XML dictionary, or from a FIX Orchestra repository such as those
// the FIX Trading Community publishes for FIX Latest, FIX 4.2, FIX 4.4 and
// the FIXT session layer (see xmldict.Load), for the fix package. With
// -dispatch, it also generates a typed Handler interface and a Dispatch
// function routing messages to its methods. With -bytes, the getters of
// numbers, booleans and times come with <Field>Bytes getters of the raw
// values, such as exact decimal prices.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sylr/fix2/fix/dictionary/xmldict"
	"github.com/sylr/fix2/internal/gen/tagvalue"
)

const usage = "Usage: fix2gen tagvalue -dict <xml-path> -pkg <package-name> [-out <dir>] [-dispatch] [-bytes]\n"

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run runs fix2gen with args, the command line without the program name,
// and returns its exit code.
func run(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "tagvalue":
		return runTagValue(args[1:], stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stderr, usage)
		return 0
	}
	fmt.Fprintf(stderr, "fix2gen: unknown subcommand %q\n%s", args[0], usage)
	return 2
}

func runTagValue(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("fix2gen tagvalue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dictPath := fs.String("dict", "", "QuickFIX XML dictionary or Orchestra repository file (required)")
	pkg := fs.String("pkg", "", "Go package name for output (required)")
	outDir := fs.String("out", ".", "output directory")
	dispatch := fs.Bool("dispatch", false, "generate a typed Handler interface and Dispatch")
	raw := fs.Bool("bytes", false, "generate <Field>Bytes getters of raw values for numbers, booleans and times")
	if err := fs.Parse(args); err == flag.ErrHelp {
		return 0
	} else if err != nil {
		return 2
	}

	if *dictPath == "" || *pkg == "" || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
		return 1
	}

	dict, err := xmldict.LoadFile(*dictPath)
	if err != nil {
		fmt.Fprintf(stderr, "fix2gen: %v\n", err)
		return 1
	}

	if err := tagvalue.Generate(dict, *pkg, *outDir, tagvalue.Options{Dispatch: *dispatch, Bytes: *raw}); err != nil {
		fmt.Fprintf(stderr, "fix2gen: %v\n", err)
		return 1
	}
	return 0
}
