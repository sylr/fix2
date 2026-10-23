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

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/session/store"
)

// logonReserved reports whether tag is one of a Logon the session writes, on
// top of the reserved ones (see Session.reservedTag): a ToAdmin callback
// cannot set them.
func (s *Session) logonReserved(tag int) bool {
	switch tag {
	case tagEncryptMethod, tagHeartBtInt, tagResetSeqNumFlag, tagNextExpectedMsgSeqNum, tagDefaultApplVerID:
		return true
	}
	return s.reservedTag(tag)
}

// logoutReserved reports whether tag is one of a Logout the session writes,
// on top of the reserved ones: Text (58), which the reason logging out
// gives.
func (s *Session) logoutReserved(tag int) bool {
	return tag == tagText || s.reservedTag(tag)
}

// toAdmin asks the handler for the extra fields of a Logon or Logout.
func (s *Session) toAdmin(msgType string) ([]byte, error) {
	b := fix.NewBuilder(s.id.BeginString, msgType)
	if err := s.handler.ToAdmin(s.callbackCtx(), s, msgType, b); err != nil {
		return nil, err
	}
	reserved := s.logonReserved
	if msgType == "5" {
		reserved = s.logoutReserved
	}
	return b.AppendBody(nil, s.dicts.adminProfile(), reserved)
}

// adminExtras asks the handler for the extra fields of a connection's
// Logons and Logouts, before its first Logon is sent or answered: the
// Logouts are sent under the lock, which no callback runs under.
func (s *Session) adminExtras() (logon, logout []byte, err error) {
	if logon, err = s.toAdmin("A"); err != nil {
		return nil, nil, err
	}
	if logout, err = s.toAdmin("5"); err != nil {
		return nil, nil, err
	}
	return logon, logout, nil
}

// logon sends the initiator's Logon on c.
func (s *Session) logon(c *conn) error {
	extra, logoutExtra, err := s.adminExtras()
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
	// Kept for the answer to a reset Logon (see fenceReset) and the
	// Logouts, the signature apart: it covers this Logon only.
	c.logonExtra, c.logoutExtra = extra, logoutExtra
	s.sentReset = false
	if s.cfg.ResetOnLogon {
		if err := s.resetStoreLocked("ResetOnLogon"); err != nil {
			return err
		}
		s.sentReset = true
	}
	h := &header{msgType: "A", sendingTime: s.now()}
	if signer, ok := s.handler.(LogonSigner); ok {
		// The signature covers the MsgSeqNum and SendingTime: both are
		// fixed before the handler signs, outside the lock, and the
		// MsgSeqNum checked after. Nothing else sends before a Logon.
		seq := s.st.NextSender
		s.mu.Unlock()
		sig, err := s.signLogon(signer, seq, h.sendingTime)
		s.mu.Lock()
		if c.closed || s.c != c {
			return c.reason
		}
		if err == nil && s.st.NextSender != seq {
			err = fmt.Errorf("MsgSeqNum changed from %d to %d while the Logon was signed", seq, s.st.NextSender)
		}
		if err != nil {
			s.disconnectLocked(c, &LogoutError{Text: "Logon signature failed", Err: err})
			return err
		}
		extra = append(slices.Clip(extra), sig...)
	}
	b := s.logonBody(s.sentReset, s.st.NextTarget)
	b = append(b, extra...)
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "sending Logon",
		slog.Uint64("seq", s.st.NextSender), slog.Bool("reset", s.sentReset))
	s.sendAdminLocked(h, b)
	return nil
}

// signLogon asks signer for the signature fields of the Logon with MsgSeqNum
// seq sent at sendingTime.
func (s *Session) signLogon(signer LogonSigner, seq uint64, sendingTime time.Time) ([]byte, error) {
	h := LogonHeader{
		BeginString:  s.id.BeginString,
		SenderCompID: s.id.SenderCompID,
		TargetCompID: s.id.TargetCompID,
		MsgSeqNum:    seq,
		SendingTime:  string(fix.AppendUTCTimestamp(nil, sendingTime, s.prec)),
	}
	b := fix.NewBuilder(s.id.BeginString, "A")
	if err := signer.SignLogon(s.callbackCtx(), s, h, b); err != nil {
		return nil, err
	}
	return b.AppendBody(nil, s.dicts.adminProfile(), s.logonReserved)
}

