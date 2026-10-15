package session

import (
	"cmp"
	"context"
	"slices"
	"time"
)

// Level is how far a sent message got.
type Level int

const (
	// Accepted: sequenced and stored.
	Accepted Level = iota
	// Written: completely written to the connection.
	Written
	// Acknowledged: the counterparty's engine provably read it. The session
	// sends a TestRequest after the message and, when the Heartbeat
	// answering it comes back on the same connection, everything written
	// before the TestRequest was read: TCP keeps the order. It proves the
	// message was read, not that it was accepted or processed: business
	// acknowledgements (an ExecutionReport for an order) stay the
	// application's.
	Acknowledged
)

func (l Level) String() string {
	return [...]string{"accepted", "written", "acknowledged"}[l]
}

// Receipt tracks a sent message. It is a small value: copy it freely.
type Receipt struct {
	s   *Session
	gen uint64
	seq uint64
}

// Seq returns the message's MsgSeqNum.
func (r Receipt) Seq() uint64 { return r.seq }

// Generation returns the sequence number generation the message belongs to.
func (r Receipt) Generation() uint64 { return r.gen }

// maxRejections bounds the rejections kept, by the messages sent they are
// for, and maxRejectText the bytes of their Text: a counterparty sending
// them could hold memory otherwise.
const (
	maxRejections = 16384
	maxRejectText = 1024
)

// RejectInfo describes a Reject or BusinessMessageReject the counterparty
// sent for a message.
type RejectInfo struct {
	MsgType  string // "3" for a Reject, "j" for a BusinessMessageReject
	Reason   int    // SessionRejectReason (373) or BusinessRejectReason (380)
	RefTagID int
	Text     string
}

