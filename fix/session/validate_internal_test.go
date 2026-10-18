package session

import (
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/tests/dicts/fix44"
)

// A valid message costs validateWith no allocation beyond the dictionary's.
func TestValidateWithValidDoesNotAllocate(t *testing.T) {
	d := fix44.Dictionary()
	s, err := New(Config{
		ID:           SessionID{BeginString: FIX44, SenderCompID: "A", TargetCompID: "B"},
		Handler:      BaseHandler{},
		Dictionaries: Dictionaries{Session: d},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := fix.NewBuilder(FIX44, "0").
		SetString(49, "B").SetString(56, "A").SetInt(34, 2).SetString(52, "20261018-12:00:00.000").
		Build()
	m, err := fix.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if verr := s.validateWith(d, &m); verr != nil {
		t.Fatalf("validateWith: %v", verr)
	}
	checks := s.validationChecks()
	dict := testing.AllocsPerRun(100, func() { _ = d.ValidateWith(&m, checks) })
	got := testing.AllocsPerRun(100, func() { _ = s.validateWith(d, &m) })
	if got != dict {
		t.Errorf("validateWith: %v allocs, the dictionary's %v", got, dict)
	}
}