// logonBody returns the fields of our Logon: next is the NextExpectedMsgSeqNum
// to announce, if used.
func (s *Session) logonBody(reset bool, next uint64) adminBody {
	b := adminBody(nil).str(tagEncryptMethod, s.encryptMethod()).int(tagHeartBtInt, uint64(s.heartBt/time.Second))
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
func (s *Session) onLogonLocked(c *conn, m *fix.Message, in Inbound, now time.Time) {
	phase := s.phase
	if c.closing {
		// Our last Logout queued, a stalled handler's leaving the phase
		// established: a reset would drop it, and its answer go after it.
		phase = phaseLogoutSent
	}
	switch phase {
	case phaseLogoutSent:
		// Logging out, FIX lets us send nothing but what a ResendRequest
		// asks for: neither a Logon answering it nor a sequence reset.
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "Logon ignored while logging out",
			slog.Bool("reset", bytes.Equal(m.Get(tagResetSeqNumFlag), yes)))
		return
	case phaseEstablished:
		if !bytes.Equal(m.Get(tagResetSeqNumFlag), yes) || !s.prof.resetSeqNum {
			s.logoutLocked(c, "Logon received while already logged on")
			return
		}
		if p := s.resetLogonProblem(m, now); p != nil {
			// Rejected, and counted if it is the one expected, as a message
			// with a wrong header is: nothing is reset.
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "Logon with ResetSeqNumFlag refused", slog.String("reason", p.text))
			s.rejectLocked(m, p.reason, p.tag, p.text)
			if in.Seq == s.st.NextTarget {
				s.markLocked(in.Seq)
				s.st.NextTarget = in.Seq + 1
			}
			s.logoutLocked(c, "Logon with ResetSeqNumFlag refused")
			return
		}
		if v := m.Get(tagDefaultApplVerID); s.prof.fixt && string(v) != s.peerApplVerID && (s.dicts.apps == nil || s.dicts.apps[string(v)] != nil) {
			// What was negotiated at logon holds until the next.
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "counterparty's DefaultApplVerID changed on a sequence reset: the one of the logon is kept",
				slog.String("default_appl_ver_id", withhold(m, tagDefaultApplVerID, string(v), "withheld")), slog.String("kept", s.peerApplVerID))
		}
		s.activeResetLocked(c, m, in)
		return
	}

	// Errors before the session is logged on disconnect without a word, as
	// FIX requires: a Logout would take a sequence number, and tell an
	// unknown peer what is wrong.
	hb, peerApplVerID, p := s.checkLogon(m, now)
	if p != nil {
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "Logon refused", slog.String("reason", p.text))
		s.disconnectLocked(c, &LogoutError{Text: "Logon refused: " + p.text})
		return
	}
	if s.initiator {
		// The answer agrees on our EncryptMethod (see checkLogon): what the
		// counterparty sends after it is sealed, and so is what we send.
		c.encrypted = s.cfg.Encrypter != nil
	}
	if s.prof.fixt && s.dicts.apps != nil && s.dicts.apps[peerApplVerID] == nil {
		// TolerateUnsupportedDefaultApplVerID, as QuickFIX: accepted, its
		// messages validated with our own default version.
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "counterparty's DefaultApplVerID has no dictionary: our default is used for its messages",
			slog.String("default_appl_ver_id", withhold(m, tagDefaultApplVerID, peerApplVerID, "withheld")), slog.String("have", strings.Join(slices.Sorted(maps.Keys(s.dicts.apps)), ", ")))
		peerApplVerID = s.cfg.Dictionaries.DefaultApplVerID
	}

	reset := s.prof.resetSeqNum && bytes.Equal(m.Get(tagResetSeqNumFlag), yes)
	// announce tells an acceptor resetting for ResetOnLogon, unasked, unless
	// UnannouncedResetOnLogon, that the reset moves its sequence numbers:
	// its answer says so with ResetSeqNumFlag, and the counterparty,
	// resetting in turn,
	// restarts at 1, its Logon left in the sequence reset. One at the start
	// of its sequence, facing a counterparty at the start of its own, has
	// nothing to announce: its answer is that of any first Logon. A Logon
	// above 1, or expecting more than our first message, tells the
	// counterparty is not. QuickFIX never announces, and its acceptance tests
	// expect it not to.
	announce := false
	var resetErr error
	switch {
	case !s.initiator && reset:
		resetErr = s.resetStoreLocked("Logon with ResetSeqNumFlag")
	case !s.initiator && s.cfg.ResetOnLogon:
		next, _ := fix.ParseInt(m.Get(tagNextExpectedMsgSeqNum))
		announce = !s.cfg.UnannouncedResetOnLogon && s.prof.resetSeqNum &&
			(s.st.NextSender > 1 || s.st.NextTarget > 1 || in.Seq > 1 || next > 1)
		resetErr = s.resetStoreLocked("ResetOnLogon")
	case s.initiator && reset && !s.sentReset:
		resetErr = s.resetStoreLocked("Logon answer with ResetSeqNumFlag")
	case s.initiator && s.sentReset && !reset:
		// Some venues, Binance for one, never do: their MsgSeqNum, checked
		// next, tells whether they reset.
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "counterparty did not echo our ResetSeqNumFlag")
	}
	if resetErr != nil {
		// Disconnected: not logged on.
		return
	}

	// NextExpectedMsgSeqNum: what the counterparty expects of us, compared
	// with what we sent before this Logon.
	// What the counterparty may expect of us: our next MsgSeqNum, an
	// initiator's own Logon included.
	nextBefore := s.st.NextSender
	peerNext := uint64(0)
	if s.cfg.NextExpectedMsgSeqNum && s.prof.nextExpected && !reset && !announce {
		if v, err := fix.ParseInt(m.Get(tagNextExpectedMsgSeqNum)); err == nil && v > 0 {
			peerNext = uint64(v)
		} else if m.Get(tagNextExpectedMsgSeqNum) != nil {
			// TolerateMalformedNextExpectedMsgSeqNum: checkLogon refuses it
			// otherwise.
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "malformed NextExpectedMsgSeqNum ignored",
				slog.String("value", withhold(m, tagNextExpectedMsgSeqNum, quoteValue(m.Get(tagNextExpectedMsgSeqNum)), "withheld")))
		}
		if peerNext > nextBefore {
			s.sendLogoutReqLocked(c, logoutReq{text: withhold(m, tagNextExpectedMsgSeqNum,
				fmt.Sprintf("Tag 789 (NextExpectedMsgSeqNum) is higher than expected. Expected %d, Received %d", nextBefore, peerNext),
				"Tag 789 (NextExpectedMsgSeqNum) is higher than expected"), status: statusNextTooHigh})
			s.closeAfterFlushLocked(c, &LogoutError{Text: "counterparty expects messages we never sent"})
			return
		}
	}
	if in.Seq < s.st.NextTarget {
		s.sendLogoutReqLocked(c, logoutReq{text: s.seqTooLowText(m, s.st.NextTarget, in.Seq), status: statusSeqTooLow})
		s.closeAfterFlushLocked(c, &LogoutError{Text: "Logon MsgSeqNum too low"})
		return
	}
	// 789, what the counterparty expects next, prevails over its 369. Both
	// refer to the sequence left behind when a reset is announced.
	if !announce {
		s.lastProcessedLocked(m, in)
	}
	if peerNext > 0 {
		s.ackedLocked(m, peerNext-1, tagNextExpectedMsgSeqNum, in.Seq)
	}

	if s.initiator {
		s.setHeartBtLocked(s.cfg.HeartBtInt)
	} else {
		s.setHeartBtLocked(time.Duration(hb) * time.Second)
	}
	s.peerApplVerID = peerApplVerID
	if !s.initiator {
		next := in.Seq + 1
		if announce || in.Seq > s.st.NextTarget {
			next = s.st.NextTarget
		}
		b := s.logonBody(reset || announce, next)
		b = append(b, c.logonExtra...)
		// Our answer agrees on the counterparty's EncryptMethod (see
		// checkLogon): what follows it, both ways, is sealed.
		c.encrypted = s.cfg.Encrypter != nil
		s.sendAdminLocked(&header{msgType: "A"}, b)
		if c.closed || s.c != c {
			// The answer could not be stored: disconnected, not logged on.
			return
		}
	}
	s.sentReset = false
	s.phase = phaseEstablished
	c.loggedOn = true
	c.logonDeadline = time.Time{}
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "logged on",
		slog.Uint64("epoch", c.epoch), slog.String("heartbeat", withhold(m, tagHeartBtInt, s.heartBt.String(), "withheld")), slog.Uint64("next_sender", s.st.NextSender), slog.Uint64("next_target", s.st.NextTarget))
	s.warnUnauthenticatedLocked(c)
	s.deliv.push(item{kind: itemLogon})

	if peerNext > 0 && peerNext < nextBefore {
		// Resend what the counterparty missed, gap-filling over our Logon,
		// which goes out first.
		s.startReplayLocked(c, peerNext, s.st.NextSender-1, true)
	}
	switch {
	case announce:
		// The counterparty's Logon belongs to the sequence reset: its next
		// message is 1.
	case in.Seq == s.st.NextTarget:
		s.markLocked(in.Seq)
		s.advanceLocked(c, in.Seq+1)
	default:
		s.markAheadLocked(in.Seq)
		s.tooHighLocked(c, m, in, nil)
	}
}

