package fixt11

import (
	"slices"
	"testing"

	fix2 "github.com/sylr/fix2"
)

// TestHeaderHops round-trips the FIXT 1.1 header NoHops group, which every
// message carries, with two hops.
func TestHeaderHops(t *testing.T) {
	raw := BuildHeartbeat("FIXT.1.1").
		SetSenderCompID("S").
		SetTargetCompID("T").
		SetMsgSeqNum(1).
		AddNoHopsEntry(NewNoHopsEntry().SetHopCompID("A").SetHopRefID(1)).
		AddNoHopsEntry(NewNoHopsEntry().SetHopCompID("B").SetHopRefID(2)).
		Build()
	msg, err := fix2.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	hops := WrapHeartbeat(&msg).NoHopsEntries()
	if len(hops) != 2 {
		t.Fatalf("%q: %d hops, want 2", raw, len(hops))
	}
	for i, want := range []struct {
		id  string
		ref int
	}{{"A", 1}, {"B", 2}} {
		if id, _ := hops[i].HopCompID(); id != want.id {
			t.Errorf("hop %d HopCompID = %q, want %q", i, id, want.id)
		}
		if ref, _, _ := hops[i].HopRefID(); ref != want.ref {
			t.Errorf("hop %d HopRefID = %d, want %d", i, ref, want.ref)
		}
	}
}

// TestHeaderFirst checks that the standard header, header groups included,
// is written before the body whatever order fields were set in.
func TestHeaderFirst(t *testing.T) {
	raw := BuildHeartbeat("FIXT.1.1").
		SetTestReqID("T").
		AddNoHopsEntry(NewNoHopsEntry().SetHopCompID("A")).
		SetSenderCompID("S").
		SetTargetCompID("D").
		SetMsgSeqNum(1).
		Build()
	msg, err := fix2.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	var tags []int
	for _, f := range msg.Fields() {
		tags = append(tags, f.Tag)
	}
	want := []int{8, 9, 35, TagSenderCompID, TagTargetCompID, TagMsgSeqNum, TagNoHops, TagHopCompID, TagTestReqID, 10}
	if !slices.Equal(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
}
