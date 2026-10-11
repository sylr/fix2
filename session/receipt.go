package session

import (
	"context"
	"sync"
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

// RejectInfo describes a Reject or BusinessMessageReject the counterparty
// sent for a message.
type RejectInfo struct {
	MsgType  string // "3" for a Reject, "j" for a BusinessMessageReject
	Reason   int    // SessionRejectReason (373) or BusinessRejectReason (380)
	RefTagID int
	Text     string
}

// Wait waits until the message reaches level, or can never reach it, or ctx
// is done. It returns nil once the level is reached; ErrSkipped,
// ErrUnknownDelivery or ErrReceiptUntracked if it never will; ctx's error;
// or ErrWaitInHandler, at once, with a handler callback's ctx. Waiting for
// Acknowledged makes the session probe the counterparty (see
// Config.AckProbe).
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
// sent for the message, if any so far. A rejection does not change what
// Wait reports: a message can be read, then rejected.
func (r Receipt) Rejection() (RejectInfo, bool) {
	if r.s == nil {
		return RejectInfo{}, false
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.gen != r.s.receipts.gen {
		return RejectInfo{}, false
	}
	ri, ok := r.s.receipts.rejections[r.seq]
	return ri, ok
}

// receiptState tells whether receipt r reached level: done reports whether
// it is settled, err why it never will. Otherwise wake is closed on the next
// change. probe asks for an acknowledgement probe.
func (s *Session) receiptState(r Receipt, level Level, probe bool) (done bool, err error, wake <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &s.receipts
	switch {
	case r.gen < t.gen:
		if rec, ok := t.oldAcked[r.gen]; ok && r.seq <= rec {
			return true, nil, nil
		}
		return true, ErrUnknownDelivery, nil
	case s.stopped:
		return true, ErrUnknownDelivery, nil
	}
	rec, ok := t.recs[r.seq]
	if !ok {
		if r.seq < t.oldest {
			return true, ErrReceiptUntracked, nil
		}
		rec = record{}
	}
	if rec.skipped && !rec.acked {
		return true, ErrSkipped, nil
	}
	switch level {
	case Written:
		if rec.epoch != 0 {
			return true, nil, nil
		}
	case Acknowledged:
		if rec.acked {
			return true, nil, nil
		}
		if probe && rec.epoch != 0 && s.c != nil && rec.epoch == s.c.epoch && s.cfg.AckProbe != AckProbeNever {
			s.probeLocked(s.c)
		}
	}
	return false, nil, t.changed()
}

// record is what the session knows of a sent message.
type record struct {
	epoch   uint64 // connection its last complete write was on, 0 if not written
	wq      uint64 // write counter of that write
	acked   bool
	skipped bool // gap-filled by us
}

// receipts tracks sent messages for their Receipts.
type receipts struct {
	max        int
	gen        uint64
	recs       map[uint64]record
	oldest     uint64 // lowest MsgSeqNum still tracked
	rejections map[uint64]RejectInfo
	// wq counts complete writes on the current connection; probes maps the
	// TestReqID of our probes to the write counter of their own write, and
	// probeAt is the counter at which a probe was last queued.
	wq       uint64
	wqEpoch  uint64
	probes   map[string]uint64
	pending  string // TestReqID of the probe queued, not yet written
	oldAcked map[uint64]uint64
	wake     chan struct{}
	once     sync.Once
}

func (t *receipts) init(max int) {
	t.max = max
	t.recs = make(map[uint64]record)
	t.rejections = make(map[uint64]RejectInfo)
	t.probes = make(map[string]uint64)
	t.oldAcked = make(map[uint64]uint64)
	t.oldest = 1
	t.gen = 1
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

// written records the complete write of message seq of generation gen on
// connection epoch.
func (t *receipts) written(gen, seq, epoch uint64) {
	if gen != t.gen {
		return
	}
	if t.wqEpoch != epoch {
		t.wq, t.wqEpoch = 0, epoch
	}
	t.wq++
	rec := t.recs[seq]
	rec.epoch, rec.wq = epoch, t.wq
	t.recs[seq] = rec
	if len(t.recs) > t.max {
		t.evict()
	}
	t.notify()
}

// probeWritten records the complete write of probe id on epoch.
func (t *receipts) probeWritten(id string, epoch uint64) {
	if t.wqEpoch != epoch {
		t.wq, t.wqEpoch = 0, epoch
	}
	t.wq++
	t.probes[id] = t.wq
	if t.pending == id {
		t.pending = ""
	}
}

// probeAnswered acknowledges the messages written before probe id on epoch,
// if id is one of our probes.
func (t *receipts) probeAnswered(epoch uint64, id []byte) {
	wq, ok := t.probes[string(id)]
	if !ok || epoch != t.wqEpoch {
		return
	}
	delete(t.probes, string(id))
	for seq, rec := range t.recs {
		if rec.epoch == epoch && rec.wq < wq && !rec.acked {
			rec.acked = true
			t.recs[seq] = rec
		}
	}
	t.notify()
}

// rejected records a rejection of message seq.
func (t *receipts) rejected(gen, seq uint64, ri RejectInfo) {
	if gen != t.gen {
		return
	}
	t.rejections[seq] = ri
	if len(t.rejections) > t.max {
		for s := range t.rejections {
			if s < seq-uint64(t.max) {
				delete(t.rejections, s)
			}
		}
	}
	t.notify()
}

// gapFilled records that we gap-filled messages begin to end-1.
func (t *receipts) gapFilled(gen, begin, end uint64) {
	if gen != t.gen {
		return
	}
	for seq := begin; seq < end; seq++ {
		if rec, ok := t.recs[seq]; ok {
			rec.skipped = true
			t.recs[seq] = rec
		} else if seq >= t.oldest {
			t.recs[seq] = record{skipped: true}
		}
	}
	t.notify()
}

// newGeneration ends generation gen: its receipts without evidence fail.
func (t *receipts) newGeneration(gen uint64) {
	var acked uint64
	for seq, rec := range t.recs {
		if rec.acked && seq > acked {
			acked = seq
		}
	}
	t.oldAcked[gen] = acked
	t.gen = gen + 1
	clear(t.recs)
	clear(t.rejections)
	clear(t.probes)
	t.oldest = 1
	t.notify()
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
// is already queued or unanswered.
func (s *Session) probeLocked(c *conn) {
	t := &s.receipts
	if t.pending != "" || len(t.probes) > 0 || c.replaying() {
		return
	}
	c.testReqs++
	id := "ACK-" + itoa(c.epoch) + "-" + itoa(c.testReqs)
	t.pending = id
	if _, err := s.queueProbeLocked(id); err != nil {
		t.pending = ""
	}
}