// logonProblem is what makes a Logon invalid: the reason and tag of the
// Reject an established session sends for it, and a text.
type logonProblem struct {
	reason dictionary.RejectReason
	tag    int
	text   string
}

// checkLogon checks the fields of Logon m the session relies on: CompIDs,
// SendingTime, EncryptMethod, HeartBtInt, for FIXT DefaultApplVerID,
// NextExpectedMsgSeqNum if the session honours it, and a MsgSeqNum of 1 with
// ResetSeqNumFlag; then the rest of m against the session dictionary (see
// validateAdmin), after the checks whose texts say more.
// FIX requires EncryptMethod of every Logon: an acceptor requires it of the
// counterparty's, as an initiator does of the Logon answer unless
// TolerateLogonWithoutEncryptMethod, as some venues leave it out (Binance's
// dictionary has it optional), and never with an Encrypter; a reset Logon on
// an established session always needs it (see resetLogonProblem). It must
// be the one our Logons declare (see Encrypter). It returns the HeartBtInt and the
// DefaultApplVerID, which may have no dictionary with
// TolerateUnsupportedDefaultApplVerID, or the problem, its text withheld if
// it may show a credential: it is logged.
func (s *Session) checkLogon(m *fix.Message, now time.Time) (int, string, *logonProblem) {
	problem := func(reason dictionary.RejectReason, tag int, format string, args ...any) (int, string, *logonProblem) {
		return 0, "", newLogonProblem(m, reason, tag, rejectText(reason, fmt.Sprintf(format, args...)))
	}
	if reason, tag, text := s.compIDOrTimeProblem(m, now); text != "" {
		return 0, "", newLogonProblem(m, reason, tag, text)
	}
	method, none := s.encryptMethod(), " (none)"
	if s.cfg.Encrypter != nil {
		none = ""
	}
	switch v := m.Get(tagEncryptMethod); {
	case v == nil && (!s.initiator || !s.cfg.TolerateLogonWithoutEncryptMethod || s.cfg.Encrypter != nil):
		return problem(dictionary.ReasonRequiredTagMissing, tagEncryptMethod, "%s", fieldDesc(nil, tagEncryptMethod))
	case v != nil && string(v) != method:
		return problem(dictionary.ReasonValueIsIncorrect, tagEncryptMethod, "EncryptMethod (98) %s not supported, only %s%s", quoteValue(v), method, none)
	}
	v := m.Get(tagHeartBtInt)
	hb, err := fix.ParseInt(v)
	switch {
	case v == nil:
		return problem(dictionary.ReasonRequiredTagMissing, tagHeartBtInt, "%s", fieldDesc(nil, tagHeartBtInt))
	case err != nil:
		return problem(dictionary.ReasonIncorrectDataFormat, tagHeartBtInt, "HeartBtInt (108) %s is not a number of seconds", quoteValue(v))
	case hb < 0:
		return problem(dictionary.ReasonValueIsIncorrect, tagHeartBtInt, "HeartBtInt (108) %s is not a number of seconds", quoteValue(v))
	case !s.initiator && hb != 0 && (time.Duration(hb)*time.Second < s.cfg.MinHeartBtInt || time.Duration(hb)*time.Second > s.cfg.MaxHeartBtInt):
		return problem(dictionary.ReasonValueIsIncorrect, tagHeartBtInt, "HeartBtInt (108) %ds outside the accepted %s to %s", hb, s.cfg.MinHeartBtInt, s.cfg.MaxHeartBtInt)
	}
	var peerApplVerID string
	if s.prof.fixt {
		v = m.Get(tagDefaultApplVerID)
		switch {
		case v == nil:
			return problem(dictionary.ReasonRequiredTagMissing, tagDefaultApplVerID, "%s", fieldDesc(nil, tagDefaultApplVerID))
		case s.dicts.apps != nil && s.dicts.apps[string(v)] == nil && !s.cfg.TolerateUnsupportedDefaultApplVerID:
			return problem(dictionary.ReasonValueIsIncorrect, tagDefaultApplVerID, "DefaultApplVerID (1137) %s not supported, have %s",
				quoteValue(v), strings.Join(slices.Sorted(maps.Keys(s.dicts.apps)), ", "))
		}
		peerApplVerID = string(v)
	}
	if next := m.Get(tagNextExpectedMsgSeqNum); next != nil && s.cfg.NextExpectedMsgSeqNum && s.prof.nextExpected &&
		!s.cfg.TolerateMalformedNextExpectedMsgSeqNum {
		if n, err := fix.ParseInt(next); err != nil || n <= 0 {
			return problem(dictionary.ReasonIncorrectDataFormat, tagNextExpectedMsgSeqNum, "NextExpectedMsgSeqNum (789) %s is not a positive integer", quoteValue(next))
		}
	}
	if s.prof.resetSeqNum && !s.cfg.TolerateResetLogonSeqNumNotOne {
		if p := resetSeqNumProblem(m); p != nil {
			return 0, "", p
		}
	}
	if verr := s.validateAdmin(m); verr != nil {
		return 0, "", newLogonProblem(m, verr.Reason, verr.Tag, rejectText(verr.Reason, verr.Msg))
	}
	return hb, peerApplVerID, nil
}

