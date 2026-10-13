package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session/transport"
)

// pendingFrame is a message queued for writing: it ends at offset end of
// its buffer.
type pendingFrame struct {
	end     int
	seq     uint64
	queued  time.Time
	probe   string // TestReqID, if the frame is an acknowledgement probe
	gapFill bool   // a SequenceReset-GapFill standing for messages seq..newSeq-1: not their write
	newSeq  uint64 // a GapFill's NewSeqNo
	admin   bool   // a session-level message, which a replay gap-fills rather than resends
}

// conn is one connection of a session: a reader goroutine and a writer
// goroutine around a transport.Conn. Its fields after the comment are guarded
// by the session's lock.
type conn struct {
	s      *Session
	tc     transport.Conn
	epoch  uint64
	framer *fix2.Framer
	msg    fix2.Message // reader-owned
	rmsg   fix2.Message // writer-owned, for replays
	// ctx is cancelled when the connection closes. syncCtx, the writer's
	// Syncs', is cancelled by it too, and by watchSync once a Sync runs
	// for WriteTimeout: syncs counts the Syncs started and ended, odd while
	// one runs; syncWatched, writer-owned, tells watchSync runs.
	ctx         context.Context
	cancel      context.CancelFunc
	syncCtx     context.Context
	syncCancel  context.CancelCauseFunc
	syncs       atomic.Uint64
	syncWatched bool

	// Guarded by s.mu. frames and heldFrames are in MsgSeqNum order: held
	// frames, sequenced after the pending ones, are released after them.
	pending     []byte // frames waiting for the writer
	frames      []pendingFrame
	held        []byte // frames sent while replaying, released after
	heldFrames  []pendingFrame
	spare       []byte // the buffer the writer last wrote, for reuse
	spareFrames []pendingFrame
	replay      *replayJob // replay waiting for the writer, or running
	writing     uint64     // lowest MsgSeqNum of the application messages the writer is writing, 0 if none
	replayQueue []replayJob
	room        chan struct{} // closed when room frees in pending; nil if no Send waits
	closing     bool          // close once pending is written
	closed      bool
	reason      error // why the connection closed

	lastRead, lastWrite time.Time
	testReqID           string    // our outstanding TestRequest's TestReqID
	testReqSent         time.Time // when we sent it
	readBlocked         time.Time // since when the reader waits for the delivery queue
	logonDeadline       time.Time
	logoutDeadline      time.Time
	logoutText          string // the Text of our Logout, why we logged out
	timer               *time.Timer
	testReqs            uint64 // TestRequests sent, for their IDs
	probeWanted         bool   // an acknowledgement probe waits for AckProbeMinInterval

	logonExtra []byte // fields the handler added to the Logon we answer with
	loggedOn   bool   // the connection got to log on

	wake       chan struct{} // wakes the writer
	writers    sync.WaitGroup
	writerStop bool   // the writer stops after its batch, for a reset fence
	resetReq   bool   // a Logon with ResetSeqNumFlag waits for its fence
	resetAfter string // reset the store, for this reason, once the writer is done
	readerDone chan struct{}
	closedCh   chan struct{} // closed when the connection closes
	finished   chan struct{} // closed when run returns: nothing of it acts any more
}

// done returns a channel closed when the connection closes.
func (c *conn) done() <-chan struct{} { return c.closedCh }

// replaying reports whether new messages are held back for a replay.
func (c *conn) replaying() bool { return c.replay != nil }

// hasRoom reports whether pending has room for another message.
func (c *conn) hasRoom() bool {
	cfg := &c.s.cfg
	return len(c.frames) < cfg.MaxPendingMessages && len(c.pending) < cfg.MaxPendingBytes
}

// roomSignal returns a channel closed once pending has room again.
func (c *conn) roomSignal() <-chan struct{} {
	if c.room == nil {
		c.room = make(chan struct{})
	}
	return c.room
}

