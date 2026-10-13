package sqlite3_test

import (
	"context"
	"path/filepath"
	"testing"
)

// BenchmarkAppend stores 200-byte messages, synced every batch of size
// messages, as the session's writer does.
func BenchmarkAppend(b *testing.B) {
	for _, batch := range []uint64{1, 64} {
		b.Run(map[uint64]string{1: "SyncEach", 64: "SyncPer64"}[batch], func(b *testing.B) {
			ctx := context.Background()
			s, st := open(b, filepath.Join(b.TempDir(), "fix2.db"), "S")
			raw := make([]byte, 200)
			b.SetBytes(int64(len(raw)))
			seq := uint64(0)
			for b.Loop() {
				seq++
				if err := s.Append(ctx, st.Generation, seq, raw); err != nil {
					b.Fatal(err)
				}
				if seq%batch == 0 {
					if err := s.Sync(ctx, st.Generation, seq); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