// resetLogonProblem checks Logon m with ResetSeqNumFlag, received on an
// established session, as an acceptor checks a first Logon, EncryptMethod
// required, and its MsgSeqNum: FIX requires 1, both sides restarting there,
// whatever TolerateResetLogonSeqNumNotOne.
func (s *Session) resetLogonProblem(m *fix.Message, now time.Time) *logonProblem {
	if _, _, p := s.checkLogon(m, now); p != nil {
		return p
	}
	if m.Get(tagEncryptMethod) == nil {
		return newLogonProblem(m, dictionary.ReasonRequiredTagMissing, tagEncryptMethod,
			rejectText(dictionary.ReasonRequiredTagMissing, fieldDesc(nil, tagEncryptMethod)))
	}
	return resetSeqNumProblem(m)
}

// resetSeqNumProblem returns the problem of Logon m if it has ResetSeqNumFlag
// (141) Y and a MsgSeqNum other than 1: a reset starts a new FIX session, and
// FIX starts one at 1.
func resetSeqNumProblem(m *fix.Message) *logonProblem {
	if !bytes.Equal(m.Get(tagResetSeqNumFlag), yes) {
		return nil
	}
	v := m.Get(tagMsgSeqNum)
	if seq, err := fix.ParseInt(v); err != nil || seq != 1 {
		return newLogonProblem(m, dictionary.ReasonValueIsIncorrect, tagMsgSeqNum,
			rejectText(dictionary.ReasonValueIsIncorrect, "MsgSeqNum (34) "+quoteValue(v)+" of a Logon with ResetSeqNumFlag, expected 1"))
	}
	return nil
}

