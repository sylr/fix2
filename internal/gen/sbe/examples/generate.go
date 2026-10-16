// Package examples holds the codecs fix2gen sbe generates from the examples
// of SBE 1.0, section 9 (testdata/examples.xml), and their tests.
package examples

//go:generate go run ../../../../cmd/fix2gen sbe -schema ../testdata/examples.xml -pkg examples -out .
