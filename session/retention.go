package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session/store"
)

// maxRetentionMarks bounds the marks MinAge keeps: beyond, every other one
// goes, which only makes the age of messages known more coarsely.
const maxRetentionMarks = 1024

// retention is the state of Config.Retention, guarded by the session's lock.
type retention struct {
	timer  *time.Timer // the next eviction, nil until the session first connects
	sweeps uint64      // evictions run, for tests
	// acked is the MsgSeqNum of the last of our messages the counterparty
	// acknowledged, if ackKnown.
	acked    uint64
	ackKnown bool
	// ackFrom is the counterparty's MsgSeqNum of the message that told
	// acked: an acknowledgement it sent before counts no more, whatever
	// the order the session handles them in.
	ackFrom uint64
	// unsentLo to unsentHi, if unsentLo is not 0, are messages a connection
	// ended before writing them all: the counterparty may never have had
	// them, until it acknowledges them or a replay resends them.
	unsentLo, unsentHi uint64
	floor              uint64          // the messages below it were evicted, from 1
	marks              []retentionMark // NextSender at each eviction, for MinAge
	warned             bool            // an acknowledgement of messages never sent was logged
	failing            bool            // the last Truncate failed, and was logged
}

// retentionMark records that the messages below next were stored by at.
type retentionMark struct {
	at   time.Time
	next uint64
}

// newGeneration forgets what the counterparty acknowledged of the previous
// generation.
func (r *retention) newGeneration() {
	*r = retention{timer: r.timer, sweeps: r.sweeps, floor: 1, marks: r.marks[:0]}
}

// startRetentionLocked schedules the evictions, once.
func (s *Session) startRetentionLocked() {
	if s.cfg.Retention.Interval <= 0 || s.ret.timer != nil || s.stopped {
		return
	}
	s.ret.timer = time.AfterFunc(s.cfg.Retention.Interval, s.onRetentionTimer)
}

// onRetentionTimer evicts what it can, and schedules the next eviction
// unless nothing can be evicted any more.
func (s *Session) onRetentionTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.halted != nil {
		return
	}
	s.evictLocked(time.Now())
	s.ret.sweeps++
	s.ret.timer.Reset(s.cfg.Retention.Interval)
}

// ackedLocked records that the counterparty has our messages up to seq, as
// source, the tag that says so, of its message from, tells. The last the
// counterparty sent counts. A BeginSeqNo only moves an earlier
// acknowledgement back: the ResendRequest proves the counterparty lacks the
// messages from it on, not that it has all those before. Above our last
// message, it is ignored: the counterparty cannot have processed messages we
// never sent.
func (s *Session) ackedLocked(m *fix2.Message, seq uint64, source int, from uint64) {
	if s.cfg.Retention.Interval <= 0 || from < s.ret.ackFrom {
		return
	}
	if seq >= s.st.NextSender {
		if !s.ret.warned {
			s.ret.warned = true
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "counterparty acknowledges messages we never sent: ignored",
				slog.String("tag", fieldDesc(nil, source)), numAttr(m, "acked", source, int64(seq)), slog.Uint64("last_sent", s.st.NextSender-1))
		}
		return
	}
	// The counterparty has them, or, for a BeginSeqNo, expects nothing
	// before: those of them a connection did not write no longer matter.
	s.ret.deliveredLocked(1, seq)
	s.ret.ackFrom = from
	if source == tagBeginSeqNo && s.ret.ackKnown && seq > s.ret.acked {
		return
	}
	s.ret.acked, s.ret.ackKnown = seq, true
}

// deliveredLocked records that the counterparty has, or was resent, our
// messages begin to end: those of them a connection did not write no longer
// hold back evictions.
func (r *retention) deliveredLocked(begin, end uint64) {
	if r.unsentLo == 0 || begin > r.unsentLo || end < r.unsentLo {
		return
	}
	r.unsentLo = end + 1
	if r.unsentLo > r.unsentHi {
		r.unsentLo, r.unsentHi = 0, 0
	}
}

// unsentLocked records that c stopped tracking its application messages not
// yet written, from lo up to the last message sequenced at most: it ended, or
// dropped them.
func (s *Session) unsentLocked(c *conn) {
	if s.cfg.Retention.Interval <= 0 {
		return
	}
	if lo := c.unwrittenLocked(); lo != 0 {
		s.ret.holdLocked(lo, s.st.NextSender-1)
	}
}

// holdLocked records that the counterparty may never have had our messages
// begin to end.
func (r *retention) holdLocked(begin, end uint64) {
	if r.unsentLo == 0 || begin < r.unsentLo {
		r.unsentLo = begin
	}
	r.unsentHi = max(r.unsentHi, end)
}

// lastProcessedLocked records the LastMsgSeqNumProcessed of m, a message
// the session accepted, unless it is a retransmission, which carries the
// value it was first sent with.
func (s *Session) lastProcessedLocked(m *fix2.Message, in Inbound) {
	if s.cfg.Retention.Interval <= 0 || in.PossDup {
		return
	}
	v := m.Get(tagLastMsgSeqNumProcessed)
	if v == nil {
		return
	}
	if n, err := fix2.ParseInt(v); err == nil && n >= 0 {
		s.ackedLocked(m, uint64(n), tagLastMsgSeqNumProcessed, in.Seq)
	}
}

