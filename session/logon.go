package session

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	fix2 "github.com/sylr/fix2"
)

// logonTags are the tags of a Logon the session writes, on top of the
// reserved ones: a ToAdmin callback cannot set them.
func logonReserved(tag int) bool {
	switch tag {
	case tagEncryptMethod, tagHeartBtInt, tagResetSeqNumFlag, tagNextExpectedMsgSeqNum, tagDefaultApplVerID:
		return true
	}
	return reserved(tag)
}

// toAdmin asks the handler for the extra fields of a Logon or Logout.
func (s *Session) toAdmin(msgType string) ([]byte, error) {
	b := fix2.NewBuilder(s.id.BeginString, msgType)
	if err := s.handler.ToAdmin(s.callbackCtx(), s, msgType, b); err != nil {
		return nil, err
	}
	return b.AppendBody(nil, logonReserved)
}

// logon sends the initiator's Logon on c.
func (s *Session) logon(c *conn) error {
	extra, err := s.toAdmin("A")
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.disconnectLocked(c, &LogoutError{Text: "Logon cancelled by the handler", Err: err})
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.closed || s.c != c {
		return c.reason
	}
	s.sentReset = false
	if s.cfg.ResetOnLogon {
		s.resetStoreLocked("ResetOnLogon")
		s.sentReset = true
	}
	b := s.logonBody(s.sentReset, s.st.NextTarget)
	b = append(b, extra...)
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "sending Logon",
		slog.Uint64("seq", s.st.NextSender), slog.Bool("reset", s.sentReset))
	s.sendAdminLocked(&header{msgType: "A"}, b)
	return nil
}

// logonBody returns the fields of our Logon: next is the NextExpectedMsgSeqNum
// to announce, if used.
func (s *Session) logonBody(reset bool, next uint64) adminBody {
	b := adminBody(nil).str(tagEncryptMethod, "0").int(tagHeartBtInt, uint64(s.heartBt/time.Second))
	if reset && s.prof.resetSeqNum {
		b = b.str(tagResetSeqNumFlag, "Y")
	}
	if s.cfg.NextExpectedMsgSeqNum && s.prof.nextExpected {
		b = b.int(tagNextExpectedMsgSeqNum, next)
	}
	if s.prof.fixt {
		b = b.str(tagDefaultApplVerID, s.cfg.Dictionaries.DefaultApplVerID)
	}
	return b
}

