package quickfixgo

import (
	"slices"
	"testing"

	fix2 "github.com/sylr/fix2"
)

// https://github.com/quickfixgo/quickfix/issues/569
//
// Without a dictionary, a message repeating tags (a repeating group the
// parser does not know of) kept only the last occurrence of each: the others
// were lost. Every field must be kept, in order, so that the group can still
// be read.
func TestIssue569_DuplicateTagsKeptWithoutDictionary(t *testing.T) {
	const fields = "8=FIX.4.2|35=UP|34=4|43=N|52=20230627-13:51:13.116|49=xx|56=xxxx|6529=req_id|8054=2|" +
		"6068=AGI|6288=0|8001=PositionList|8002=AGI/USD|6068=PDD|6288=0|8001=PositionList|8002=PDD/USD"
	check := func(t *testing.T, m *fix2.Message) {
		t.Helper()
		var symbols []string
		for _, f := range m.Fields() {
			if f.Tag == 6068 {
				symbols = append(symbols, string(f.Value))
			}
		}
		if !slices.Equal(symbols, []string{"AGI", "PDD"}) {
			t.Errorf("tag 6068 values %q, want [AGI PDD]", symbols)
		}
		g := m.Groups(8054, []int{6068, 6288, 8001, 8002})
		if len(g) != 2 || string(g[0].Get(8002)) != "AGI/USD" || string(g[1].Get(8002)) != "PDD/USD" {
			t.Errorf("group entries %v", g)
		}
	}

	t.Run("parse", func(t *testing.T) {
		m, err := fix2.Parse(raw(fields))
		if err != nil {
			t.Fatal(err)
		}
		check(t, &m)
	})
	t.Run("session", func(t *testing.T) {
		_, rec, p := acceptedPeer(t, nil)
		p.send("UP", 0, "6529", "req_id", "8054", "2",
			"6068", "AGI", "6288", "0", "8001", "PositionList", "8002", "AGI/USD",
			"6068", "PDD", "6288", "0", "8001", "PositionList", "8002", "PDD/USD")
		rec.wait(t, "the message", func() bool { return len(rec.apps) == 1 })
		rec.mu.Lock()
		defer rec.mu.Unlock()
		check(t, rec.apps[0])
	})
}
