package quickfixgo

import (
	"strconv"
	"sync"
	"testing"

	fix2 "github.com/sylr/fix2"
)

// https://github.com/quickfixgo/quickfix/issues/320
//
// Reading a received message from several goroutines, without modifying it,
// crashed with "concurrent map read and map write": its field map is not safe
// for concurrent use, and QuickFIX/Go wrote to it. A kept message must be safe
// to read concurrently, groups included, while the session goes on receiving.
func TestIssue320_ConcurrentReadsOfReceivedMessage(t *testing.T) {
	_, rec, p := acceptedPeer(t, nil)
	p.send("8", 0, "37", "O1", "17", "E1", "150", "2", "39", "2", "55", "SYM", "54", "1",
		"151", "0", "14", "100", "6", "12.5", "118", "1250",
		"453", "2", "448", "P1", "447", "D", "452", "1", "448", "P2", "447", "D", "452", "3")
	rec.wait(t, "the ExecutionReport", func() bool { return len(rec.apps) == 1 })
	rec.mu.Lock()
	m := rec.apps[0]
	rec.mu.Unlock()

	// A spec large enough to index its tags on first use, which every
	// goroutine races to.
	spec := &fix2.GroupSpec{Counter: 453, Delim: 448, Tags: []int{448, 447, 452}}
	for tag := 9000; len(spec.Tags) < 20; tag++ {
		spec.Tags = append(spec.Tags, tag)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if string(m.Get(6)) != "12.5" || string(m.Get(118)) != "1250" || string(m.MsgType()) != "8" {
					t.Error("wrong AvgPx, NetMoney or MsgType")
					return
				}
				n := 0
				for range m.TopLevelFields(spec) {
					n++
				}
				if n != m.Count()-6 {
					t.Errorf("%d top level fields, want %d", n, m.Count()-6)
					return
				}
				if g := m.GroupsOf(spec); len(g) != 2 || string(g[1].Get(452)) != "3" {
					t.Errorf("parties %v", g)
					return
				}
				if m.Clone().Count() != m.Count() {
					t.Error("clone differs")
					return
				}
			}
		})
	}
	for i := range 50 {
		p.send("8", 0, "37", "O"+strconv.Itoa(i+2), "17", "E", "150", "0", "39", "0", "55", "SYM", "54", "1", "151", "100", "14", "0", "6", "0")
	}
	wg.Wait()
	rec.wait(t, "the other ExecutionReports", func() bool { return len(rec.apps) == 51 })
}
