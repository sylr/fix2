package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session/store"
)

// replayJob is a range of our messages to resend.
type replayJob struct {
	begin, end uint64
	running    bool
}

// coveredRun is a run of messages beyond retention a GapFill covers, the
// GapFill ending at offset end of the replay's buffer.
type coveredRun struct {
	run [2]uint64
	end int
}

// maxQueuedReplays bounds the ResendRequests queued behind a running replay.
const maxQueuedReplays = 16

// errMissing stops a replay at a message missing from the store.
var errMissing = errors.New("message missing from the store")

// replayChunk is how many bytes a replay writes at once.
const replayChunk = 64 << 10

// startReplayLocked has the writer resend messages begin to end. New
// messages are held back until it is done, so that none goes out amid the
// replay; queued application messages of the range not yet handed to the
// writer are dropped, to be sent by the replay as retransmissions, and
// queued session-level ones are written first. A replay asked for while one
// runs is queued, merged with an overlapping one.
func (s *Session) startReplayLocked(c *conn, begin, end uint64, keepPending bool) {
	if c.replay != nil {
		for i, q := range c.replayQueue {
			if begin <= q.end+1 && end+1 >= q.begin {
				c.replayQueue[i] = replayJob{begin: min(begin, q.begin), end: max(end, q.end)}
				return
			}
		}
		if len(c.replayQueue) < maxQueuedReplays {
			c.replayQueue = append(c.replayQueue, replayJob{begin: begin, end: end})
		} else {
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "too many ResendRequests queued: one dropped",
				slog.Uint64("begin", begin), slog.Uint64("end", end))
			// Not served: the counterparty will ask again.
			s.ret.holdLocked(begin, end)
		}
		return
	}
	if !keepPending {
		c.removePendingLocked(begin, end)
	}
	c.replay = &replayJob{begin: begin, end: end}
	c.wakeWriter()
}

