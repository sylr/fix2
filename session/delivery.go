package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	fix2 "github.com/sylr/fix2"
)

// itemKind is what a delivery item carries.
type itemKind uint8

const (
	itemApp    itemKind = iota // an application message for FromApp
	itemMark                   // received messages up to seq processed, nothing to deliver
	itemLogon                  // OnLogon
	itemLogout                 // OnLogout
	itemLoss                   // OnRecoveryLoss for seq to end
)

// item is a unit of work for the delivery goroutine, in the order the
// session produced them.
type item struct {
	kind  itemKind
	gen   uint64
	seq   uint64
	end   uint64
	frame []byte // itemApp: the message, a pooled copy
	in    Inbound
	err   error
}

// delivery delivers received messages and events to the handler, in order,
// from a goroutine of its own, and records the processed prefix: the
// MsgSeqNum up to which every received message was processed.
//
// push never blocks, so that it may be called with the session's lock held:
// the reader waits for room before reading the next message instead.
type delivery struct {
	s *Session

	mu    sync.Mutex
	items []item
	head  int
	room  chan struct{} // closed when the queue has room again
	wake  chan struct{}
	stop  bool
	done  chan struct{}
	idle  chan struct{} // closed when the queue is empty and nothing runs
	busy  bool

	pool sync.Pool
	msg  fix2.Message
	// persisted is the NextTarget last recorded in the store, for
	// generation persistedGen.
	persisted, persistedGen uint64
}

func newDelivery(s *Session) *delivery {
	d := &delivery{s: s, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go d.run()
	return d
}

// push queues it.
func (d *delivery) push(it item) {
	d.mu.Lock()
	d.items = append(d.items, it)
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// pending returns the number of items queued.
func (d *delivery) pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.items) - d.head
}

// copyFrame returns a pooled copy of frame.
func (d *delivery) copyFrame(frame []byte) []byte {
	b, _ := d.pool.Get().([]byte)
	return append(b[:0], frame...)
}

// roomSignal returns a channel closed once the queue has room.
func (d *delivery) roomSignal() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.items)-d.head < d.s.cfg.DeliveryQueue {
		c := make(chan struct{})
		close(c)
		return c
	}
	if d.room == nil {
		d.room = make(chan struct{})
	}
	return d.room
}

// waitIdle returns a channel closed once every queued item is processed.
func (d *delivery) waitIdle() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.items) == d.head && !d.busy {
		c := make(chan struct{})
		close(c)
		return c
	}
	if d.idle == nil {
		d.idle = make(chan struct{})
	}
	return d.idle
}

// close stops the goroutine once the queue is processed.
func (d *delivery) close() {
	d.mu.Lock()
	d.stop = true
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// waitDeliveryRoom makes the reader wait while the delivery queue is full:
// TCP then slows the counterparty down. The liveness checks are suspended
// meanwhile, for at most MaxDeliveryStall (see onTimer).
func (s *Session) waitDeliveryRoom(c *conn) {
	d := s.deliv
	if d.pending() < s.cfg.DeliveryQueue {
		return
	}
	s.mu.Lock()
	c.readBlocked = time.Now()
	s.armLocked(c)
	s.mu.Unlock()
	select {
	case <-d.roomSignal():
	case <-c.done():
	}
	s.mu.Lock()
	c.readBlocked = time.Time{}
	c.lastRead = time.Now()
	if !c.closed {
		s.armLocked(c)
	}
	s.mu.Unlock()
}

// run processes the queue until close.
func (d *delivery) run() {
	defer close(d.done)
	for {
		d.mu.Lock()
		for len(d.items) == d.head && !d.stop {
			if d.idle != nil {
				close(d.idle)
				d.idle = nil
			}
			d.mu.Unlock()
			d.persist(true)
			<-d.wake
			d.mu.Lock()
		}
		if len(d.items) == d.head {
			d.mu.Unlock()
			d.persist(true)
			return
		}
		it := d.items[d.head]
		d.items[d.head] = item{}
		d.head++
		switch {
		case d.head == len(d.items):
			d.items, d.head = d.items[:0], 0
		case d.head >= 1024 && 2*d.head >= len(d.items):
			// Compact: the queue never stays empty under sustained traffic.
			n := copy(d.items, d.items[d.head:])
			clear(d.items[n:])
			d.items, d.head = d.items[:n], 0
		}
		d.busy = true
		if d.room != nil && len(d.items)-d.head < d.s.cfg.DeliveryQueue {
			close(d.room)
			d.room = nil
		}
		d.mu.Unlock()

		d.process(it)

		d.mu.Lock()
		d.busy = false
		d.mu.Unlock()
		d.s.unstall()
	}
}

// unstall lets a session stalled by a stuck callback connect again, now
// that the callback returned.
func (s *Session) unstall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stalled {
		s.stalled = false
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "handler unstalled: the session may connect again")
	}
}

