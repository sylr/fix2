package main

import (
	"bytes"
	"os"
	"testing"
)

// codecs.go is aliasgen's output for the codecs as generated: regenerated
// with them (go generate ./...).
func TestUpToDate(t *testing.T) {
	want, err := generate("../fixsbe", "binancefix", "github.com/sylr/fix2/contribs/binancefix/internal/fixsbe")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../codecs.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("codecs.go is not up to date: run go generate ./...")
	}
}
