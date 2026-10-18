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
	walk(newCommand(io.Discard, io.Discard))
}

// complete returns what the shell completion of args gets: a line per
// completion, the directive last.
func complete(t *testing.T, args ...string) []string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), append([]string{"__complete"}, args...), &stdout, &stderr); code != 0 {
		t.Fatalf("%q: exit %d: %s", args, code, stderr.String())
	}
	return strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
}

func TestCompletion(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"server", "--tls-cert", ""}, []string{":0"}}, // any file: certificates and keys, whatever their names
		{[]string{"server", "--tls-key", ""}, []string{":0"}},
		{[]string{"client", "--tls-ca", ""}, []string{":0"}},
		{[]string{"server", "--listen", ""}, []string{":4"}}, // NoFileComp
		{[]string{"client", "--count", ""}, []string{":4"}},
		{[]string{"client", "--token", ""}, []string{":4"}},
		{[]string{"server", "--flow", ""}, []string{"Recoverable", "Idempotent", "Unsequenced", ":4"}},
		{[]string{"client", "--log-level", ""}, []string{"debug", "info", "warn", "error", ":4"}},
		// Lists: the flows not given yet, no space after (NoFileComp|NoSpace).
		{[]string{"server", "--client-flows", ""}, []string{"Recoverable", "Idempotent", "Unsequenced", ":6"}},
		{[]string{"client", "--server-flows", "idempotent,Unsequenced,"}, []string{"idempotent,Unsequenced,Recoverable", ":6"}},
		// What is typed is kept, spaces and case: the shell matches on it.
		{[]string{"server", "--client-flows", "Recoverable, I"}, []string{"Recoverable, Idempotent", ":6"}},
		{[]string{"server", "--client-flows", "Recoverable,  "}, []string{"Recoverable,  Idempotent", "Recoverable,  Unsequenced", ":6"}},
		{[]string{"server", "--client-flows", "recoverable , un"}, []string{"recoverable , unsequenced", ":6"}},
		{[]string{"client", "--server-flows", "Idempotent,idem"}, []string{":6"}}, // given already
		{[]string{"client", "--server-flows", "X"}, []string{":6"}},
		{[]string{"client", ""}, []string{":4"}}, // no argument
		{[]string{"version", ""}, []string{":4"}},
	} {
		if got := complete(t, tc.args...); !slices.Equal(got, tc.want) {
			t.Errorf("%q: %q, want %q", tc.args, got, tc.want)
		}
	}
}