// process runs item it.
func (d *delivery) process(it item) {
	s := d.s
	ctx := s.callbackCtx()
	switch it.kind {
	case itemLogon:
		s.handler.OnLogon(ctx, s)
		return
	case itemLogout:
		s.handler.OnLogout(ctx, s, it.err)
		return
	case itemLoss:
		s.log.LogAttrs(context.Background(), slog.LevelError, "messages may never be delivered",
			slog.Uint64("from", it.seq), slog.Uint64("to", it.end), slog.Any("cause", it.err))
		s.handler.OnRecoveryLoss(ctx, s, it.seq, it.end, it.err)
		return
	case itemApp:
		d.deliver(ctx, it)
		d.pool.Put(it.frame[:0]) //nolint:staticcheck // a slice header is small
	}
	s.mu.Lock()
	if it.gen == s.st.Generation && it.seq > s.committed {
		s.committed = it.seq
	}
	s.mu.Unlock()
	d.persist(false)
}

// deliver calls FromApp for an application message, and rejects it if the
// handler says so.
func (d *delivery) deliver(ctx context.Context, it item) {
	s := d.s
	m := &d.msg
	p := s.dicts.parse
	if v := it.in.ApplVerID; v != "" && s.dicts.parseApps != nil && s.dicts.parseApps[v] != nil {
		p = s.dicts.parseApps[v]
	}
	if err := fix2.ParseInto(m, it.frame, p); err != nil {
		return
	}
	err := s.handler.FromApp(ctx, s, m, it.in)
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil || it.gen != s.st.Generation {
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "handler rejected a message after the connection or sequence changed: not sent",
			slog.Uint64("seq", it.seq), slog.Any("error", err))
		return
	}
	var rej *RejectError
	var brej *BusinessRejectError
	switch {
	case errors.As(err, &rej):
		tag := rej.Tag
		if tag == 0 {
			tag = noTag
		}
		s.rejectLocked(m, rej.Reason, tag, rejectText(rej.Reason, rej.Text))
	case errors.As(err, &brej):
		s.businessRejectLocked(m, brej.Reason, brej.RefID, brej.Text)
	default:
		s.businessRejectLocked(m, BusinessRejectOther, "", err.Error())
	}
}

// persist records the processed prefix in the store: always if force, else
// every 64 messages.
func (d *delivery) persist(force bool) {
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	d.persistLocked(force)
}

// idleNow reports whether nothing is queued nor being delivered.
func (d *delivery) idleNow() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.items) == d.head && !d.busy
}

// persistLocked is persist with the session's lock held.
func (d *delivery) persistLocked(force bool) {
	s := d.s
	if d.persistedGen != s.st.Generation {
		d.persisted, d.persistedGen = 0, s.st.Generation
	}
	next := s.committed + 1
	if next == d.persisted || !force && next < d.persisted+64 {
		return
	}
	if err := s.store.SetNextTarget(context.Background(), s.st.Generation, next); err != nil {
		s.log.LogAttrs(context.Background(), slog.LevelError, "recording received messages processed failed",
			slog.Uint64("next_target", next), slog.Any("error", err))
		return
	}
	d.persisted = next
}

// rejectInfo describes a Reject or BusinessMessageReject received.
func rejectInfo(m *fix2.Message) RejectInfo {
	r := RejectInfo{MsgType: string(m.MsgType()), Text: string(m.Get(tagText))}
	if v, err := fix2.ParseInt(m.Get(tagSessionRejectReason)); err == nil {
		r.Reason = v
	}
	if v, err := fix2.ParseInt(m.Get(tagBusinessRejectReason)); err == nil {
		r.Reason = v
	}
	if v, err := fix2.ParseInt(m.Get(tagRefTagID)); err == nil {
		r.RefTagID = v
	}
	return r
}
