package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

// settle records message seq of the receipts' generation as written on
// connection 2, and acknowledged if acked.
func settle(t *receipts, seq uint64, acked bool) {
	t.written(t.gen, seq, 2)
	if acked {
		rec := t.recs[seq]
		rec.acked = true
		t.recs[seq] = rec
	}
}

// endGeneration ends the receipts' generation, as a sequence reset does, and
// returns the one ended.
func endGeneration(t *receipts) uint64 {
	gen := t.gen
	t.newGeneration(gen, gen+1)
	return gen
}

// check compares what receipt r reports, Wait at each level and Rejection,
// with what is wanted.
func check(t *testing.T, r Receipt, written, acked error, rejected bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, c := range []struct {
		level Level
		want  error
	}{{Written, written}, {Acknowledged, acked}} {
		if err := r.Wait(ctx, c.level); !errors.Is(err, c.want) {
			t.Errorf("message %d of generation %d: Wait(%v) = %v, want %v", r.seq, r.gen, c.level, err, c.want)
		}
	}
	if _, ok := r.Rejection(); ok != rejected {
		t.Errorf("message %d of generation %d: Rejection found %v, want %v", r.seq, r.gen, ok, rejected)
	}
}

// Every receipt of a generation ended reports what it did before: written,
// acknowledged, gap-filled, rejected or too old; a message never written
// fails with ErrUnknownDelivery. A receipt of the next generation with the
// same MsgSeqNum reads nothing of it.
func TestReceipts_SettledAcrossReset(t *testing.T) {
	s := fenceSession(t, func(c *Config) { c.MaxTrackedReceipts = 8 })
	s.mu.Lock()
	rs := &s.receipts
	for seq := uint64(1); seq <= 9; seq++ {
		settle(rs, seq, seq == 6) // the ninth evicts 1 to 4
	}
	rs.written(rs.gen, 1, 1) // a late write of the connection replaced
	rs.gapFilled(rs.gen, 7, 9)
	rs.gapFilled(rs.gen, 10, 11) // 11 is not written either
	rs.rejected(rs.gen, 5, 12, RejectInfo{MsgType: "3"})
	rs.rejected(rs.gen, 6, 12, RejectInfo{MsgType: "j"})
	gen := endGeneration(rs)
	s.mu.Unlock()
	r := func(seq uint64) Receipt { return Receipt{s: s, gen: gen, seq: seq} }
	check(t, r(1), nil, ErrUnknownDelivery, false) // written late, below oldest
	check(t, r(2), ErrReceiptUntracked, ErrReceiptUntracked, false)
	check(t, r(5), nil, ErrUnknownDelivery, true)
	check(t, r(6), nil, nil, true)
	check(t, r(7), nil, ErrSkipped, false)
	check(t, r(9), nil, ErrUnknownDelivery, false)
	check(t, r(10), ErrSkipped, ErrSkipped, false)
	check(t, r(11), ErrUnknownDelivery, ErrUnknownDelivery, false)

	next := Receipt{s: s, gen: gen + 1, seq: 6}
	if err := next.Err(); err != nil {
		t.Errorf("message 6 of the next generation, not written: Err = %v, want nil", err)
	}
	if _, ok := next.Rejection(); ok {
		t.Error("message 6 of the next generation: Rejection found, of the generation ended")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := next.Wait(ctx, Written); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("message 6 of the next generation, not written: Wait(Written) = %v, want it pending", err)
	}
}

// What is kept of the generations ended is bounded as one generation is, in
// receipts, gap-filled spans and rejections, and in generations: the oldest
// are forgotten first, their receipts reporting ErrReceiptUntracked, and the
// generation just ended is always kept.
func TestReceipts_EndedBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		max  int // MaxTrackedReceipts
		gens int // generations ended
		fill func(t *receipts)
	}{
		{"receipts", 4, 3, func(t *receipts) {
			settle(t, 1, true)
			settle(t, 2, false)
		}},
		{"gap-filled spans", 4, 3, func(t *receipts) {
			settle(t, 1, true)
			for i := range uint64(maxSkipped / 2) {
				t.gapFilled(t.gen, 2+2*i, 3+2*i)
			}
		}},
		{"rejections", 8, 3, func(t *receipts) {
			settle(t, 1, true)
			for seq := uint64(2); seq <= 4; seq++ {
				t.rejected(t.gen, seq, 5, RejectInfo{})
			}
		}},
		{"generations", 1 << 20, maxGenerations + 1, func(t *receipts) {
			settle(t, 1, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fenceSession(t, func(c *Config) { c.MaxTrackedReceipts = tc.max })
			var gens []uint64
			s.mu.Lock()
			for range tc.gens {
				tc.fill(&s.receipts)
				gens = append(gens, endGeneration(&s.receipts))
			}
			kept := len(s.receipts.ended)
			s.mu.Unlock()
			if kept != tc.gens-1 {
				t.Errorf("%d generations kept, want %d", kept, tc.gens-1)
			}
			check(t, Receipt{s: s, gen: gens[0], seq: 1}, ErrReceiptUntracked, ErrReceiptUntracked, false)
			for _, gen := range gens[1:] {
				check(t, Receipt{s: s, gen: gen, seq: 1}, nil, nil, false)
			}
		})
	}
}