// Wait waits until the message reaches level, or can never reach it, or ctx
// is done. It returns nil once the level is reached, and keeps doing so;
// ErrSkipped, ErrUnknownDelivery or ErrReceiptUntracked if it never will;
// ctx's error; or ErrWaitInHandler, at once, with a handler callback's ctx.
// Waiting for Acknowledged makes the session probe the counterparty (see
// Config.AckProbe). A sequence reset keeps what the message reached, as long
// as its receipt is tracked: a message written stays written, one gap-filled
// stays skipped; one not acknowledged by then never will be, and fails with
// ErrUnknownDelivery.
func (r Receipt) Wait(ctx context.Context, level Level) error {
	if r.s == nil {
		return ErrReceiptUntracked
	}
	if level == Accepted {
		return nil
	}
	if inCallback(ctx) {
		return ErrWaitInHandler
	}
	for {
		done, err, wake := r.s.receiptState(r, level, true)
		if done {
			return err
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Err returns, without waiting, ErrSkipped, ErrUnknownDelivery or
// ErrReceiptUntracked if the message can never be acknowledged, nil
// otherwise.
func (r Receipt) Err() error {
	if r.s == nil {
		return ErrReceiptUntracked
	}
	done, err, _ := r.s.receiptState(r, Acknowledged, false)
	if done {
		return err
	}
	return nil
}

// Rejection returns the Reject or BusinessMessageReject the counterparty
// sent for the message, if any so far, and if it is one of the last 16384
// sent (fewer if Config.MaxTrackedReceipts is): Text holds the first 1 KiB
// of its Text (58). A rejection does not change what Wait reports: a
// message can be read, then rejected. A sequence reset keeps it, as Wait
// keeps what the message reached.
func (r Receipt) Rejection() (RejectInfo, bool) {
	if r.s == nil {
		return RejectInfo{}, false
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	t := &r.s.receipts
	if r.gen != t.gen {
		if e := t.endedGen(r.gen); e != nil {
			ri, ok := e.rejections[r.seq]
			return ri, ok
		}
		return RejectInfo{}, false
	}
	ri, ok := t.rejections[r.seq]
	return ri, ok
}

// receiptState tells whether receipt r reached level: done reports whether
// it is settled, err why it never will. Otherwise wake is closed on the next
// change. probe asks for an acknowledgement probe. Evidence obtained always
// wins: a level reached stays reached.
func (s *Session) receiptState(r Receipt, level Level, probe bool) (done bool, err error, wake <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &s.receipts
	if r.gen != t.gen {
		if e := t.endedGen(r.gen); e != nil {
			return true, e.state(r.seq, level), nil
		}
		// A generation ended so long ago it was dropped.
		return true, ErrReceiptUntracked, nil
	}
	rec, tracked := t.recs[r.seq]
	switch {
	case level == Written && rec.epoch != 0:
		return true, nil, nil
	case rec.acked:
		return true, nil, nil
	case skipped(t.skips, r.seq):
		return true, ErrSkipped, nil
	case !tracked && r.seq < t.oldest:
		return true, ErrReceiptUntracked, nil
	case s.stopped || s.halted != nil:
		return true, ErrUnknownDelivery, nil
	}
	if level == Acknowledged && probe && s.cfg.AckProbe != AckProbeNever && s.c != nil && rec.epoch == s.c.epoch {
		s.probeLocked(s.c)
	}
	return false, nil, t.changed()
}

// record is what the session knows of a sent message.
type record struct {
	epoch uint64 // connection its last complete write was on, 0 if not written
	wq    uint64 // write counter of that write
	acked bool
}

// span is a range of MsgSeqNums, end excluded.
type span struct{ begin, end uint64 }

// maxSkipped bounds the gap-filled spans remembered.
const maxSkipped = 1024

// maxGenerations bounds the earlier generations whose receipts are
// remembered: a counterparty resetting the sequence numbers at every logon
// could hold memory otherwise, each generation kept costing some even with
// nothing in it.
const maxGenerations = 1024

// ended is what the receipts of a generation that ended had reached, for
// them to keep reporting it.
type ended struct {
	gen        uint64
	written    []uint64 // MsgSeqNums written, not acknowledged, in order
	acked      []uint64 // MsgSeqNums acknowledged, in order
	oldest     uint64   // lowest MsgSeqNum still tracked when it ended
	skips      []span
	rejections map[uint64]RejectInfo
}

// state tells what Wait reports for message seq of e at level: what it did
// before e ended, a message not settled by then failing with
// ErrUnknownDelivery.
func (e *ended) state(seq uint64, level Level) error {
	if _, ok := slices.BinarySearch(e.acked, seq); ok {
		return nil
	}
	_, written := slices.BinarySearch(e.written, seq)
	switch {
	case written && level == Written:
		return nil
	case skipped(e.skips, seq):
		return ErrSkipped
	case !written && seq < e.oldest:
		// A late write of a connection replaced can track a message below
		// oldest again.
		return ErrReceiptUntracked
	}
	return ErrUnknownDelivery
}

// receipts tracks sent messages for their Receipts.
type receipts struct {
	max        int
	gen        uint64
	recs       map[uint64]record
	oldest     uint64 // lowest MsgSeqNum still tracked
	rejections map[uint64]RejectInfo
	skips      []span // gap-filled by us, in order
	// wq counts complete writes on connection wqEpoch; probes maps the
	// TestReqID of each probe written to the write counter of its own
	// write. pending is the probe queued and not yet written, answered
	// whether its answer came first.
	wq       uint64
	wqEpoch  uint64
	probes   map[string]uint64
	pending  string
	answered bool
	lastAt   time.Time // when the last probe was queued
	// ended holds what the receipts of earlier generations reached, the
	// oldest generation first. When what they hold together exceeds any
	// bound of one generation, receipts, gap-filled spans or rejections, or
	// maxGenerations, the oldest are dropped whole: many rejections or
	// gap-fills can make an earlier generation forget sooner the messages it
	// wrote or had acknowledged.
	ended []ended
	wake  chan struct{}
}

func (t *receipts) init(max int, gen uint64) {
	t.max = max
	t.gen = gen
	t.recs = make(map[uint64]record)
	t.rejections = make(map[uint64]RejectInfo)
	t.probes = make(map[string]uint64)
	t.oldest = 1
}

// endedGen returns what is kept of generation gen, ended, nil if nothing.
// Generations end in increasing order: resetStoreLocked sees to it.
func (t *receipts) endedGen(gen uint64) *ended {
	i, ok := slices.BinarySearchFunc(t.ended, gen, func(e ended, gen uint64) int { return cmp.Compare(e.gen, gen) })
	if !ok {
		return nil
	}
	return &t.ended[i]
}

// changed returns a channel closed on the next change.
func (t *receipts) changed() <-chan struct{} {
	if t.wake == nil {
		t.wake = make(chan struct{})
	}
	return t.wake
}

func (t *receipts) notify() {
	if t.wake != nil {
		close(t.wake)
		t.wake = nil
	}
}

// skipped reports whether skips, the spans we gap-filled, hold seq.
func skipped(skips []span, seq uint64) bool {
	for i := len(skips) - 1; i >= 0; i-- {
		if s := skips[i]; seq >= s.begin && seq < s.end {
			return true
		}
	}
	return false
}

// written records the complete write of message seq of generation gen on
// connection epoch.
func (t *receipts) written(gen, seq, epoch uint64) {
	if gen != t.gen {
		return
	}
	if epoch < t.wqEpoch {
		// A late write of a connection already replaced: written, but no
		// probe of the new connection covers it.
		rec := t.recs[seq]
		rec.epoch, rec.wq = epoch, 0
		t.recs[seq] = rec
		t.notify()
		return
	}
	t.tick(epoch)
	rec := t.recs[seq]
	rec.epoch, rec.wq = epoch, t.wq
	t.recs[seq] = rec
	if len(t.recs) > t.max {
		t.evict()
	}
	t.notify()
}

// tick counts a complete write on epoch; writes of earlier connections are
// not counted.
func (t *receipts) tick(epoch uint64) {
	if epoch < t.wqEpoch {
		return
	}
	if t.wqEpoch != epoch {
		t.wq, t.wqEpoch = 0, epoch
		clear(t.probes)
	}
	t.wq++
}

// probeWritten records the complete write of probe id on epoch: if its
// answer already came, the messages before it are acknowledged now.
func (t *receipts) probeWritten(id string, epoch uint64) {
	if epoch < t.wqEpoch {
		return // a probe of a connection already replaced
	}
	t.tick(epoch)
	if t.pending == id {
		t.pending = ""
		if t.answered {
			t.answered = false
			t.ack(epoch, t.wq)
			return
		}
	}
	t.probes[id] = t.wq
}

// probeAnswered acknowledges the messages written before probe id on epoch,
// if id is one of our probes; if its write is not accounted for yet, it is
// once it is.
func (t *receipts) probeAnswered(epoch uint64, id []byte) {
	if string(id) == t.pending {
		t.answered = true
		return
	}
	wq, ok := t.probes[string(id)]
	if !ok || epoch != t.wqEpoch {
		return
	}
	delete(t.probes, string(id))
	t.ack(epoch, wq)
}

// ack acknowledges the messages whose last complete write was on epoch
// before write counter wq.
func (t *receipts) ack(epoch, wq uint64) {
	for seq, rec := range t.recs {
		if rec.epoch == epoch && rec.wq < wq && !rec.acked {
			rec.acked = true
			t.recs[seq] = rec
		}
	}
	t.notify()
}

// connectionLost forgets the probes of a connection that ended.
func (t *receipts) connectionLost() {
	clear(t.probes)
	t.pending, t.answered = "", false
	t.notify()
}

// rejected records a rejection of message seq, if it is one of the last
// maxRejections sent before next: the counterparty names the message, any
// number, and writes Text, up to 1 MiB, of which maxRejectText is kept.
func (t *receipts) rejected(gen, seq, next uint64, ri RejectInfo) {
	window := uint64(min(t.max, maxRejections))
	if gen != t.gen || seq >= next || seq+window < next {
		return
	}
	t.rejections[seq] = ri
	if len(t.rejections) > int(window) {
		for s := range t.rejections {
			if s+window < next {
				delete(t.rejections, s)
			}
		}
	}
	t.notify()
}

// gapFilled records that we gap-filled messages begin to end-1.
func (t *receipts) gapFilled(gen, begin, end uint64) {
	if gen != t.gen || begin >= end {
		return
	}
	if n := len(t.skips); n > 0 && t.skips[n-1].end >= begin && t.skips[n-1].begin <= begin {
		t.skips[n-1].end = max(t.skips[n-1].end, end)
	} else {
		t.skips = append(t.skips, span{begin, end})
	}
	if len(t.skips) > maxSkipped {
		t.skips = append(t.skips[:0], t.skips[len(t.skips)-maxSkipped:]...)
	}
	t.notify()
}

// newGeneration ends generation gen, starting generation next: its receipts
// keep what they reached, written, acknowledged, gap-filled or rejected: a
// level reached stays reached. The others fail.
func (t *receipts) newGeneration(gen, next uint64) {
	// Two sorted slices sharing one array, rather than maps: 8 bytes a
	// message, for up to max of them.
	seqs := make([]uint64, len(t.recs))
	w, a := 0, len(seqs)
	for seq, rec := range t.recs {
		switch {
		case rec.acked:
			a--
			seqs[a] = seq
		case rec.epoch != 0:
			seqs[w] = seq
			w++
		}
	}
	e := ended{gen: gen, written: seqs[:w:w], acked: seqs[a:], oldest: t.oldest, skips: slices.Clone(t.skips)}
	slices.Sort(e.written)
	slices.Sort(e.acked)
	if len(t.rejections) > 0 {
		e.rejections = t.rejections
		t.rejections = make(map[uint64]RejectInfo)
	}
	t.ended = append(t.ended, e)
	// Bound what is kept of earlier generations as one generation is, the
	// oldest dropped first, the generation just ended always kept.
	var recs, skips, rejections int
	for _, e := range t.ended {
		recs += len(e.written) + len(e.acked)
		skips += len(e.skips)
		rejections += len(e.rejections)
	}
	window := min(t.max, maxRejections)
	drop := 0
	for ; drop < len(t.ended)-1; drop++ {
		if recs <= t.max && skips <= maxSkipped && rejections <= window && len(t.ended)-drop <= maxGenerations {
			break
		}
		e := &t.ended[drop]
		recs -= len(e.written) + len(e.acked)
		skips -= len(e.skips)
		rejections -= len(e.rejections)
	}
	t.ended = slices.Delete(t.ended, 0, drop)
	t.gen = next
	clear(t.recs)
	t.skips = t.skips[:0]
	t.oldest = 1
	t.connectionLost()
}

// evict drops the oldest records beyond max.
func (t *receipts) evict() {
	var newest uint64
	for seq := range t.recs {
		newest = max(newest, seq)
	}
	limit := newest - uint64(t.max/2)
	for seq := range t.recs {
		if seq < limit {
			delete(t.recs, seq)
		}
	}
	t.oldest = limit
}

// probeLocked queues a TestRequest probing for acknowledgements, unless one
// is queued or unanswered, or the last was queued less than
// AckProbeMinInterval ago, in which case the timer sends it when due.
func (s *Session) probeLocked(c *conn) {
	t := &s.receipts
	if t.pending != "" || len(t.probes) > 0 || c.replaying() || s.resetting || c.closed {
		return
	}
	if min := s.cfg.AckProbeMinInterval; min > 0 && time.Since(t.lastAt) < min {
		c.probeWanted = true
		s.armLocked(c)
		return
	}
	c.probeWanted = false
	c.testReqs++
	id := "ACK-" + itoa(c.epoch) + "-" + itoa(c.testReqs)
	t.pending, t.answered, t.lastAt = id, false, time.Now()
	if _, err := s.queueProbeLocked(id); err != nil {
		t.pending = ""
	}
}
