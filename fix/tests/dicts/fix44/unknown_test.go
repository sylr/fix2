package fix44

import (
	"strconv"
	"testing"

	"github.com/sylr/fix2/fix"
)

// TestEntriesKeepUnknownTags checks that the readers' group entries keep
// the tags the message does not define, as validation does: the entry's
// other fields and the next entries are read still.
func TestEntriesKeepUnknownTags(t *testing.T) {
	b := fix.NewBuilder("FIX.4.4", "D")
	for _, f := range [][2]string{
		{"11", "O1"}, {"78", "2"},
		{"79", "A"}, {"9001", "x"}, {"80", "1"}, {"5000", "1"}, {"5001", "D"}, {"9002", "y"}, {"5002", "C"},
		{"79", "B"}, {"4999", "z"}, {"80", "2"},
		{"55", "EUR/USD"},
	} {
		tag, _ := strconv.Atoi(f[0])
		b.Append(tag, []byte(f[1]))
	}
	m, err := fix.Parse(b.Build())
	if err != nil {
		t.Fatal(err)
	}
	r := WrapNewOrderSingle(&m)
	entries := r.NoAllocsEntries()
	if len(entries) != 2 {
		t.Fatalf("%d entries, want 2", len(entries))
	}
	for i, want := range []struct {
		account string
		qty     float64
	}{{"A", 1}, {"B", 2}} {
		account, _ := entries[i].AllocAccount()
		qty, _, _ := entries[i].AllocQty()
		if account != want.account || qty != want.qty {
			t.Errorf("entry %d: AllocAccount %q, AllocQty %v, want %q, %v", i, account, qty, want.account, want.qty)
		}
	}
	dlvy := entries[0].NoDlvyInstEntries()
	if len(dlvy) != 1 {
		t.Fatalf("%d NoDlvyInst entries, want 1", len(dlvy))
	}
	if code, _ := dlvy[0].DlvyInstCode(); code != "C" {
		t.Errorf("DlvyInstCode = %q, want C", code)
	}
	if symbol, _ := r.Symbol(); symbol != "EUR/USD" {
		t.Errorf("Symbol = %q, want EUR/USD", symbol)
	}
}
