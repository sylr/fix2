package quickfixgo

import (
	"testing"

	fix2 "github.com/sylr/fix2"
)

// https://github.com/quickfixgo/quickfix/issues/390
//
// ParseMessage into a zero-value Message panicked assigning to its nil field
// map. A zero Message must be usable: read, cloned and parsed into.
func TestIssue390_ZeroValueMessageParses(t *testing.T) {
	var m fix2.Message
	if m.Count() != 0 || m.Get(35) != nil || m.MsgType() != nil || len(m.Fields()) != 0 ||
		m.Groups(78, []int{79, 80}) != nil || m.Clone().Count() != 0 {
		t.Fatal("zero Message is not empty")
	}
	for range m.TopLevelFields() {
		t.Fatal("zero Message has fields")
	}

	data := raw("8=FIX.4.2|35=D|34=2|49=TW|52=20140515-19:49:56.659|56=ISLD|11=100|21=1|40=1|54=1|55=TSLA|60=00010101-00:00:00.000")
	if err := fix2.ParseInto(&m, data, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(m.Get(55)); got != "TSLA" {
		t.Errorf("Symbol %q, want TSLA", got)
	}

	// The message of the issue, its SOHs lost on the way: an error.
	var z fix2.Message
	if err := fix2.ParseInto(&z, []byte("8=FIX.4.29=10435=D34=249=TW52=20140515-19:49:56.65956=ISLD11=10021=140=154=155=TSLA60=00010101-00:00:00.00010=039"), nil); err == nil {
		t.Error("ParseInto succeeded on a message without SOH")
	}
}
