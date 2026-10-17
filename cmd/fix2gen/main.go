// Command fix2gen generates typed Go code for the messages of fix2's
// protocols, one subcommand per encoding.
//
// Usage:
//
//	fix2gen tagvalue -dict <xml-path> -pkg <package-name> [-out <dir>] [-dispatch] [-bytes]
//	fix2gen sbe -schema <xml-path> -pkg <package-name> [-out <dir>] [-tolerate-foreign-metadata] [-tolerate-undeclared-primitive-types] [-tolerate-group-redefinitions] [-tolerate-field-redefinitions] [-tolerate-unfilled-counts] [-assume-same-encodings]
//
// sbe generates codecs of the messages of an SBE 1.0 message schema: a Go
// struct per message, encoded and decoded whole (see docs/sbe.md). By
// default it follows SBE strictly: a schema with elements or attributes of
// other namespaces is refused, unless -tolerate-foreign-metadata; a field
// whose type names a primitive no encoding is named, unless
// -tolerate-undeclared-primitive-types; a group name or id standing for
// different groups, unless -tolerate-group-redefinitions; a field name or
// id standing for different fields, unless -tolerate-field-redefinitions;
// decoders
// return ErrMalformed for a header's or dimension's numGroups or
// numVarDataFields other than the received version's, unless
// -tolerate-unfilled-counts, for counts left zero; and ErrNewer for the
// groups and data of a newer schema version, which -assume-same-encodings
// lets them skip.
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
	"github.com/sylr/fix2/internal/gen/sbe"
	"github.com/sylr/fix2/internal/gen/tagvalue"
)

const (
	tagValueUsage = "fix2gen tagvalue -dict <xml-path> -pkg <package-name> [-out <dir>] [-dispatch] [-bytes]"
	sbeUsage      = "fix2gen sbe -schema <xml-path> -pkg <package-name> [-out <dir>] [-tolerate-foreign-metadata] [-tolerate-undeclared-primitive-types] [-tolerate-group-redefinitions] [-tolerate-field-redefinitions] [-tolerate-unfilled-counts] [-assume-same-encodings]"
	usage         = "Usage:\n  " + tagValueUsage + "\n  " + sbeUsage + "\n"
)

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
	case "sbe":
		return runSBE(args[1:], stderr)
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

func runSBE(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("fix2gen sbe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	schemaPath := fs.String("schema", "", "SBE message schema file (required)")
	pkg := fs.String("pkg", "", "Go package name for output (required)")
	outDir := fs.String("out", ".", "output directory")
	var load sbe.LoadOptions
	var opts sbe.Options
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
	if err := fs.Parse(args); err == flag.ErrHelp {
		return 0
	} else if err != nil {
		return 2
	}

	if *schemaPath == "" || *pkg == "" || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
		return 1
	}

	s, err := sbe.LoadFileWith(*schemaPath, load)
	if err != nil {
		fmt.Fprintf(stderr, "fix2gen: %v\n", err)
		return 1
	}

	if err := sbe.GenerateWith(s, *pkg, *outDir, opts); err != nil {
		fmt.Fprintf(stderr, "fix2gen: %v\n", err)
		return 1
	}
	return 0
}
