package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

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
	// withheld tells, for itemLoss, that its end, worked out from a
	// MsgSeqNum received, may show a credential: it is not logged.
	withheld bool
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
	size  int           // the bytes the queued items' frames hold, their capacity
	room  chan struct{} // closed when the queue has room again
	wake  chan struct{}
	stop  bool
	done  chan struct{}
	idle  chan struct{} // closed when the queue is empty and nothing runs
	busy  bool
	// busyMsg tells the item running is a received message: an application
	// message or a mark, not a notification such as OnLogon.
	busyMsg bool
	// held is the generation recording the received messages processed
	// failed for, 0 if none: its messages queued, and their losses, wait
	// until it works again (see persistLocked). Those of an earlier
	// generation, and notifications, go past them.
	held uint64

	pool sync.Pool
	msg  fix2.Message
	// persisted is the NextTarget last recorded in the store, for
	// generation persistedGen; failed tells recording it failed since.
	// Guarded by the session's lock.
	persisted, persistedGen uint64
	failed                  bool
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
	d.size += cap(it.frame)
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// full reports whether the queue holds DeliveryQueue items, or
// DeliveryQueueBytes of frames.
func (d *delivery) full() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fullLocked()
}

func (d *delivery) fullLocked() bool {
	n := len(d.items) - d.head
	return n >= d.s.cfg.DeliveryQueue || n > 0 && d.size >= d.s.cfg.DeliveryQueueBytes
}

// copyFrame returns a pooled copy of frame.
func (d *delivery) copyFrame(frame []byte) []byte {
	var b []byte
	if p, _ := d.pool.Get().(*[]byte); p != nil {
		b = *p
	}
	return append(b[:0], frame...)
}

// releaseFrame returns frame, from copyFrame, to the pool, unless it is
// larger than maxPooledBuffer.
func (d *delivery) releaseFrame(frame []byte) {
	if cap(frame) > maxPooledBuffer {
		return
	}
	frame = frame[:0]
	d.pool.Put(&frame)
}

// roomSignal returns a channel closed once the queue has room.
func (d *delivery) roomSignal() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.fullLocked() {
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
	if !d.full() {
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
		next := d.nextLocked()
		for next < 0 && !d.stop {
			if len(d.items) == d.head && d.idle != nil {
				close(d.idle)
				d.idle = nil
			}
			d.mu.Unlock()
			// Records what was processed; held, tries again.
			d.persist(true)
			<-d.wake
			d.mu.Lock()
			next = d.nextLocked()
		}
		if len(d.items) == d.head {
			d.mu.Unlock()
			d.persist(true)
			return
		}
		if next < 0 {
			// Stopping while held: what was received is delivered, to be
			// delivered again after a restart rather than lost.
			next = d.head
		}
		it := d.items[next]
		d.size -= cap(it.frame)
		if next != d.head {
			// A notification going past held messages.
			copy(d.items[next:], d.items[next+1:])
			d.items[len(d.items)-1] = item{}
			d.items = d.items[:len(d.items)-1]
		} else {
			d.items[d.head] = item{}
			d.head++
		}
		switch {
		case d.head == len(d.items):
			d.items, d.head = d.items[:0], 0
		case d.head >= 1024 && 2*d.head >= len(d.items):
			// Compact: the queue never stays empty under sustained traffic.
			n := copy(d.items, d.items[d.head:])
			clear(d.items[n:])
			d.items, d.head = d.items[:n], 0
		}
		d.busy, d.busyMsg = true, it.kind == itemApp || it.kind == itemMark
		if d.room != nil && !d.fullLocked() {
			close(d.room)
			d.room = nil
		}
		d.mu.Unlock()

		d.process(it)

		d.mu.Lock()
		d.busy, d.busyMsg = false, false
		d.mu.Unlock()
		d.s.unstall()
	}
}

// nextLocked returns the index of the next item to process, -1 if none: the
// first queued unless it is a received message held, then the first
// notification.
func (d *delivery) nextLocked() int {
	if len(d.items) == d.head {
		return -1
	}
	switch it := &d.items[d.head]; {
	case d.held == 0, it.gen != d.held, it.kind == itemLogon || it.kind == itemLogout:
		return d.head
	}
	for i := d.head; i < len(d.items); i++ {
		if k := d.items[i].kind; k == itemLogon || k == itemLogout {
			return i
		}
	}
	return -1
}