func (c *conn) wakeWriter() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// attach makes tc the session's connection and starts its writer. The
// caller starts the reader with run once it has sent or awaits the Logon.
// A new connection waits for the previous one to be completely done: its
// writer joined and a reset on disconnection made, so that nothing of it
// acts on its successor.
func (s *Session) attach(tc transport.Conn, framer *fix2.Framer) (*conn, error) {
	s.mu.Lock()
	for prev := s.prev; prev != nil && s.c == nil; prev = s.prev {
		select {
		case <-prev.finished:
		default:
			s.mu.Unlock()
			<-prev.finished
			s.mu.Lock()
			continue
		}
		break
	}
	defer s.mu.Unlock()
	switch {
	case s.stopped || s.stopping:
		return nil, ErrStopped
	case s.halted != nil:
		return nil, s.halted
	case s.stalled:
		return nil, errors.New("session: a handler callback is stuck, see Config.MaxDeliveryStall")
	case s.c != nil:
		return nil, errAlreadyConnected
	case s.cfg.Schedule != nil && !s.inWindow:
		return nil, errOutsideWindow
	}
	if s.deliv == nil {
		s.deliv = newDelivery(s)
	}
	// What the previous connection left undone is done first, or refuses
	// this one: a reset on disconnection, which a new window's reset makes
	// too, then the recording of the received messages processed, which
	// would otherwise fall ever further behind. A reset discards those
	// messages' generation: there is nothing left to record of it.
	switch {
	case s.cfg.Schedule != nil && s.st.WindowID != s.window.ID:
		// A new window starts afresh; the previous connection is done.
		if err := s.resetStoreLocked("new session window "+s.window.Start.Format(time.RFC3339), s.window.ID); err != nil {
			return nil, err
		}
	case s.resetOwed != "":
		if err := s.resetStoreLocked(s.resetOwed); err != nil {
			return nil, err
		}
	}
	if d := s.deliv; d.failed && d.persistedGen == s.st.Generation {
		if err := d.persistLocked(true); err != nil {
			return nil, fmt.Errorf("session %s: recording the received messages processed: %w", s.id, err)
		}
	}
	s.epoch++
	now := time.Now()
	c := &conn{
		s:          s,
		tc:         tc,
		epoch:      s.epoch,
		framer:     framer,
		wake:       make(chan struct{}, 1),
		readerDone: make(chan struct{}),
		closedCh:   make(chan struct{}),
		finished:   make(chan struct{}),
		lastRead:   now,
		lastWrite:  now,
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	if s.syncer != nil {
		c.syncCtx, c.syncCancel = context.WithCancelCause(c.ctx)
	}
	if c.framer == nil {
		c.framer = fix2.NewFramer(tc, fix2.FramerOptions{})
	}
	s.c = c
	s.prev = c
	s.phase = phaseAwaitingLogon
	if s.initiator {
		s.phase = phaseLogonSent
	}
	c.logonDeadline = now.Add(s.cfg.LogonTimeout)
	c.timer = time.AfterFunc(s.cfg.LogonTimeout, func() { s.onTimer(c) })
	s.stats.Connections.Add(1)
	s.startRetentionLocked()
	c.startWriterLocked()
	return c, nil
}

// startWriterLocked starts a writer for c; the returned channel closes when
// it exits.
func (c *conn) startWriterLocked() <-chan struct{} {
	done := make(chan struct{})
	c.writers.Add(1)
	go func() {
		defer c.writers.Done()
		defer close(done)
		c.writeLoop()
	}()
	return done
}

var (
	errAlreadyConnected = errors.New("session: already connected")
	errSyncTimeout      = errors.New("session: store Sync took longer than WriteTimeout")
)

// run reads and handles the connection's messages until it closes, then
// waits for its writer and returns why it closed.
func (c *conn) run() error {
	defer close(c.finished)
	defer close(c.readerDone)
	for {
		frame, err := c.framer.Next()
		if err != nil {
			var g *fix2.GarbledError
			if errors.As(err, &g) {
				c.s.stats.Garbled.Add(1)
				c.s.log.LogAttrs(context.Background(), slog.LevelWarn, "garbled input ignored",
					slog.Int("bytes", g.Skipped), slog.String("error", g.Err.Error()))
				continue
			}
			c.lost(err)
			break
		}
		c.handleFrame(frame)
	}
	c.writers.Wait()
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if c.resetAfter != "" {
		// Once the writer is done: nothing of the old generation can be
		// written after the reset. If it fails, the next connection makes
		// it first.
		if err := c.s.resetStoreLocked(c.resetAfter); err != nil {
			c.s.resetOwed = c.resetAfter
		}
	}
	return c.reason
}

// lost handles the end of the stream, or a read error.
func (c *conn) lost(err error) {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.closed {
		return
	}
	if errors.Is(err, io.EOF) {
		err = errors.New("connection closed by the counterparty")
	}
	if s.phase == phaseLogoutSent && c.logoutText != "" {
		// Hanging up answers our Logout too: why we logged out.
		s.disconnectLocked(c, &LogoutError{Text: c.logoutText, Err: err})
		return
	}
	s.disconnectLocked(c, &LogoutError{Text: "connection lost", Err: err})
}

// disconnectLocked closes c, which unblocks its goroutines, and detaches it
// from the session.
func (s *Session) disconnectLocked(c *conn, reason error) {
	if c.closed {
		return
	}
	c.closed = true
	close(c.closedCh)
	c.cancel()
	if c.reason == nil {
		c.reason = reason
	}
	s.unsentLocked(c)
	c.timer.Stop()
	c.tc.Close()
	c.wakeWriter()
	if c.room != nil {
		close(c.room)
		c.room = nil
	}
	if s.c != c {
		return
	}
	was := s.phase
	s.c = nil
	s.phase = phaseDisconnected
	s.receipts.connectionLost()
	s.recov = recovery{}
	clear(s.stash)
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "disconnected",
		slog.Uint64("epoch", c.epoch), slog.String("phase", was.String()), slog.Any("reason", reason))
	if was == phaseEstablished || was == phaseLogoutSent {
		s.deliv.push(item{kind: itemLogout, err: reason})
	}
	switch {
	case s.cfg.ResetOnDisconnect:
		c.resetAfter = "ResetOnDisconnect"
	case s.cfg.ResetOnLogout && was == phaseLogoutSent:
		c.resetAfter = "ResetOnLogout"
	}
}

