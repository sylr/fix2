package dictionary

import (
	"testing"
)

func TestTagTable(t *testing.T) {
	entries := map[int]tagEntry{}
	for _, tag := range []int{-7, 0, 1, 8, 1 << 30, -(1 << 30)} {
		entries[tag] = tagEntry{slot: int32(tag & 0xff)}
	}
	// Tags sharing their low bits, as a crafted dictionary could choose.
	for i := range 1000 {
		entries[i<<20] = tagEntry{slot: int32(i)}
	}
	tt := newTagTable(entries)
	for tag, want := range entries {
		if e := tt.get(tag); e == nil || e.slot != want.slot || e.tag != tag {
			t.Fatalf("get(%d) = %+v, want slot %d", tag, e, want.slot)
		}
	}
	for _, tag := range []int{2, -1, 1<<30 + 1, 1001 << 20} {
		if e := tt.get(tag); e != nil {
			t.Errorf("get(%d) = %+v, want nil", tag, e)
		}
	}
	if e := newTagTable(nil); e.get(1) != nil {
		t.Error("empty table found a tag")
	}
}
