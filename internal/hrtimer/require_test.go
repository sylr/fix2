package hrtimer

import (
	"cmp"
	"reflect"
	"testing"
	"time"
)

// require is the subset of testify's require the tests copied from rex use.
var require requireT

type requireT struct{}

func (requireT) False(t *testing.T, v bool, msg ...any) {
	t.Helper()
	if v {
		t.Fatal(append([]any{"want false"}, msg...)...)
	}
}

func (requireT) Equal(t *testing.T, want, got any, msg ...any) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatal(append([]any{"not equal:", want, got}, msg...)...)
	}
}

func (requireT) NotEqual(t *testing.T, a, b any, msg ...any) {
	t.Helper()
	if reflect.DeepEqual(a, b) {
		t.Fatal(append([]any{"equal:", a}, msg...)...)
	}
}

func (requireT) Zero(t *testing.T, v any, msg ...any) {
	t.Helper()
	if !reflect.ValueOf(v).IsZero() {
		t.Fatal(append([]any{"not zero:", v}, msg...)...)
	}
}

func (requireT) GreaterOrEqual(t *testing.T, a, b int, msg ...any) {
	t.Helper()
	if cmp.Less(a, b) {
		t.Fatal(append([]any{a, "<", b}, msg...)...)
	}
}

func (requireT) LessOrEqual(t *testing.T, a, b int, msg ...any) {
	t.Helper()
	if cmp.Less(b, a) {
		t.Fatal(append([]any{a, ">", b}, msg...)...)
	}
}

func (requireT) Eventually(t *testing.T, cond func() bool, wait, tick time.Duration, msg ...any) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(append([]any{"condition never met"}, msg...)...)
		}
		time.Sleep(tick)
	}
}