// closeAfterFlushLocked closes c once what is queued is written: after a
// Logout.
func (s *Session) closeAfterFlushLocked(c *conn, reason error) {
	if c.closed || c.closing {
		return
	}
	c.closing = true
	c.reason = reason
	c.wakeWriter()
}

// writeLoop writes what is queued until the connection closes.
func (c *conn) writeLoop() {
	s := c.s
	for {
		s.mu.Lock()
		for len(c.pending) == 0 && (c.replay == nil || c.replay.running) && !c.closed && !c.closing && !c.writerStop {
			s.mu.Unlock()
			<-c.wake
			s.mu.Lock()
		}
		if c.closed || c.writerStop {
			s.mu.Unlock()
			return
		}
		// What is pending was sequenced before the replay was asked for:
		// it goes first. Messages sequenced since are held behind it.
		if job := c.replay; job != nil && !job.running && len(c.pending) == 0 {
			job.running = true
			s.mu.Unlock()
			c.runReplay(job)
			continue
		}
		if len(c.pending) == 0 && c.closing {
			s.disconnectLocked(c, c.reason)
			s.mu.Unlock()
			return
		}
		buf, frames := c.pending, c.frames
		c.pending, c.frames = c.spare[:0], c.spareFrames[:0]
		c.spare, c.spareFrames = nil, nil
		if s.cfg.Retention.Interval > 0 {
			// Not evicted until written.
			for _, f := range frames {
				if !f.admin && (c.writing == 0 || f.seq < c.writing) {
					c.writing = f.seq
				}
			}
		}
		c.signalRoomLocked()
		gen, epoch := s.st.Generation, c.epoch
		s.mu.Unlock()

		var err error
		if s.syncer != nil {
			err = c.sync(gen, frames[len(frames)-1].seq)
		}
		if err == nil {
			c.tc.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
			var n int
			n, err = c.tc.Write(buf)
			c.wrote(buf, frames, n, gen, epoch, false)
		}

		s.mu.Lock()
		c.spare, c.spareFrames = buf[:0], frames[:0]
		if err != nil && !c.closed {
			// What it did not write stays unwritten: disconnectLocked notes
			// it.
			s.disconnectLocked(c, &LogoutError{Text: "write failed", Err: err})
		}
		c.writing = 0
		s.mu.Unlock()
	}
}

