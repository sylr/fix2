package memstore

import (
	"context"
	"testing"
)

// Truncating releases chunks for good: the bookkeeping does not grow with
// the messages ever stored.
func TestTruncateReleasesChunks(t *testing.T) {
	ctx := context.Background()
	s := New()
	st, _ := s.Open(ctx)
	big := make([]byte, chunkSize)
	for seq := uint64(1); seq <= 100; seq++ {
		if err := s.Append(ctx, st.Generation, seq, big); err != nil {
			t.Fatal(err)
		}
		if err := s.Truncate(ctx, st.Generation, seq); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.chunks) > 2 || len(s.last) != len(s.chunks) || len(s.index) > 1 {
		t.Errorf("after truncating 100 messages: %d chunks, %d index entries", len(s.chunks), len(s.index))
	}
}