// onLogonLocked handles a Logon: the counterparty's, or its answer to ours.
func (s *Session) onLogonLocked(c *conn, m *fix2.Message, in Inbound, now time.Time) {
	if s.phase == phaseEstablished || s.phase == phaseLogoutSent {
		if bytes.Equal(m.Get(tagResetSeqNumFlag), yes) && s.prof.resetSeqNum {
			s.activeResetLocked(c, m, in)
			return
		}
		s.logoutLocked(c, "Logon received while already logged on")
		return
	}

	// Errors before the session is logged on disconnect without a word, as
	// FIX requires: a Logout would take a sequence number, and tell an
	// unknown peer what is wrong.
	refuse := func(format string, args ...any) {
		text := fmt.Sprintf(format, args...)
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "Logon refused", slog.String("reason", text))
		s.disconnectLocked(c, &LogoutError{Text: "Logon refused: " + text})
	}
	if _, text := s.compIDProblem(m); text != "" {
		refuse("%s", text)
		return
	}
	if _, _, text := s.sendingTimeProblem(m, now); text != "" {
		refuse("%s", text)
		return
	}
	if v := m.Get(tagEncryptMethod); v != nil && string(v) != "0" {
		refuse("EncryptMethod (98) %s not supported, only 0 (none)", quoteValue(v))
		return
	}
	hb, err := fix2.ParseInt(m.Get(tagHeartBtInt))
	switch {
	case m.Get(tagHeartBtInt) == nil:
		refuse("HeartBtInt (108) missing")
		return
	case err != nil || hb < 0:
		refuse("HeartBtInt (108) %s is not a number of seconds", quoteValue(m.Get(tagHeartBtInt)))
		return
	case !s.initiator && hb != 0 && (time.Duration(hb)*time.Second < s.cfg.MinHeartBtInt || time.Duration(hb)*time.Second > s.cfg.MaxHeartBtInt):
		refuse("HeartBtInt (108) %ds outside the accepted %s to %s", hb, s.cfg.MinHeartBtInt, s.cfg.MaxHeartBtInt)
		return
	}
	peerApplVerID := ""
	if s.prof.fixt {
		v := m.Get(tagDefaultApplVerID)
		if v == nil {
			refuse("DefaultApplVerID (1137) missing, required on a FIXT.1.1 Logon")
			return
		}
		peerApplVerID = string(v)
		if s.dicts.apps != nil && s.dicts.apps[peerApplVerID] == nil {
			have := strings.Join(slices.Sorted(maps.Keys(s.dicts.apps)), ", ")
			if s.cfg.Compatibility == CompatStrict2020 {
				refuse("DefaultApplVerID (1137) %s not supported, have %s", quoteValue(v), have)
				return
			}
			// As QuickFIX: accepted, its messages validated with our own
			// default version.
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "counterparty's DefaultApplVerID has no dictionary: our default is used for its messages",
				slog.String("default_appl_ver_id", peerApplVerID), slog.String("have", have))
			peerApplVerID = s.cfg.Dictionaries.DefaultApplVerID
		}
	}

	reset := s.prof.resetSeqNum && bytes.Equal(m.Get(tagResetSeqNumFlag), yes)
	switch {
	case !s.initiator && (reset || s.cfg.ResetOnLogon):
		s.resetStoreLocked("Logon with ResetSeqNumFlag")
	case s.initiator && reset && !s.sentReset:
		s.resetStoreLocked("Logon answer with ResetSeqNumFlag")
	case s.initiator && s.sentReset && !reset:
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "counterparty did not echo our ResetSeqNumFlag")
	}

	// NextExpectedMsgSeqNum: what the counterparty expects of us, compared
	// with what we sent before this Logon.
	// What the counterparty may expect of us: our next MsgSeqNum, an
	// initiator's own Logon included.
	nextBefore := s.st.NextSender
	peerNext := uint64(0)
	if s.cfg.NextExpectedMsgSeqNum && s.prof.nextExpected && !reset {
		if v, err := fix2.ParseInt(m.Get(tagNextExpectedMsgSeqNum)); err == nil && v > 0 {
			peerNext = uint64(v)
		} else if m.Get(tagNextExpectedMsgSeqNum) != nil {
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "malformed NextExpectedMsgSeqNum ignored", slog.String("value", string(m.Get(tagNextExpectedMsgSeqNum))))
		}
		if peerNext > nextBefore {
			s.sendLogoutLocked(c, fmt.Sprintf("Tag 789 (NextExpectedMsgSeqNum) is higher than expected. Expected %d, Received %d", nextBefore, peerNext))
			s.closeAfterFlushLocked(c, &LogoutError{Text: "counterparty expects messages we never sent"})
			return
		}
	}
	if in.Seq < s.st.NextTarget {
		s.sendLogoutLocked(c, fmt.Sprintf("MsgSeqNum too low, expecting %d but received %d", s.st.NextTarget, in.Seq))
		s.closeAfterFlushLocked(c, &LogoutError{Text: "Logon MsgSeqNum too low"})
		return
	}

	if s.initiator {
		s.heartBt = s.cfg.HeartBtInt
	} else {
		s.heartBt = time.Duration(hb) * time.Second
	}
	s.peerApplVerID = peerApplVerID
	if !s.initiator {
		next := in.Seq + 1
		if in.Seq > s.st.NextTarget {
			next = s.st.NextTarget
		}
		b := s.logonBody(reset, next)
		b = append(b, c.logonExtra...)
		s.sendAdminLocked(&header{msgType: "A"}, b)
	}
	s.sentReset = false
	s.phase = phaseEstablished
	c.loggedOn = true
	c.logonDeadline = time.Time{}
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "logged on",
		slog.Uint64("epoch", c.epoch), slog.Duration("heartbeat", s.heartBt), slog.Uint64("next_sender", s.st.NextSender), slog.Uint64("next_target", s.st.NextTarget))
	s.deliv.push(item{kind: itemLogon})

	if peerNext > 0 && peerNext < nextBefore {
		// Resend what the counterparty missed, gap-filling over our Logon,
		// which goes out first.
		s.startReplayLocked(c, peerNext, s.st.NextSender-1, true)
	}
	switch {
	case in.Seq == s.st.NextTarget:
		s.markLocked(in.Seq)
		s.advanceLocked(c, in.Seq+1)
	default:
		s.markAheadLocked(in.Seq)
		s.tooHighLocked(c, in, nil)
	}
}