// sync makes the messages of generation gen up to upTo durable before they
// are written, if the store needs it: from the writer, without the session's
// lock, as Sync may run alongside the store's other calls, bounded by
// WriteTimeout (see watchSync) and c closing. A failure keeps them from being
// written: it disconnects c, and halts the session if the outcome is
// uncertain, unless the session cancelled the Sync itself and the store says
// no more than that it did not finish. Otherwise the next Sync, which covers
// them again, retries.
func (c *conn) sync(gen, upTo uint64) error {
	s := c.s
	if !c.syncWatched {
		c.syncWatched = true
		c.watchSync(0, 0)
	}
	// A counter, not a timer armed for each Sync: the default store syncs
	// too, on the hot path.
	c.syncs.Add(1)
	err := s.syncer.Sync(c.syncCtx, gen, upTo)
	c.syncs.Add(1)
	if err == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch cause := context.Cause(c.syncCtx); {
	// A store that moved on, or was closed, is no outcome of the
	// cancellation, whatever error it is joined with.
	case cause != nil && !diverged(err):
		// The session cancelled the Sync itself, the connection closing
		// or the Sync taking too long, and its outcome is unknown at
		// worst: no halt. The batch is not written, and Sync being
		// cumulative, the next one, the next connection's, makes it
		// durable again.
		level := slog.LevelDebug
		if errors.Is(cause, errSyncTimeout) {
			level = slog.LevelWarn
		}
		if !errors.Is(err, cause) {
			err = fmt.Errorf("%w: %w", cause, err)
		}
		s.log.LogAttrs(context.Background(), level, "making messages durable cancelled: not sent",
			slog.Uint64("up_to", upTo), slog.Any("error", err))
	default:
		s.log.LogAttrs(context.Background(), slog.LevelError, "making messages durable failed: not sent",
			slog.Uint64("up_to", upTo), slog.Any("error", err))
		if uncertain(err) && gen == s.st.Generation {
			s.haltLocked(fmt.Errorf("making messages up to %d durable: %w", upTo, err))
		}
	}
	s.disconnectLocked(c, &LogoutError{Text: "store failure", Err: err})
	return err
}

// watchSync checks, every half WriteTimeout (a millisecond at least) until c
// closes, whether the writer's Sync running, the one counted seen, was
// running seen times before: three times in a row, it has run for
// WriteTimeout at least, and at most half more, and it is cancelled.
func (c *conn) watchSync(seen uint64, times int) {
	if c.ctx.Err() != nil {
		return
	}
	switch n := c.syncs.Load(); {
	case n%2 == 0:
		times = 0
	case n == seen:
		times++
	default:
		seen, times = n, 1
	}
	if times >= 3 {
		c.syncCancel(errSyncTimeout)
		return
	}
	time.AfterFunc(max(c.s.cfg.WriteTimeout/2, time.Millisecond), func() { c.watchSync(seen, times) })
}

// wrote accounts for frames of buf, of which the first n bytes were written:
// the frames written completely, and only those, are Written, or, a
// replay's GapFills, skip the messages they stand for.
func (c *conn) wrote(buf []byte, frames []pendingFrame, n int, gen, epoch uint64, resend bool) {
	s := c.s
	start, done := 0, 0
	for _, f := range frames {
		if f.end > n {
			break
		}
		if s.cfg.Observer != nil {
			s.cfg.Observer.OnOutbound(s, Frame{Raw: buf[start:f.end], Seq: f.seq, Generation: gen, Epoch: epoch, Resend: resend})
		}
		start = f.end
		done++
	}
	if done == 0 {
		return
	}
	s.stats.MessagesSent.Add(uint64(done))
	s.stats.BytesSent.Add(uint64(start))
	s.mu.Lock()
	defer s.mu.Unlock()
	c.lastWrite = time.Now()
	for _, f := range frames[:done] {
		switch {
		case f.probe != "":
			s.receipts.probeWritten(f.probe, epoch)
		case f.gapFill:
			// Written whole: the counterparty moves past them for good.
			s.receipts.tick(epoch)
			s.receipts.gapFilled(gen, f.seq, f.newSeq)
			s.stats.GapFilled.Add(f.newSeq - f.seq)
		case resend:
			s.receipts.written(gen, f.seq, epoch)
			s.stats.Resent.Add(1)
		default:
			s.receipts.written(gen, f.seq, epoch)
		}
		if f.end <= len(buf) && s.cfg.AckProbe == AckProbeEager && !f.gapFill && f.probe == "" && !c.closed {
			s.probeLocked(c)
		}
	}
	if !c.closed {
		s.armLocked(c)
	}
}

// signalRoomLocked wakes the Sends waiting for room, if there is some.
func (c *conn) signalRoomLocked() {
	if c.room != nil && c.hasRoom() {
		close(c.room)
		c.room = nil
	}
}

// replayRunning reports whether the replay job is being run by the writer.
func (c *conn) replayRunning() bool { return c.replay != nil && c.replay.running }