// newLogonProblem returns the problem of Logon m about its field tag, its
// text withheld if it may show a credential.
func newLogonProblem(m *fix.Message, reason dictionary.RejectReason, tag int, text string) *logonProblem {
	return &logonProblem{reason, tag, withhold(m, tag, text, rejectText(reason, "value withheld"))}
}

// activeResetLocked handles a Logon with ResetSeqNumFlag on a logged on
// connection: both sides restart at sequence number 1, once the fence the
// reader runs next (fenceReset) has made sure nothing of the old generation
// is still being written or delivered. New messages are refused meanwhile.
func (s *Session) activeResetLocked(c *conn, m *fix.Message, in Inbound) {
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "counterparty resets the sequence numbers on the established connection")
	s.resetting = true
	c.resetReq = true
	// What is not yet handed to the writer belongs to the old generation,
	// which the counterparty is leaving: dropped, as is any replay, but
	// kept from eviction should the reset be refused.
	s.unsentLocked(c)
	c.replay, c.replayQueue = nil, nil
	c.pending, c.frames = reusable(c.pending, s.cfg.MaxPendingBytes), c.frames[:0]
	c.held, c.heldFrames = reusable(c.held, s.cfg.MaxPendingBytes), c.heldFrames[:0]
	c.signalRoomLocked()
	s.recov = recovery{}
	s.clearStashLocked()
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
	c.pending, c.frames = reusable(c.pending, s.cfg.MaxPendingBytes), c.frames[:0]
	c.held, c.heldFrames = reusable(c.held, s.cfg.MaxPendingBytes), c.heldFrames[:0]
	if err := s.resetStoreLocked("Logon with ResetSeqNumFlag on an established connection"); err != nil {
		// Disconnected: not answered.
		return
	}
	s.epoch++
	c.epoch = s.epoch
	b := s.logonBody(true, 2)
	b = append(b, c.logonExtra...)
	s.sendAdminLocked(&header{msgType: "A"}, b)
	if c.closed || s.c != c {
		// The answer could not be stored: disconnected, not answered.
		return
	}
	s.markLocked(1)
	s.advanceLocked(c, 2)
	c.startWriterLocked()
}

