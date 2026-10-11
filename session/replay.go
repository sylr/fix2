package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session/store"
)

// replayJob is a range of our messages to resend.
type replayJob struct {
	begin, end uint64
	running    bool
}

// maxQueuedReplays bounds the ResendRequests queued behind a running replay.
const maxQueuedReplays = 16

// errMissing stops a replay at a message missing from the store.
var errMissing = errors.New("message missing from the store")

// replayChunk is how many bytes a replay writes at once.
const replayChunk = 64 << 10

// startReplayLocked has the writer resend messages begin to end. New
// messages are held back until it is done, so that none goes out amid the
// replay; queued ones of the range not yet handed to the writer are dropped,
// to be sent by the replay as retransmissions. A replay asked for while one
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

	var (
		buf      []byte
		frames   []pendingFrame
		gapStart uint64
		now      = time.Now()
		sending  = fix2.AppendUTCTimestamp(nil, now, s.prec)
		writeErr error
		gaps     [][2]uint64
	)
	flush := func() {
		if len(buf) == 0 || writeErr != nil {
			buf, frames = buf[:0], frames[:0]
			return
		}
		c.tc.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
		n, err := c.tc.Write(buf)
		c.wrote(buf, frames, n, gen, epoch, true)
		writeErr = err
		buf, frames = buf[:0], frames[:0]
	}
	gapFill := func(next uint64) {
		if gapStart == 0 {
			return
		}
		buf = s.appendFrame(buf, &header{msgType: "4", seq: gapStart, possDup: true, orig: sending, sendingTime: now},
			adminBody(nil).int(tagNewSeqNo, next).str(tagGapFillFlag, "Y"))
		frames = append(frames, pendingFrame{end: len(buf), seq: gapStart, gapFill: true})
		s.stats.GapFilled.Add(next - gapStart)
		gaps = append(gaps, [2]uint64{gapStart, next})
		gapStart = 0
	}
	var missing uint64 // a message missing from the store: the replay stops there
	err := s.store.Range(context.Background(), gen, job.begin, job.end, func(seq uint64, raw []byte, hole store.Hole) error {
		if writeErr != nil {
			return writeErr
		}
		skip := false
		switch {
		case hole == store.HoleMissing:
			// Not gap-filled: the counterparty would move past a message
			// that may still exist somewhere.
			missing = seq
			return errMissing
		case hole != store.HoleNone:
			if hole == store.HoleRetention {
				s.log.LogAttrs(context.Background(), slog.LevelWarn, "message beyond retention gap-filled", slog.Uint64("seq", seq))
			}
			skip = true
		case isAdmin(msgTypeOf(raw)):
			skip = true
		default:
			if fix2.ParseInto(&c.rmsg, raw, s.dicts.parse) != nil || !s.handler.OnResend(s.callbackCtx(), s, seq, &c.rmsg) {
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
		out, err := fix2.RewriteForResend(buf, raw, s.dicts.parse, sending)
		if err != nil {
			s.log.LogAttrs(context.Background(), slog.LevelError, "stored message cannot be resent: gap-filled",
				slog.Uint64("seq", seq), slog.Any("error", err))
			gapStart = seq
			return nil
		}
		buf = out
		frames = append(frames, pendingFrame{end: len(buf), seq: seq})
		s.stats.Resent.Add(1)
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
		readErr = err
		gapFill(job.begin) // only what precedes is covered: nothing
	}
	flush()

	s.mu.Lock()
	defer s.mu.Unlock()
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
		c.replay, c.replayQueue = nil, nil
		s.logoutLocked(c, text)
		return
	}
	for _, g := range gaps {
		s.receipts.gapFilled(gen, g[0], g[1])
	}
	if writeErr != nil {
		if !c.closed {
			s.disconnectLocked(c, &LogoutError{Text: "write failed while resending", Err: writeErr})
		}
		c.replay = nil
		return
	}
	if len(c.replayQueue) > 0 {
		next := c.replayQueue[0]
		c.replayQueue = c.replayQueue[1:]
		c.replay = &replayJob{begin: next.begin, end: min(next.end, s.st.NextSender-1)}
		if c.replay.begin > c.replay.end {
			c.replay = nil
		} else {
			c.wakeWriter()
			return
		}
	}
	c.replay = nil
	// Release what was held back, after what is pending.
	base := len(c.pending)
	c.pending = append(c.pending, c.held...)
	for _, f := range c.heldFrames {
		f.end += base
		c.frames = append(c.frames, f)
	}
	c.held, c.heldFrames = c.held[:0], c.heldFrames[:0]
	c.signalRoomLocked()
	c.wakeWriter()
}
