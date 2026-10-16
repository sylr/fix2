// Package bigrich holds the codecs fix2gen sbe generates from
// testdata/bigrich.xml, rich.xml big-endian, and their tests.
package bigrich

//go:generate go run ../../../../cmd/fix2gen sbe -schema ../testdata/bigrich.xml -pkg bigrich -out .