// resetStoreLocked starts a new sequence number generation: both sides
// restart at 1. Receipts of the previous generation without evidence end with
// ErrUnknownDelivery. A failure disconnects; one leaving the store's
// generation uncertain halts the session, as does failing to reload the
// state once reset, or reloading a generation not later than the one reset:
// the session's own is stale, and it does not guess.
func (s *Session) resetStoreLocked(why string, windowID ...string) error {
	gen := s.st.Generation
	id := s.st.WindowID
	if len(windowID) > 0 {
		id = windowID[0]
	}
	if err := s.storeLocked(func() error { return s.store.Reset(context.Background(), gen, s.now(), id) }); err != nil {
		s.log.LogAttrs(context.Background(), slog.LevelError, "resetting the store failed", slog.String("why", why), slog.Any("error", err))
		err = fmt.Errorf("resetting the store (%s): %w", why, err)
		if uncertain(err) {
			s.haltLocked(err)
		}
		if s.c != nil {
			s.disconnectLocked(s.c, &LogoutError{Text: "store reset failed", Err: err})
		}
		return fmt.Errorf("session %s: %w", s.id, err)
	}
	var st store.State
	err := s.storeLocked(func() (err error) {
		st, err = s.store.Open(context.Background())
		return err
	})
	if err == nil && st.Generation <= gen {
		// The store broke Reset's contract: the receipts of the generation
		// ended would read those of the next.
		err = fmt.Errorf("generation %d after resetting generation %d, not a later one", st.Generation, gen)
	}
	if err != nil {
		err = fmt.Errorf("reloading the state of the store reset (%s): %w", why, err)
		s.haltLocked(err)
		if s.c != nil {
			s.disconnectLocked(s.c, &LogoutError{Text: "store reset failed", Err: err})
		}
		return fmt.Errorf("session %s: %w", s.id, err)
	}
	s.st = st
	s.resetOwed = ""
	s.committed = 0
	s.recov = recovery{}
	s.clearStashLocked()
	s.receipts.newGeneration(gen, st.Generation)
	s.ret.newGeneration()
	if s.deliv != nil {
		// The messages of the previous generation held behind a failure to
		// record them are delivered: recording them is moot now.
		s.deliv.holdLocked(0)
	}
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "sequence numbers reset", slog.String("why", why), slog.Uint64("generation", st.Generation))
	return nil
}