// holdLocked holds the delivery of the received messages of generation gen,
// or releases what is held if gen is 0.
func (d *delivery) holdLocked(gen uint64) {
	d.mu.Lock()
	release := d.held != 0 && gen == 0
	d.held = gen
	d.mu.Unlock()
	if release {
		select {
		case d.wake <- struct{}{}:
		default:
		}
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
		to := slog.Uint64("to", it.end)
		if it.withheld {
			to = slog.String("to", "withheld")
		}
		s.log.LogAttrs(context.Background(), slog.LevelError, "messages may never be delivered",
			slog.Uint64("from", it.seq), to, slog.Any("cause", it.err))
		s.handler.OnRecoveryLoss(ctx, s, it.seq, it.end, it.err)
		return
	case itemApp:
		d.deliver(ctx, it)
		d.releaseFrame(it.frame)
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
	// The profile it was received with (see parseApp): it parses.
	if err := fix2.ParseInto(m, it.frame, s.dicts.appProfile(it.in.ApplVerID)); err != nil {
		s.log.LogAttrs(context.Background(), slog.LevelError, "received message does not parse again: not delivered",
			slog.Uint64("seq", it.seq), slog.Any("error", err))
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
	if s.c.closing {
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "handler rejected a message after our Logout: not sent",
			slog.Uint64("seq", it.seq), slog.Any("error", err))
		return
	}
	if isReject(m.MsgType()) {
		// A BusinessMessageReject the handler refuses, as a generated
		// Dispatch without its handler does, is not rejected: a counterparty
		// refusing ours alike would reject it in turn, and the two would loop.
		s.logUnanswered(m, noTag, err.Error())
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
	case errors.Is(err, fix2.ErrUnsupportedMessageType):
		s.businessRejectLocked(m, BusinessRejectUnsupportedMessageType, "", "Unsupported Message Type: "+string(m.MsgType()))
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

// messagesAhead reports whether a received message is queued or being
// delivered, or their delivery held: messages received after it are
// processed only once it is. Notifications (OnLogon, OnLogout,
// OnRecoveryLoss) hold nothing back.
func (d *delivery) messagesAhead() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.busyMsg || d.held != 0 {
		return true
	}
	for _, it := range d.items[d.head:] {
		if it.kind == itemApp || it.kind == itemMark {
			return true
		}
	}
	return false
}

// persistLocked is persist with the session's lock held.
//
// Received messages are processed at least once: a restart processes again
// those processed but not recorded, a batch of 64 at most. A failure to
// record them keeps it so: it stops the connection, and holds the delivery
// of the messages received after them, those released from a stash
// included, until recording works again: the next connection starts only
// then (see attach), and the delivery goroutine tries again meanwhile. They
// were received, and are counted in NextTarget: they are not asked for
// again, but delivered then, or once the generation they belong to is reset,
// or as the session stops, at the risk of a second delivery after a restart
// rather than of a loss. A failure that tells the store moved on halts the
// session. ErrUnknownOutcome alone does not: setting the counter again
// settles it.
func (d *delivery) persistLocked(force bool) error {
	s := d.s
	if d.persistedGen != s.st.Generation {
		// What is held belongs to a generation reset since: delivered.
		d.persisted, d.persistedGen, d.failed = 0, s.st.Generation, false
		d.holdLocked(0)
	}
	next := s.committed + 1
	if next == d.persisted || !force && next < d.persisted+64 {
		return nil
	}
	err := s.storeLocked(func() error { return s.store.SetNextTarget(context.Background(), s.st.Generation, next) })
	if err == nil {
		d.persisted, d.failed = next, false
		d.holdLocked(0)
		return nil
	}
	if !d.failed {
		d.failed = true
		s.log.LogAttrs(context.Background(), slog.LevelError, "recording received messages processed failed",
			slog.Uint64("next_target", next), slog.Any("error", err))
	}
	if diverged(err) {
		s.haltLocked(fmt.Errorf("recording the received messages processed up to %d: %w", next-1, err))
	}
	d.holdLocked(s.st.Generation)
	if s.c != nil {
		s.disconnectLocked(s.c, &LogoutError{Text: "store failure", Err: err})
	}
	return err
}

// rejectInfo describes a Reject or BusinessMessageReject received.
func rejectInfo(m *fix2.Message) RejectInfo {
	text := m.Get(tagText)
	if len(text) > maxRejectText {
		// Cut on a character boundary, if UTF-8.
		n := maxRejectText
		for n > maxRejectText-utf8.UTFMax && !utf8.RuneStart(text[n]) {
			n--
		}
		text = text[:n]
	}
	r := RejectInfo{MsgType: string(m.MsgType()), Text: string(text)}
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
