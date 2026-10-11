package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
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
	gapFill bool   // a SequenceReset-GapFill standing for messages seq..: not their write
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

	// Guarded by s.mu.
	pending     []byte // frames waiting for the writer
	frames      []pendingFrame
	held        []byte // frames sent while replaying, released after
	heldFrames  []pendingFrame
	spare       []byte // the buffer the writer last wrote, for reuse
	spareFrames []pendingFrame
	replay      *replayJob // replay waiting for the writer, or running
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
	if s.cfg.Schedule != nil && s.st.WindowID != s.window.ID {
		// A new window starts afresh; the previous connection is done.
		if err := s.resetStoreLocked("new session window "+s.window.Start.Format(time.RFC3339), s.window.ID); err != nil {
			return nil, err
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

var errAlreadyConnected = errors.New("session: already connected")

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
		// written after the reset.
		c.s.resetStoreLocked(c.resetAfter)
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
	if c.reason == nil {
		c.reason = reason
	}
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
		c.signalRoomLocked()
		gen, epoch := s.st.Generation, c.epoch
		s.mu.Unlock()

		c.tc.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
		n, err := c.tc.Write(buf)
		c.wrote(buf, frames, n, gen, epoch, false)

		s.mu.Lock()
		c.spare, c.spareFrames = buf[:0], frames[:0]
		if err != nil && !c.closed {
			s.disconnectLocked(c, &LogoutError{Text: "write failed", Err: err})
		}
		s.mu.Unlock()
	}
}

// wrote accounts for frames of buf, of which the first n bytes were written:
// the frames written completely, and only those, are Written.
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
			s.receipts.tick(epoch)
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