// activeResetLocked handles a Logon with ResetSeqNumFlag on a logged on
// connection: both sides restart at sequence number 1, once the fence the
// reader runs next (fenceReset) has made sure nothing of the old generation
// is still being written or delivered. New messages are refused meanwhile.
func (s *Session) activeResetLocked(c *conn, m *fix2.Message, in Inbound) {
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "counterparty resets the sequence numbers on the established connection")
	s.resetting = true
	c.resetReq = true
	// What is not yet handed to the writer belongs to the old generation,
	// which the counterparty is leaving: dropped, as is any replay.
	c.replay, c.replayQueue = nil, nil
	c.pending, c.frames = c.pending[:0], c.frames[:0]
	c.held, c.heldFrames = c.held[:0], c.heldFrames[:0]
	c.signalRoomLocked()
	s.recov = recovery{}
	clear(s.stash)
}

// fenceReset completes a reset activeResetLocked started, from the reader
// goroutine: it waits for the messages received before to be delivered (at
// most ResetDrainTimeout, or the reset is refused), stops and joins the
// writer, then resets the store and answers the Logon with a new writer.
func (s *Session) fenceReset(c *conn) {
	select {
	case <-s.deliv.waitIdle():
	case <-time.After(s.cfg.ResetDrainTimeout):
		s.mu.Lock()
		defer s.mu.Unlock()
		c.resetReq, s.resetting = false, false
		s.logoutLocked(c, "sequence reset could not complete: messages received before are still being processed")
		return
	case <-c.done():
	}
	s.mu.Lock()
	c.writerStop = true
	c.wakeWriter()
	s.mu.Unlock()
	c.writers.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	c.resetReq, s.resetting, c.writerStop = false, false, false
	if c.closed || s.c != c {
		return
	}
	// Whatever the old writer's last writes queued meanwhile belongs to the
	// old generation too.
	c.pending, c.frames = c.pending[:0], c.frames[:0]
	c.held, c.heldFrames = c.held[:0], c.heldFrames[:0]
	s.resetStoreLocked("Logon with ResetSeqNumFlag on an established connection")
	s.epoch++
	c.epoch = s.epoch
	s.sendAdminLocked(&header{msgType: "A"}, s.logonBody(true, 2))
	s.markLocked(1)
	s.advanceLocked(c, 2)
	c.startWriterLocked()
}

// resetStoreLocked starts a new sequence number generation: both sides
// restart at 1. Receipts of the previous generation without evidence end with
// ErrUnknownDelivery.
func (s *Session) resetStoreLocked(why string, windowID ...string) error {
	gen := s.st.Generation
	id := s.st.WindowID
	if len(windowID) > 0 {
		id = windowID[0]
	}
	if err := s.store.Reset(context.Background(), gen, time.Now(), id); err != nil {
		s.log.LogAttrs(context.Background(), slog.LevelError, "resetting the store failed", slog.Any("error", err))
		if s.c != nil {
			s.disconnectLocked(s.c, &LogoutError{Text: "store reset failed", Err: err})
		}
		return fmt.Errorf("session %s: resetting the store (%s): %w", s.id, why, err)
	}
	st, err := s.store.Open(context.Background())
	if err != nil {
		st = s.st
		st.Generation, st.NextSender, st.NextTarget = gen+1, 1, 1
	}
	s.st = st
	s.committed = 0
	s.recov = recovery{}
	clear(s.stash)
	s.receipts.newGeneration(gen, st.Generation)
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "sequence numbers reset", slog.String("why", why), slog.Uint64("generation", st.Generation))
	return nil
}