// onTimer handles c's timer: logon and logout timeouts, heartbeats, test
// requests and stalls.
func (s *Session) onTimer(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.closed || s.c != c {
		return
	}
	now := time.Now()
	switch s.phase {
	case phaseAwaitingLogon, phaseLogonSent:
		if !now.Before(c.logonDeadline) {
			s.disconnectLocked(c, &LogoutError{Text: "no Logon received within " + s.cfg.LogonTimeout.String()})
			return
		}
	case phaseLogoutSent:
		if !now.Before(c.logoutDeadline) {
			s.disconnectLocked(c, &LogoutError{Text: "no Logout reply within " + s.cfg.LogoutTimeout.String()})
			return
		}
	}
	if !c.readBlocked.IsZero() && now.Sub(c.readBlocked) >= s.cfg.deliveryStall(s.heartBt) {
		s.stalled = true
		s.log.LogAttrs(context.Background(), slog.LevelError, "handler stalled: FromApp has not returned while the delivery queue is full",
			slog.Duration("for", now.Sub(c.readBlocked)))
		s.sendLogoutLocked(c, "application not processing messages")
		s.closeAfterFlushLocked(c, &LogoutError{Text: "handler stalled"})
		return
	}
	if s.phase == phaseEstablished || s.phase == phaseLogoutSent {
		if hb := s.heartBt; hb > 0 && c.readBlocked.IsZero() {
			grace := hb + hb/5
			switch {
			case c.testReqID != "" && now.Sub(c.testReqSent) >= grace:
				s.disconnectLocked(c, &LogoutError{Text: "no answer to TestRequest " + c.testReqID + " within " + grace.String()})
				return
			case c.testReqID == "" && now.Sub(c.lastRead) >= grace:
				// "TEST", as QuickFIX and its acceptance tests: a single
				// liveness probe is ever outstanding.
				c.testReqID = "TEST"
				c.testReqSent = now
				s.sendAdminLocked(&header{msgType: "1"}, adminBody(nil).str(tagTestReqID, c.testReqID))
			}
			// No heartbeat while our TestRequest waits for its answer, as
			// QuickFIX: the disconnection follows.
			if c.testReqID == "" && now.Sub(c.lastWrite) >= hb && len(c.pending) == 0 && !c.replaying() {
				s.sendAdminLocked(&header{msgType: "0"}, nil)
			}
		}
		if c.probeWanted {
			s.probeLocked(c)
		}
		if age := s.cfg.MaxPendingAge; age > 0 && len(c.frames) > 0 && now.Sub(c.frames[0].queued) >= age {
			s.disconnectLocked(c, &LogoutError{Text: "counterparty not reading: a message waited " + now.Sub(c.frames[0].queued).String(), Err: ErrSlowConsumer})
			return
		}
	}
	s.armLocked(c)
}

// armLocked sets c's timer to its next deadline.
func (s *Session) armLocked(c *conn) {
	if c.closed {
		return
	}
	now := time.Now()
	next := now.Add(time.Hour)
	at := func(t time.Time) {
		if t.Before(next) {
			next = t
		}
	}
	switch s.phase {
	case phaseAwaitingLogon, phaseLogonSent:
		at(c.logonDeadline)
	case phaseLogoutSent:
		at(c.logoutDeadline)
	}
	if !c.readBlocked.IsZero() {
		at(c.readBlocked.Add(s.cfg.deliveryStall(s.heartBt)))
	} else if hb := s.heartBt; hb > 0 && (s.phase == phaseEstablished || s.phase == phaseLogoutSent) {
		grace := hb + hb/5
		at(c.lastWrite.Add(hb))
		if c.testReqID != "" {
			at(c.testReqSent.Add(grace))
		} else {
			at(c.lastRead.Add(grace))
		}
	}
	if age := s.cfg.MaxPendingAge; age > 0 && len(c.frames) > 0 {
		at(c.frames[0].queued.Add(age))
	}
	if c.probeWanted {
		at(s.receipts.lastAt.Add(s.cfg.AckProbeMinInterval))
	}
	c.timer.Reset(max(next.Sub(now), time.Millisecond))
}

func itoa(v uint64) string { return string(appendDecimal(nil, v)) }

// removePendingLocked removes from c's pending frames the application
// messages whose MsgSeqNum is in [begin, end]: they are about to be replayed.
// Session-level messages stay, to be written before the replay: it only
// gap-fills them, and a ResendRequest or a Heartbeat answering a TestRequest
// dropped would never be sent.
func (c *conn) removePendingLocked(begin, end uint64) {
	if len(c.frames) == 0 {
		return
	}
	out, kept := c.pending[:0], c.frames[:0]
	start := 0
	for _, f := range c.frames {
		srcEnd := f.end
		if f.admin || f.seq < begin || f.seq > end {
			out = append(out, c.pending[start:srcEnd]...)
			f.end = len(out)
			kept = append(kept, f)
		}
		start = srcEnd
	}
	c.pending, c.frames = out, slices.Clip(kept)[:len(kept)]
	c.signalRoomLocked()
}
