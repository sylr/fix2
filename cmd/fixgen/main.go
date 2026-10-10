// Command fixgen generates typed Go code for FIX messages from a QuickFIX XML dictionary.
//
// Usage:
//
//	fixgen -dict <xml-path> -pkg <package-name> [-out <dir>]
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sylr/fix2/dictionary/xmldict"
)

func main() {
	dictPath := flag.String("dict", "", "QuickFIX XML dictionary file (required)")
	pkg := flag.String("pkg", "", "Go package name for output (required)")
	outDir := flag.String("out", ".", "output directory")
	flag.Parse()

	if *dictPath == "" || *pkg == "" {
		fmt.Fprintf(os.Stderr, "Usage: fixgen -dict <xml-path> -pkg <package-name> [-out <dir>]\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	dict, err := xmldict.LoadFile(*dictPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixgen: %v\n", err)
		os.Exit(1)
	}

	if err := generate(dict, *pkg, *outDir); err != nil {
		fmt.Fprintf(os.Stderr, "fixgen: %v\n", err)
		os.Exit(1)
	}
}
