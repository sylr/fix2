package main

import (
	"bytes"
	"context"
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
	walk(newCommand(env{stdout: io.Discard, stderr: io.Discard, getenv: func(string) string { return "" }}))
}

// complete returns what the shell completion of args gets: a line per
// completion, the directive last.
func complete(t *testing.T, args ...string) []string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	e := env{stdout: &stdout, stderr: &stderr, getenv: func(string) string { return "" }}
	if code := run(context.Background(), e, append([]string{"__complete"}, args...)); code != 0 {
		t.Fatalf("%q: exit %d: %s", args, code, stderr.String())
	}
	return strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
}

func TestCompletion(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"logon", "--key-file", ""}, []string{":0"}},     // any file: a key, whatever its name
		{[]string{"probe", "--api-key-file", ""}, []string{":0"}}, // any file
		{[]string{"logon", "--sender", ""}, []string{":4"}},       // NoFileComp
		{[]string{"probe", "--wait", ""}, []string{":4"}},
		{[]string{"logon", "--signature-time", ""}, []string{"us", "ms", "s", "ns", "unix-us", "unix-ms", "auto", ":4"}},
		{[]string{"probe", "--signature-time", ""}, []string{"us", "ms", "s", "ns", "unix-us", "unix-ms", ":4"}},
		{[]string{"logon", ""}, []string{":4"}}, // no argument
		{[]string{"version", ""}, []string{":4"}},
		{[]string{"probe", "order", "c", "LIMITS", ""}, []string{"logon-seqnum\t(a)", "cancel-replace\t(d)", "list-cancel\t(e)", "fill\t(f)", "mass-cancel\t(g)", ":4"}},
	} {
		if got := complete(t, tc.args...); !slices.Equal(got, tc.want) {
			t.Errorf("%q: %q, want %q", tc.args, got, tc.want)
		}
	}
}