// runReplay resends what job asks for, from the writer goroutine: stored
// application messages are rewritten as retransmissions, and runs of
// administrative messages, messages not stored and messages the handler does
// not want resent are covered by one SequenceReset-GapFill each.
func (c *conn) runReplay(job *replayJob) {
	s := c.s
	s.mu.Lock()
	gen, epoch := s.st.Generation, c.epoch
	s.mu.Unlock()
	if s.syncer != nil {
		// What is resent may not be durable yet: the messages taken from
		// those pending, for one.
		if err := c.sync(gen, job.end); err != nil {
			s.mu.Lock()
			c.replay, c.replayQueue = nil, nil
			s.mu.Unlock()
			return
		}
	}

	var (
		buf      []byte
		frames   []pendingFrame
		gapStart uint64
		now      = time.Now()
		sending  = fix2.AppendUTCTimestamp(nil, now, s.prec)
		writeErr error
		evicted  [][2]uint64  // runs of messages beyond retention, inclusive
		covered  []coveredRun // those of them a GapFill in buf covers
		lost     [][2]uint64  // those of them a GapFill written covers
	)
	flush := func() {
		if len(buf) == 0 || writeErr != nil {
			buf, frames, covered = buf[:0], frames[:0], covered[:0]
			return
		}
		c.tc.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
		n, err := c.tc.Write(buf)
		c.wrote(buf, frames, n, gen, epoch, true)
		for _, r := range covered {
			if r.end <= n {
				lost = append(lost, r.run)
			}
		}
		covered = covered[:0]
		writeErr = err
		buf, frames = buf[:0], frames[:0]
	}
	gapFill := func(next uint64) {
		if gapStart == 0 || next <= gapStart {
			gapStart = 0
			return
		}
		buf = s.appendFrame(buf, &header{msgType: "4", seq: gapStart, possDup: true, orig: sending, sendingTime: now},
			adminBody(nil).int(tagNewSeqNo, next).str(tagGapFillFlag, "Y"))
		frames = append(frames, pendingFrame{end: len(buf), seq: gapStart, gapFill: true, newSeq: next})
		for len(evicted) > 0 && evicted[0][0] < next {
			covered = append(covered, coveredRun{run: evicted[0], end: len(buf)})
			evicted = evicted[1:]
		}
		gapStart = 0
	}
	var missing uint64 // a message missing from the store: the replay stops there
	err := s.store.Range(context.Background(), gen, job.begin, job.end, func(seq uint64, raw []byte, hole store.Hole) error {
		if writeErr != nil {
			return writeErr
		}
		// Our own message: of our default version, unless its header says.
		p := s.profileFor(raw, false)
		skip := false
		switch {
		case hole == store.HoleMissing:
			// Not gap-filled: the counterparty would move past a message
			// that may still exist somewhere.
			missing = seq
			return errMissing
		case hole != store.HoleNone:
			if hole == store.HoleRetention {
				if n := len(evicted); n > 0 && evicted[n-1][1]+1 == seq {
					evicted[n-1][1] = seq
				} else {
					evicted = append(evicted, [2]uint64{seq, seq})
				}
			}
			skip = true
		case isAdmin(msgTypeOf(raw)):
			skip = true
		default:
			if fix2.ParseInto(&c.rmsg, raw, p) != nil || !s.handler.OnResend(s.callbackCtx(), s, seq, &c.rmsg) {
				skip = true
			}
		}
		if skip {
			if gapStart == 0 {
				gapStart = seq
			}
			return nil
		}
		gapFill(seq)
		out, err := fix2.RewriteForResend(buf, raw, p, sending)
		if err != nil {
			s.log.LogAttrs(context.Background(), slog.LevelError, "stored message cannot be resent: gap-filled",
				slog.Uint64("seq", seq), slog.Any("error", err))
			gapStart = seq
			return nil
		}
		buf = out
		frames = append(frames, pendingFrame{end: len(buf), seq: seq})
		if len(buf) >= replayChunk {
			flush()
		}
		return nil
	})
	readErr := error(nil)
	switch {
	case err == nil:
		gapFill(job.end + 1)
	case errors.Is(err, errMissing):
		gapFill(missing)
	case writeErr == nil || !errors.Is(err, writeErr):
		// Nothing more is covered: what was read is resent, not gap-filled.
		readErr = err
		gapStart = 0
	}
	flush()

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range lost {
		// Gap-filled: the counterparty moves past them for good.
		s.stats.BeyondRetention.Add(e[1] - e[0] + 1)
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "messages beyond retention gap-filled",
			slog.Uint64("from", e[0]), slog.Uint64("to", e[1]))
		s.deliv.push(item{kind: itemLoss, gen: gen, seq: e[0], end: e[1], err: ErrBeyondRetention})
	}
	if diverged(err) && gen == s.st.Generation && !s.stopped {
		// The store moved on without the session, not because a reset
		// since dropped the generation read. A stopped session closed its
		// store itself: it logs out as for any other failure to read.
		s.haltLocked(fmt.Errorf("reading our messages %d to %d to resend: %w", job.begin, job.end, err))
		c.replay, c.replayQueue = nil, nil
		return
	}
	if (missing != 0 || readErr != nil) && writeErr == nil && !c.closed {
		var text string
		var cause error
		if missing != 0 {
			text = fmt.Sprintf("cannot resend message %d: missing from our store", missing)
			cause = errors.New("our message is missing from the store")
			s.deliv.push(item{kind: itemLoss, gen: gen, seq: missing, end: missing, err: cause})
		} else {
			text = "cannot read our messages to resend"
			cause = readErr
		}
		s.log.LogAttrs(context.Background(), slog.LevelError, "resend aborted", slog.String("reason", text), slog.Any("error", cause))
		// What it did not resend stays unwritten.
		s.unsentLocked(c)
		c.replay, c.replayQueue = nil, nil
		s.logoutLocked(c, text)
		return
	}
	if writeErr != nil {
		if !c.closed {
			s.disconnectLocked(c, &LogoutError{Text: "write failed while resending", Err: writeErr})
		}
		c.replay = nil
		return
	}
	if missing == 0 && readErr == nil {
		s.ret.deliveredLocked(job.begin, job.end)
	}
	c.replay = nil
	for c.replay == nil && len(c.replayQueue) > 0 {
		next := c.replayQueue[0]
		c.replayQueue = c.replayQueue[1:]
		if end := min(next.end, s.st.NextSender-1); next.begin <= end {
			// What was held up to the end of the next replay was
			// sequenced before it was asked for: it is released, then
			// treated as startReplayLocked treats what is pending. What
			// was held after stays held behind it.
			c.releaseHeldLocked(end)
			c.removePendingLocked(next.begin, end)
			c.replay = &replayJob{begin: next.begin, end: end}
		}
	}
	if c.replay == nil {
		// Release what was held back, after what is pending.
		c.releaseHeldLocked(math.MaxUint64)
	}
	c.wakeWriter()
}

// releaseHeldLocked moves the frames held back during a replay whose
// MsgSeqNum is at most upTo, the first ones, to the end of the pending ones.
func (c *conn) releaseHeldLocked(upTo uint64) {
	n := len(c.heldFrames)
	for i, f := range c.heldFrames {
		if f.seq > upTo {
			n = i
			break
		}
	}
	if n == 0 {
		return
	}
	cut := c.heldFrames[n-1].end
	base := len(c.pending)
	c.pending = append(c.pending, c.held[:cut]...)
	for _, f := range c.heldFrames[:n] {
		f.end += base
		c.frames = append(c.frames, f)
	}
	rest := copy(c.held, c.held[cut:])
	c.held = c.held[:rest]
	if rest == 0 {
		c.held = reusable(c.held, c.s.cfg.MaxPendingBytes)
	}
	kept := copy(c.heldFrames, c.heldFrames[n:])
	c.heldFrames = c.heldFrames[:kept]
	for i := range c.heldFrames {
		c.heldFrames[i].end -= cut
	}
	c.signalRoomLocked()
}