// storedLocked holds back the messages the store had when the session
// started: they may never have been written.
func (s *Session) storedLocked() {
	if s.cfg.Retention.Interval > 0 && s.st.NextSender > 1 {
		s.ret.holdLocked(1, s.st.NextSender-1)
	}
}

// evictLocked evicts from the store the messages Config.Retention no longer
// keeps, at time now. It runs under the session's lock, as Append does: a
// ResendRequest is either served before or sees the eviction whole. Since a
// store may copy what it keeps when it truncates, it evicts once at least an
// eighth of what it keeps can go.
func (s *Session) evictLocked(now time.Time) {
	t, ok := s.store.(store.Truncater)
	if !ok || s.halted != nil || s.resetting {
		return
	}
	before := s.evictBeforeLocked(now)
	if before <= s.ret.floor || before-s.ret.floor < (s.st.NextSender-before)/8 {
		return
	}
	if err := t.Truncate(context.Background(), s.st.Generation, before); err != nil {
		if diverged(err) {
			s.haltLocked(fmt.Errorf("evicting messages before %d: %w", before, err))
			return
		}
		// An outcome unknown alone leaves at worst messages evicted that the
		// session still counts as kept: a replay finds their holes.
		if !s.ret.failing {
			s.ret.failing = true
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "evicting messages from the store failed",
				slog.Uint64("before", before), slog.Any("error", err))
		}
		return
	}
	s.ret.failing = false
	s.stats.Evicted.Add(before - s.ret.floor)
	s.log.LogAttrs(context.Background(), slog.LevelDebug, "messages evicted from the store",
		slog.Uint64("from", s.ret.floor), slog.Uint64("to", before-1), slog.Uint64("acked", s.ret.acked), slog.Bool("ack_known", s.ret.ackKnown))
	s.ret.floor = before
}

// evictBeforeLocked returns the MsgSeqNum of the first message to keep at
// time now: the messages acknowledged, beyond the last Keep and older than
// MinAge, go; beyond Max, all go. Those not yet written, and those a
// ResendRequest waits for, stay.
func (s *Session) evictBeforeLocked(now time.Time) uint64 {
	r := &s.cfg.Retention
	next := s.st.NextSender
	below := func(n int) uint64 {
		if next > uint64(n) {
			return next - uint64(n)
		}
		return 1
	}
	before := uint64(1)
	if s.ret.ackKnown {
		before = s.ret.acked + 1
	}
	before = min(before, below(r.Keep))
	if r.MinAge > 0 {
		before = min(before, s.agedLocked(now))
	}
	if r.Max > 0 {
		before = max(before, below(r.Max))
	}
	if s.ret.unsentLo != 0 {
		before = min(before, s.ret.unsentLo)
	}
	if c := s.c; c != nil {
		if lo := c.unwrittenLocked(); lo != 0 {
			before = min(before, lo)
		}
	}
	return before
}

// agedLocked returns the MsgSeqNum below which every message was stored at
// least MinAge before now, as far as the marks tell, and marks now.
func (s *Session) agedLocked(now time.Time) uint64 {
	marks := s.ret.marks
	if n := len(marks); n == 0 || marks[n-1].next != s.st.NextSender {
		// A mark of the same NextSender, older, already tells more.
		marks = append(marks, retentionMark{at: now, next: s.st.NextSender})
	}
	aged, i := uint64(1), -1
	for k, m := range marks {
		if now.Sub(m.at) < s.cfg.Retention.MinAge {
			break
		}
		aged, i = m.next, k
	}
	if i > 0 {
		// The marks before the one used are older still: no longer needed.
		marks = append(marks[:0], marks[i:]...)
	}
	if n := len(marks); n > maxRetentionMarks {
		k := 0
		for j := 0; j < n-1; j += 2 {
			marks[k] = marks[j]
			k++
		}
		marks[k] = marks[n-1]
		marks = marks[:k+1]
	}
	s.ret.marks = marks
	return aged
}

// unwrittenLocked returns the lowest MsgSeqNum of the application messages c
// has not completely written yet, 0 if none: those queued, being written, or
// asked for by a ResendRequest not yet completely served, which a replay took
// out of the queue. Session-level messages do not count: a replay gap-fills
// them.
func (c *conn) unwrittenLocked() uint64 {
	lo := c.writing
	low := func(seq uint64) {
		if lo == 0 || seq < lo {
			lo = seq
		}
	}
	if c.replay != nil {
		low(c.replay.begin)
	}
	for _, q := range c.replayQueue {
		low(q.begin)
	}
	for _, frames := range [][]pendingFrame{c.frames, c.heldFrames} {
		for _, f := range frames {
			if !f.admin && !f.gapFill {
				low(f.seq)
			}
		}
	}
	return lo
}
