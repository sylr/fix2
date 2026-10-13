package hrtimer

import (
	"cmp"
	"fmt"
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
		fail(t, "want false", msg)
	}
}

func (requireT) Equal(t *testing.T, want, got any, msg ...any) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		fail(t, fmt.Sprintf("not equal: %v %v", want, got), msg)
	}
}

func (requireT) NotEqual(t *testing.T, a, b any, msg ...any) {
	t.Helper()
	if reflect.DeepEqual(a, b) {
		fail(t, fmt.Sprintf("equal: %v", a), msg)
	}
}

func (requireT) Zero(t *testing.T, v any, msg ...any) {
	t.Helper()
	if !reflect.ValueOf(v).IsZero() {
		fail(t, fmt.Sprintf("not zero: %v", v), msg)
	}
}

func (requireT) GreaterOrEqual(t *testing.T, a, b int, msg ...any) {
	t.Helper()
	if cmp.Less(a, b) {
		fail(t, fmt.Sprintf("%d < %d", a, b), msg)
	}
}

func (requireT) LessOrEqual(t *testing.T, a, b int, msg ...any) {
	t.Helper()
	if cmp.Less(b, a) {
		fail(t, fmt.Sprintf("%d > %d", a, b), msg)
	}
}

func (requireT) Eventually(t *testing.T, cond func() bool, wait, tick time.Duration, msg ...any) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			fail(t, "condition never met", msg)
		}
		time.Sleep(tick)
	}
}

// fail ends the test with failure and, as testify does, msg: a format
// string and its arguments, or a single value.
func fail(t *testing.T, failure string, msg []any) {
	t.Helper()
	if len(msg) > 0 {
		if format, ok := msg[0].(string); ok {
			failure += ": " + fmt.Sprintf(format, msg[1:]...)
		} else {
			failure += ": " + fmt.Sprint(msg...)
		}
	}
	t.Fatal(failure)
}
