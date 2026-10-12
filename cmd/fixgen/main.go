// Command fixgen generates typed Go code for FIX messages from a QuickFIX XML
// dictionary, or from a FIX Orchestra repository such as those the FIX
// Trading Community publishes for FIX Latest, FIX 4.2, FIX 4.4 and the FIXT
// session layer (see xmldict.Load).
//
// Usage:
//
//	fixgen -dict <xml-path> -pkg <package-name> [-out <dir>] [-dispatch] [-bytes]
//
// With -dispatch, it also generates a typed Handler interface and a
// Dispatch function routing messages to its methods. With -bytes, the
// getters of numbers, booleans and times come with <Field>Bytes getters of
// the raw values, such as exact decimal prices.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sylr/fix2/dictionary/xmldict"
)

func main() {
	dictPath := flag.String("dict", "", "QuickFIX XML dictionary or Orchestra repository file (required)")
	pkg := flag.String("pkg", "", "Go package name for output (required)")
	outDir := flag.String("out", ".", "output directory")
	dispatch := flag.Bool("dispatch", false, "generate a typed Handler interface and Dispatch")
	raw := flag.Bool("bytes", false, "generate <Field>Bytes getters of raw values for numbers, booleans and times")
	flag.Parse()

	if *dictPath == "" || *pkg == "" {
		fmt.Fprintf(os.Stderr, "Usage: fixgen -dict <xml-path> -pkg <package-name> [-out <dir>] [-dispatch] [-bytes]\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	dict, err := xmldict.LoadFile(*dictPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixgen: %v\n", err)
		os.Exit(1)
	}

	if err := generateWith(dict, *pkg, *outDir, options{dispatch: *dispatch, bytes: *raw}); err != nil {
		fmt.Fprintf(os.Stderr, "fixgen: %v\n", err)
		os.Exit(1)
	}
}
