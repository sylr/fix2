package quickfixgo

import (
	"errors"
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
)

// https://github.com/quickfixgo/quickfix/issues/366
//
// The validator walked a group entry's fields in step with the dictionary's,
// skipping the definitions of the fields absent but never moving past a field
// that did not match: a field coming before one the definition lists first
// ended the entry, and the entries left were not counted, Incorrect
// NumInGroup. Entries are delimited by their first field, whatever the order
// of the others: counted, they are rejected for that order only when
// dictionary.CheckGroupOrder is on, as it is in CheckAll.
func TestIssue366_EntriesInAnotherOrderCounted(t *testing.T) {
	d := dict(t, "FIX44")
	// NoAllocs lists AllocAccount (79), AllocQty (80), AllocPrice (366),
	// IndividualAllocID (467), then NoDlvyInst (5000).
	m, err := fix.Parse(raw("8=FIX.4.4|35=D|49=INI|56=ACC|34=2|52=20240101-00:00:00.000|" +
		"11=1|54=1|60=20240101-00:00:00.000|40=2|44=10|55=EUR/USD|78=3|" +
		"79=A|467=I1|80=10|" +
		"79=B|366=1.5|5000=1|5001=X|80=5|" +
		"79=C"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ValidateWith(&m, dictionary.CheckAll&^dictionary.CheckGroupOrder); err != nil {
		t.Fatalf("NoAllocs entries in another order rejected: %v", err)
	}
	var ve *dictionary.ValidationError
	if err := d.ValidateWith(&m, dictionary.CheckAll); !errors.As(err, &ve) || ve.Reason != dictionary.ReasonRepeatingGroupOutOfOrder {
		t.Fatalf("CheckAll: %v, want repeating group fields out of order", err)
	}
}
