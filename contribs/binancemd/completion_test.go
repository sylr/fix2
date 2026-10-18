package main

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Every flag taking a value, and every command's arguments, complete:
// files only where a file is expected. A flag or command added must choose.
func TestCompletionChosen(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if _, ok := c.GetFlagCompletionFunc(f.Name); !ok && f.Value.Type() != "bool" {
				t.Errorf("%s --%s: no completion", c.CommandPath(), f.Name)
			}
		})
		if c.HasParent() && c.ValidArgsFunction == nil {
			t.Errorf("%s: no completion of its arguments", c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newCommand(io.Discard, io.Discard))
}

// complete returns what the shell completion of args gets: a line per
// completion, the directive last.
func complete(t *testing.T, args ...string) []string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(append([]string{"__complete"}, args...), &stdout, &stderr); code != 0 {
		t.Fatalf("%q: exit %d: %s", args, code, stderr.String())
	}
	return strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
}

func TestCompletion(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"trades", "--key-file", ""}, []string{":0"}},     // any file, a persistent flag
		{[]string{"limits", "--api-key-file", ""}, []string{":0"}}, // any file
		{[]string{"ticker", "--endpoint", ""}, []string{":4"}},     // NoFileComp
		{[]string{"--heartbeat", ""}, []string{":4"}},
		{[]string{"book", "--show", ""}, []string{":4"}},
		{[]string{"depth", "--levels", ""}, []string{":4"}},
		{[]string{"trades", "BTCUSDT", ""}, []string{":4"}}, // symbols: no files
		{[]string{"instruments", ""}, []string{":4"}},
		{[]string{"limits", ""}, []string{":4"}}, // no argument
	} {
		if got := complete(t, tc.args...); !slices.Equal(got, tc.want) {
			t.Errorf("%q: %q, want %q", tc.args, got, tc.want)
		}
	}
}
