package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/session/transport"
)

// recovery tracks the messages missing before one received too early.
type recovery struct {
	active bool
	// target is the highest MsgSeqNum received so far: everything below it
	// is to be recovered.
	target uint64
	// chunkEnd is the end of the range last asked for, 0 for "up to the
	// last".
	chunkEnd uint64
}

// handleFrame handles a frame the reader read.
func (c *conn) handleFrame(frame []byte) {
	s := c.s
	s.waitDeliveryRoom(c)
	now := time.Now()
	m := &c.msg
	if err := fix2.ParseInto(m, frame, s.dicts.parse); err != nil {
		if tag, ok := badTagNumber(frame, m, err); ok {
			s.mu.Lock()
			if s.c == c && !c.closed && s.phase == phaseEstablished {
				c.lastRead = now
				s.invalidTagLocked(c, m, tag)
				s.armLocked(c)
			}
			s.mu.Unlock()
			return
		}
		c.garbled(frame, err)
		return
	}
	if p := s.appProfile(m); p != nil && p != s.dicts.parse {
		if err := fix2.ParseInto(m, frame, p); err != nil {
			c.garbled(frame, err)
			return
		}
	}
	s.stats.MessagesReceived.Add(1)
	s.stats.BytesReceived.Add(uint64(len(frame)))
	if obs := s.cfg.Observer; obs != nil {
		seq, _ := fix2.ParseInt(m.Get(tagMsgSeqNum))
		obs.OnInbound(s, Frame{Raw: frame, Seq: uint64(max(seq, 0)), Generation: s.generation(), Epoch: c.epoch, Resend: bytes.Equal(m.Get(tagPossDupFlag), yes)})
	}

	// A Logon is authenticated before the session acts on it.
	if bytes.Equal(m.MsgType(), msgLogon) {
		s.mu.Lock()
		ph, current := s.phase, s.c == c && !c.closed
		s.mu.Unlock()
		if !current {
			return
		}
		if ph == phaseAwaitingLogon || ph == phaseLogonSent || bytes.Equal(m.Get(tagResetSeqNumFlag), yes) {
			if err := s.authenticate(c, m); err != nil {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.log.LogAttrs(context.Background(), slog.LevelWarn, "Logon refused by the handler", slog.Any("error", err))
				if ph == phaseEstablished {
					s.logoutLocked(c, "Logon rejected")
				} else {
					s.disconnectLocked(c, &LogoutError{Text: "Logon refused by the handler", Err: err})
				}
				return
			}
		}
	}

	s.mu.Lock()
	if s.c != c || c.closed {
		s.mu.Unlock()
		return
	}
	c.lastRead = now
	in, fromAdmin := s.onMessageLocked(c, m, frame, now)
	if !c.closed {
		s.armLocked(c)
	}
	fence := c.resetReq
	s.mu.Unlock()
	if fence {
		s.fenceReset(c)
	}
	if fromAdmin {
		s.handler.FromAdmin(s.callbackCtx(), s, m, in)
	}
}

var (
	yes       = []byte("Y")
	msgLogon  = []byte("A")
	msgLogout = []byte("5")
	msgReject = []byte("3")
)

// badTagNumber tells whether frame failed to parse only for a malformed tag
// number after its header, its CheckSum being right: such a message is
// rejected, not ignored as garbled. It returns the tag, as an integer when
// it is one (negative ones included).
func badTagNumber(frame []byte, m *fix2.Message, err error) (int, bool) {
	var pe *fix2.ParseError
	if !errors.As(err, &pe) || !errors.Is(err, fix2.ErrInvalidTag) || m.MsgType() == nil || m.Get(tagMsgSeqNum) == nil {
		return 0, false
	}
	n := len(frame)
	if n < 7 || string(frame[n-7:n-4]) != "10=" {
		return 0, false
	}
	want, cerr := strconv.Atoi(string(frame[n-4 : n-1]))
	if cerr != nil || int(fix2.CheckSum(frame[:n-7])) != want {
		return 0, false
	}
	end := bytes.IndexByte(frame[pe.Offset:], '=')
	if end < 0 {
		return noTag, true
	}
	tag, aerr := strconv.Atoi(string(frame[pe.Offset : pe.Offset+end]))
	if aerr != nil {
		return noTag, true
	}
	return tag, true
}

// invalidTagLocked rejects m, whose tag is not a valid tag number, and counts
// it as received if it is the one expected.
func (s *Session) invalidTagLocked(c *conn, m *fix2.Message, tag int) {
	seq, err := fix2.ParseInt(m.Get(tagMsgSeqNum))
	if err != nil || uint64(seq) != s.st.NextTarget {
		return
	}
	s.rejectLocked(m, dictionary.ReasonInvalidTagNumber, tag, rejectText(dictionary.ReasonInvalidTagNumber, "malformed tag number"))
	s.markLocked(uint64(seq))
	s.advanceLocked(c, uint64(seq)+1)
}

// garbled handles a frame that does not parse: FIX ignores it, without
// changing sequence numbers.
func (c *conn) garbled(frame []byte, err error) {
	c.s.stats.Garbled.Add(1)
	c.s.log.LogAttrs(context.Background(), slog.LevelWarn, "garbled message ignored",
		slog.String("error", err.Error()), slog.String("frame", string(frame[:min(len(frame), 256)])))
}

// authenticate asks the handler whether to accept Logon m.
func (s *Session) authenticate(c *conn, m *fix2.Message) error {
	req := &LogonRequest{ID: s.id, Logon: m, Response: s.initiator, RemoteAddr: c.tc.RemoteAddr()}
	if tc, ok := c.tc.(transport.TLSConn); ok {
		st := tc.ConnectionState()
		req.TLS = &st
	}
	if pc, ok := c.tc.(transport.PrincipalConn); ok {
		req.Principal = pc.Principal()
	}
	return s.handler.Authenticate(s.callbackCtx(), req)
}

// callbackCtx returns the context passed to handler callbacks.
func (s *Session) callbackCtx() context.Context {
	return s.cbCtx
}

func (s *Session) generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Generation
}

// appProfile returns the parsing profile of a FIXT application message whose
// version is not the default, or nil.
func (s *Session) appProfile(m *fix2.Message) *fix2.Profile {
	if s.dicts.parseApps == nil {
		return nil
	}
	v := m.Get(tagApplVerID)
	if v == nil {
		s.mu.Lock()
		peer := s.peerApplVerID
		s.mu.Unlock()
		if peer == "" {
			return nil
		}
		return s.dicts.parseApps[peer]
	}
	return s.dicts.parseApps[string(v)]
}

// onMessageLocked handles received message m, frame its bytes, on c. It
// returns what to tell the handler's FromAdmin, if anything.
func (s *Session) onMessageLocked(c *conn, m *fix2.Message, frame []byte, now time.Time) (Inbound, bool) {
	msgType := m.MsgType()
	if string(m.BeginString()) != s.id.BeginString {
		if s.phase == phaseAwaitingLogon || s.phase == phaseLogonSent {
			s.disconnectLocked(c, &LogoutError{Text: fmt.Sprintf("Logon with BeginString %s, expected %s", m.BeginString(), s.id.BeginString)})
			return Inbound{}, false
		}
		s.logoutLocked(c, fmt.Sprintf("Incorrect BeginString: expected %s, received %s", s.id.BeginString, m.BeginString()))
		return Inbound{}, false
	}
	seq, err := fix2.ParseInt(m.Get(tagMsgSeqNum))
	resetMode := string(msgType) == "4" && !bytes.Equal(m.Get(tagGapFillFlag), yes)
	if resetMode && (err != nil || seq < 0) {
		seq, err = 0, nil // ignored in Reset mode
	}
	if err != nil || seq <= 0 && !resetMode {
		text := "MsgSeqNum (34) missing"
		if m.Get(tagMsgSeqNum) != nil {
			text = "MsgSeqNum (34) is not a positive integer: " + quoteValue(m.Get(tagMsgSeqNum))
		}
		if s.phase == phaseAwaitingLogon || s.phase == phaseLogonSent {
			s.disconnectLocked(c, &LogoutError{Text: text})
		} else {
			s.logoutLocked(c, text)
		}
		return Inbound{}, false
	}
	in := Inbound{
		Seq:        uint64(seq),
		Generation: s.st.Generation,
		PossDup:    bytes.Equal(m.Get(tagPossDupFlag), yes),
		PossResend: bytes.Equal(m.Get(tagPossResend), yes),
		Received:   now,
	}
	s.replyTo = in.Seq
	defer func() { s.replyTo = 0 }()

	switch s.phase {
	case phaseAwaitingLogon, phaseLogonSent:
		if bytes.Equal(msgType, msgLogout) || bytes.Equal(msgType, msgReject) {
			// The counterparty refuses to log on, with a Logout or, as
			// some venues do, a Reject of our Logon: the handler sees it,
			// and the reason tells why.
			s.disconnectLocked(c, &LogoutError{ByCounterparty: true, Text: logonRefusal(m)})
			return in, true
		}
		if !bytes.Equal(msgType, msgLogon) {
			s.disconnectLocked(c, &LogoutError{Text: fmt.Sprintf("first message is MsgType %s, not a Logon", msgType)})
			return in, false
		}
		s.onLogonLocked(c, m, in, now)
		return in, s.c == c
	}

	switch string(msgType) {
	case "A":
		s.onLogonLocked(c, m, in, now)
		return in, true
	case "5":
		s.onLogoutLocked(c, m, frame, in, now)
		return in, true
	case "2":
		s.onResendRequestLocked(c, m, in, now)
		return in, true
	case "4":
		s.onSequenceResetLocked(c, m, frame, in, now)
		return in, true
	}
	return s.onRegularLocked(c, m, frame, in, now)
}

// checkHeaderLocked checks the CompIDs and SendingTime of an established
// session's message, received with sequence number seq. On a problem it
// rejects the message and returns false: a wrong CompID or an inaccurate
// SendingTime then logs out, as FIX requires; a missing or malformed field
// counts the message as received if it is the one expected.
func (s *Session) checkHeaderLocked(c *conn, m *fix2.Message, seq uint64, now time.Time) bool {
	reason, tag, text := s.compIDOrTimeProblem(m, now)
	if text == "" {
		return true
	}
	s.rejectLocked(m, reason, tag, text)
	switch reason {
	case dictionary.ReasonCompIDProblem, dictionary.ReasonSendingTimeAccuracyProblem:
		s.logoutLocked(c, "")
	default:
		if seq == s.st.NextTarget {
			s.markLocked(seq)
			s.advanceLocked(c, seq+1)
		}
	}
	return false
}

// compIDOrTimeProblem returns the first problem of m's CompIDs or SendingTime.
func (s *Session) compIDOrTimeProblem(m *fix2.Message, now time.Time) (dictionary.RejectReason, int, string) {
	if reason, text := s.compIDProblem(m); text != "" {
		return reason, compIDTag(reason, m), text
	}
	return s.sendingTimeProblem(m, now)
}

// compIDProblem checks the counterparty's identity on m.
func (s *Session) compIDProblem(m *fix2.Message) (dictionary.RejectReason, string) {
	for _, f := range []struct {
		tag  int
		want string
	}{
		{tagSenderCompID, s.id.TargetCompID}, {tagTargetCompID, s.id.SenderCompID},
		{tagSenderSubID, s.id.TargetSubID}, {tagTargetSubID, s.id.SenderSubID},
		{tagSenderLocationID, s.id.TargetLocationID}, {tagTargetLocationID, s.id.SenderLocationID},
	} {
		v := m.Get(f.tag)
		switch {
		case v == nil && (f.tag == tagSenderCompID || f.tag == tagTargetCompID):
			return dictionary.ReasonRequiredTagMissing, rejectText(dictionary.ReasonRequiredTagMissing, fieldDesc(nil, f.tag))
		case v != nil && len(v) == 0:
			return dictionary.ReasonTagSpecifiedWithoutAValue, rejectText(dictionary.ReasonTagSpecifiedWithoutAValue, fieldDesc(nil, f.tag))
		case f.want == "" && v == nil:
		case string(v) != f.want:
			return dictionary.ReasonCompIDProblem, rejectText(dictionary.ReasonCompIDProblem,
				fmt.Sprintf("%s is %s, expected %q", fieldDesc(nil, f.tag), quoteValue(v), f.want))
		}
	}
	return 0, ""
}

// noTag is a reject's tag when it refers to none: tags 0 and -1 can be the
// one, received as malformed tag numbers.
const noTag = math.MinInt

// compIDTag returns the tag a CompID problem refers to, for RefTagID: none
// for a mismatch, as QuickFIX sends.
func compIDTag(reason dictionary.RejectReason, m *fix2.Message) int {
	switch reason {
	case dictionary.ReasonRequiredTagMissing:
		if m.Get(tagSenderCompID) == nil {
			return tagSenderCompID
		}
		return tagTargetCompID
	case dictionary.ReasonTagSpecifiedWithoutAValue:
		if v := m.Get(tagSenderCompID); v != nil && len(v) == 0 {
			return tagSenderCompID
		}
		return tagTargetCompID
	}
	return noTag
}

// sendingTimeProblem checks m's SendingTime against our clock.
func (s *Session) sendingTimeProblem(m *fix2.Message, now time.Time) (dictionary.RejectReason, int, string) {
	v := m.Get(tagSendingTime)
	if v == nil {
		return dictionary.ReasonRequiredTagMissing, tagSendingTime, rejectText(dictionary.ReasonRequiredTagMissing, fieldDesc(nil, tagSendingTime))
	}
	t, err := fix2.ParseUTCTimestamp(v)
	if err != nil {
		return dictionary.ReasonIncorrectDataFormat, tagSendingTime, rejectText(dictionary.ReasonIncorrectDataFormat, fieldDesc(nil, tagSendingTime)+" is "+quoteValue(v))
	}
	if max := s.cfg.MaxLatency; max > 0 {
		if d := now.Sub(t); d >= max || d <= -max {
			return dictionary.ReasonSendingTimeAccuracyProblem, noTag, rejectText(dictionary.ReasonSendingTimeAccuracyProblem,
				fmt.Sprintf("SendingTime %s is %s from our clock %s (max %s)", v, d.Round(time.Millisecond).Abs(), fix2.AppendUTCTimestamp(nil, now, fix2.Milliseconds), max))
		}
	}
	return 0, 0, ""
}

// seqVerdict is what a received MsgSeqNum means.
type seqVerdict int

const (
	seqOK      seqVerdict = iota // the one expected
	seqTooHigh                   // messages are missing before it
	seqIgnore                    // a duplicate to ignore
	seqDone                      // too low: handled (rejected or logged out)
)

// checkSeqLocked compares in.Seq with the one expected, handling messages
// received too low: a retransmission (PossDupFlag) whose OrigSendingTime is
// consistent is ignored, otherwise FIX requires a Reject or a Logout.
func (s *Session) checkSeqLocked(c *conn, m *fix2.Message, in Inbound) seqVerdict {
	switch {
	case in.Seq == s.st.NextTarget:
		if in.PossDup && !s.origTimeOK(c, m, true) {
			return seqDone
		}
		return seqOK
	case in.Seq > s.st.NextTarget:
		return seqTooHigh
	}
	if !in.PossDup {
		s.logoutLocked(c, fmt.Sprintf("MsgSeqNum too low, expecting %d but received %d", s.st.NextTarget, in.Seq))
		return seqDone
	}
	if !s.origTimeOK(c, m, false) {
		return seqDone
	}
	return seqIgnore
}

// origTimeOK checks the OrigSendingTime of a PossDup message: missing, or
// later than its SendingTime, it is rejected, the latter with a Logout. A
// message in sequence then counts as received.
func (s *Session) origTimeOK(c *conn, m *fix2.Message, inSequence bool) bool {
	consumed := func() {
		if inSequence {
			seq, _ := fix2.ParseInt(m.Get(tagMsgSeqNum))
			s.markLocked(uint64(seq))
			s.advanceLocked(c, uint64(seq)+1)
		}
	}
	orig := m.Get(tagOrigSendingTime)
	if orig == nil {
		if s.cfg.Leniency.SequenceResetWithoutOrigSendingTime && bytes.Equal(m.MsgType(), []byte("4")) {
			return true
		}
		s.rejectLocked(m, dictionary.ReasonRequiredTagMissing, tagOrigSendingTime,
			rejectText(dictionary.ReasonRequiredTagMissing, fieldDesc(nil, tagOrigSendingTime)+" on a PossDup message"))
		consumed()
		return false
	}
	ot, err1 := fix2.ParseUTCTimestamp(orig)
	st, err2 := fix2.ParseUTCTimestamp(m.Get(tagSendingTime))
	if err1 != nil || err2 != nil {
		s.rejectLocked(m, dictionary.ReasonIncorrectDataFormat, tagOrigSendingTime,
			rejectText(dictionary.ReasonIncorrectDataFormat, fieldDesc(nil, tagOrigSendingTime)+" is "+quoteValue(orig)))
		consumed()
		return false
	}
	if st.Before(ot) {
		s.rejectLocked(m, dictionary.ReasonSendingTimeAccuracyProblem, noTag, rejectText(dictionary.ReasonSendingTimeAccuracyProblem,
			fmt.Sprintf("OrigSendingTime %s is after SendingTime %s", orig, m.Get(tagSendingTime))))
		s.logoutLocked(c, "")
		return false
	}
	return true
}

// tooHighLocked handles a message received ahead of a gap: it is kept, if
// there is room, and the missing messages are asked for.
func (s *Session) tooHighLocked(c *conn, in Inbound, frame []byte) {
	if s.stash == nil {
		s.stash = make(map[uint64][]byte)
	}
	if _, ok := s.stash[in.Seq]; !ok {
		if len(s.stash) < s.cfg.MaxStash {
			s.stash[in.Seq] = bytes.Clone(frame)
		} else {
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "too many messages received ahead of a gap: dropped, to be asked for again",
				slog.Uint64("seq", in.Seq))
		}
	}
	if in.Seq > s.recov.target {
		s.recov.target = in.Seq
	}
	if !s.recov.active {
		s.recov.active = true
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "MsgSeqNum too high, asking for the missing messages",
			slog.Uint64("expected", s.st.NextTarget), slog.Uint64("received", in.Seq))
		s.sendResendRequestLocked()
	}
}

// sendResendRequestLocked asks for the messages from the one expected up to
// the recovery target, in chunks if configured.
func (s *Session) sendResendRequestLocked() {
	begin := s.st.NextTarget
	end := s.prof.infinity
	s.recov.chunkEnd = 0
	if n := uint64(s.cfg.ResendChunkSize); n > 0 && begin+n-1 < s.recov.target {
		end = begin + n - 1
		s.recov.chunkEnd = end
	}
	s.sendAdminLocked(&header{msgType: "2"}, adminBody(nil).int(tagBeginSeqNo, begin).int(tagEndSeqNo, end))
}

// advanceLocked records that the messages up to next-1 were received, then
// handles the messages kept ahead of the gap that are now in sequence. Those
// may advance further: a nested call only moves the expected MsgSeqNum, and
// the outermost one goes on.
func (s *Session) advanceLocked(c *conn, next uint64) {
	if next > s.st.NextTarget+1 && !s.draining {
		// A jump (a GapFill): what it skipped over is behind.
		s.st.NextTarget = next
		s.pruneStashLocked()
	}
	if next > s.st.NextTarget {
		s.st.NextTarget = next
	}
	if s.draining {
		return
	}
	s.draining = true
	defer func() { s.draining = false }()
	pruned := s.st.NextTarget
	for {
		if s.recov.active {
			switch {
			case s.st.NextTarget > s.recov.target:
				s.recov = recovery{}
			// The next chunk once this one is received, or once a GapFill
			// ends at its last message, as QuickFIX asks for it.
			case s.recov.chunkEnd != 0 && s.stash[s.st.NextTarget] == nil && (s.st.NextTarget > s.recov.chunkEnd || s.gapFilling && s.st.NextTarget == s.recov.chunkEnd):
				s.sendResendRequestLocked()
			}
		}
		frame, ok := s.stash[s.st.NextTarget]
		if !ok || s.c != c || c.closed {
			break
		}
		delete(s.stash, s.st.NextTarget)
		s.replayStashedLocked(c, frame)
		if s.st.NextTarget > pruned+1 {
			// A jump (a GapFill): drop what it skipped over.
			s.pruneStashLocked()
		}
		pruned = s.st.NextTarget
	}
	if len(s.stash) > 0 && s.st.NextTarget > pruned {
		s.pruneStashLocked()
	}
}

// pruneStashLocked drops the messages kept that are now behind.
func (s *Session) pruneStashLocked() {
	for seq := range s.stash {
		if seq < s.st.NextTarget {
			delete(s.stash, seq)
		}
	}
}

// replayStashedLocked handles a message kept ahead of a gap, now in sequence.
func (s *Session) replayStashedLocked(c *conn, frame []byte) {
	m := &s.stashMsg
	if err := fix2.ParseInto(m, frame, s.dicts.parse); err != nil {
		return
	}
	if p := s.appProfileLocked(m); p != nil && p != s.dicts.parse {
		if fix2.ParseInto(m, frame, p) != nil {
			return
		}
	}
	seq, _ := fix2.ParseInt(m.Get(tagMsgSeqNum))
	in := Inbound{
		Seq:        uint64(seq),
		Generation: s.st.Generation,
		PossDup:    bytes.Equal(m.Get(tagPossDupFlag), yes),
		PossResend: bytes.Equal(m.Get(tagPossResend), yes),
		Received:   time.Now(),
	}
	switch string(m.MsgType()) {
	case "A":
		// A Logon received too high: processed when it arrived.
		s.markLocked(in.Seq)
		s.advanceLocked(c, in.Seq+1)
	case "5":
		s.onLogoutLocked(c, m, frame, in, in.Received)
	case "2":
		s.onResendRequestLocked(c, m, in, in.Received)
	case "4":
		s.onSequenceResetLocked(c, m, frame, in, in.Received)
	default:
		s.onRegularLocked(c, m, frame, in, in.Received)
	}
}

// appProfileLocked is appProfile with the lock held.
func (s *Session) appProfileLocked(m *fix2.Message) *fix2.Profile {
	if s.dicts.parseApps == nil {
		return nil
	}
	if v := m.Get(tagApplVerID); v != nil {
		return s.dicts.parseApps[string(v)]
	}
	if s.peerApplVerID != "" {
		return s.dicts.parseApps[s.peerApplVerID]
	}
	return nil
}

// onRegularLocked handles a message other than Logon, Logout, ResendRequest
// and SequenceReset.
func (s *Session) onRegularLocked(c *conn, m *fix2.Message, frame []byte, in Inbound, now time.Time) (Inbound, bool) {
	if !s.checkHeaderLocked(c, m, in.Seq, now) {
		return in, false
	}
	switch s.checkSeqLocked(c, m, in) {
	case seqTooHigh:
		s.tooHighLocked(c, in, frame)
		return in, false
	case seqIgnore, seqDone:
		return in, false
	}
	msgType := m.MsgType()
	admin := isAdmin(msgType)
	if verr := s.validateLocked(m, &in); verr != nil {
		s.rejectValidationLocked(m, verr)
		s.markLocked(in.Seq)
		s.advanceLocked(c, in.Seq+1)
		return in, false
	}
	switch string(msgType) {
	case "0":
		if id := m.Get(tagTestReqID); id != nil {
			if c.testReqID != "" && string(id) == c.testReqID {
				c.testReqID = ""
			}
			s.receipts.probeAnswered(c.epoch, id)
		}
	case "1":
		id := m.Get(tagTestReqID)
		if id == nil {
			s.rejectLocked(m, dictionary.ReasonRequiredTagMissing, tagTestReqID, rejectText(dictionary.ReasonRequiredTagMissing, fieldDesc(nil, tagTestReqID)))
			s.markLocked(in.Seq)
			s.advanceLocked(c, in.Seq+1)
			return in, false
		}
		s.sendAdminLocked(&header{msgType: "0"}, adminBody(nil).bytes(tagTestReqID, id))
	case "3", "j":
		if ref, err := fix2.ParseInt(m.Get(tagRefSeqNum)); err == nil && ref > 0 {
			s.receipts.rejected(s.st.Generation, uint64(ref), rejectInfo(m))
		}
	}
	if admin {
		s.markLocked(in.Seq)
	} else {
		s.deliv.push(item{kind: itemApp, gen: s.st.Generation, seq: in.Seq, frame: s.deliv.copyFrame(frame), in: in})
	}
	s.advanceLocked(c, in.Seq+1)
	return in, admin
}

// markLocked queues the processing of received message seq, which needs no
// delivery, so that the processed prefix moves past it in order.
func (s *Session) markLocked(seq uint64) {
	if !s.deliv.messagesAhead() {
		// No message ahead of it to deliver: processed now. A Logon is,
		// whether OnLogon ran yet or not: LastMsgSeqNumProcessed says so.
		if seq > s.committed {
			s.committed = seq
			s.deliv.persistLocked(false)
		}
		return
	}
	s.deliv.push(item{kind: itemMark, gen: s.st.Generation, seq: seq})
}

// validateLocked validates m against the session's dictionaries.
func (s *Session) validateLocked(m *fix2.Message, in *Inbound) *dictionary.ValidationError {
	d := s.dicts.session
	if s.prof.fixt && !isAdmin(m.MsgType()) {
		v := string(m.Get(tagApplVerID))
		if v == "" {
			v = s.peerApplVerID
		}
		in.ApplVerID = v
		if s.dicts.apps != nil {
			if d = s.dicts.apps[v]; d == nil {
				return &dictionary.ValidationError{Err: dictionary.ErrValidation, Tag: tagApplVerID, Reason: dictionary.ReasonInvalidApplVerID,
					Msg: fmt.Sprintf("no dictionary for ApplVerID %q", v)}
			}
		}
	}
	if d == nil {
		return nil
	}
	err := d.ValidateWith(m, dictionary.CheckAll&^s.cfg.SkipChecks)
	var ve *dictionary.ValidationError
	if errors.As(err, &ve) {
		if errors.Is(ve, dictionary.ErrUnknownMsgType) {
			// A MsgType the dictionary knows of but has no message for is
			// unsupported; one it does not know at all is invalid.
			if fd := d.Fields[tagMsgType]; fd != nil && fd.Enums[string(m.MsgType())] != "" && s.prof.businessRej {
				return &dictionary.ValidationError{Err: ve.Err, Tag: tagMsgType, Reason: -1, Msg: "Unsupported Message Type"}
			}
		}
		return ve
	}
	if err != nil {
		return &dictionary.ValidationError{Err: err, Reason: dictionary.ReasonOther, Msg: err.Error()}
	}
	return nil
}

// rejectValidationLocked rejects m for verr.
func (s *Session) rejectValidationLocked(m *fix2.Message, verr *dictionary.ValidationError) {
	if verr.Reason == -1 {
		s.businessRejectLocked(m, BusinessRejectUnsupportedMessageType, "", "Unsupported Message Type")
		return
	}
	detail, tag := verr.Msg, verr.Tag
	if verr.Reason == dictionary.ReasonInvalidMsgType {
		// RefMsgType says which: no RefTagID, as QuickFIX.
		detail, tag = "MsgType "+quoteValue(m.MsgType()), noTag
	}
	s.rejectLocked(m, verr.Reason, tag, rejectText(verr.Reason, detail))
}

// isRejectLocked reports whether m is a Reject (35=3) or a
// BusinessMessageReject (35=j), logging why it would have been rejected: a
// reject is never rejected, or the counterparty, rejecting our reject in
// turn, would loop with us.
func (s *Session) isRejectLocked(m *fix2.Message, text string) bool {
	switch string(m.MsgType()) {
	case "3", "j":
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "not rejecting a reject",
			slog.String("msg_type", string(m.MsgType())), slog.String("seq", string(m.Get(tagMsgSeqNum))),
			slog.String("text", text))
		return true
	}
	return false
}

// rejectLocked sends a Reject (35=3) of m about tag, noTag for none, unless m
// is a reject itself.
func (s *Session) rejectLocked(m *fix2.Message, reason dictionary.RejectReason, tag int, text string) {
	if s.c == nil || s.isRejectLocked(m, text) {
		return
	}
	s.stats.RejectsSent.Add(1)
	s.log.LogAttrs(context.Background(), slog.LevelWarn, "rejecting a message",
		slog.String("msg_type", string(m.MsgType())), slog.String("seq", string(m.Get(tagMsgSeqNum))),
		slog.Int("reason", int(reason)), slog.Int("tag", tag), slog.String("text", text))
	b := adminBody(nil)
	if ref := m.Get(tagMsgSeqNum); ref != nil {
		b = b.bytes(tagRefSeqNum, ref)
	}
	if s.prof.rejectReason {
		b = b.str(tagText, cleanText(text))
		if tag != noTag {
			b = fix2.AppendTagInt(b, tagRefTagID, int64(tag))
		}
		b = b.bytes(tagRefMsgType, m.MsgType())
		if s.prof.reasonAllowed(reason) {
			b = b.int(tagSessionRejectReason, uint64(reason))
		}
	} else {
		// No RefTagID before FIX 4.2: the tag follows the reason's name,
		// as QuickFIX writes it.
		if tag != noTag {
			if name := reason.String(); strings.HasPrefix(text, name) {
				text = name + " (" + strconv.Itoa(tag) + ")" + text[len(name):]
			} else {
				text += " (" + strconv.Itoa(tag) + ")"
			}
		}
		b = b.str(tagText, cleanText(text))
	}
	s.sendAdminLocked(&header{msgType: "3", route: reverseRoute(m, s.id.BeginString)}, b)
}

// businessRejectLocked sends a BusinessMessageReject (35=j) of m, or a
// Reject before FIX 4.2, unless m is a reject itself.
func (s *Session) businessRejectLocked(m *fix2.Message, reason int, refID, text string) {
	if s.c == nil || s.isRejectLocked(m, text) {
		return
	}
	if !s.prof.businessRej {
		s.rejectLocked(m, dictionary.ReasonOther, noTag, text)
		return
	}
	s.stats.RejectsSent.Add(1)
	b := adminBody(nil)
	if ref := m.Get(tagMsgSeqNum); ref != nil {
		b = b.bytes(tagRefSeqNum, ref)
	}
	b = b.str(tagText, cleanText(text)).bytes(tagRefMsgType, m.MsgType())
	if refID != "" {
		b = b.str(tagBusinessRejectRefID, refID)
	}
	b = b.int(tagBusinessRejectReason, uint64(reason))
	if _, err := s.queueLocked(&header{msgType: "j", route: reverseRoute(m, s.id.BeginString)}, b, true); err != nil && s.c != nil {
		s.disconnectLocked(s.c, &LogoutError{Text: "could not send a BusinessMessageReject", Err: err})
	}
}

// logoutLocked sends a Logout with text and waits for the counterparty's,
// at most LogoutTimeout, before disconnecting; not logged on, it disconnects.
func (s *Session) logoutLocked(c *conn, text string) {
	if c.closed || s.c != c {
		return
	}
	switch s.phase {
	case phaseLogoutSent:
		return
	case phaseAwaitingLogon, phaseLogonSent:
		// Not logged on: a Logout could even precede our Logon, while a
		// LogonSigner signs it. Disconnecting says enough.
		s.disconnectLocked(c, &LogoutError{Text: text})
		return
	}
	s.sendLogoutLocked(c, text)
	s.phase = phaseLogoutSent
	c.logoutText = text
	c.logoutDeadline = time.Now().Add(s.cfg.LogoutTimeout)
	s.armLocked(c)
}

// sendLogoutLocked sends a Logout with text.
func (s *Session) sendLogoutLocked(c *conn, text string) {
	if text != "" {
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "logging out", slog.String("text", text))
	}
	var b adminBody
	if text != "" {
		b = b.str(tagText, cleanText(text))
	}
	s.sendAdminLocked(&header{msgType: "5"}, b)
}

// onLogoutLocked handles a Logout: the answer to ours ends the connection;
// the counterparty's is answered, once the messages before it are recovered
// if some are missing.
func (s *Session) onLogoutLocked(c *conn, m *fix2.Message, frame []byte, in Inbound, now time.Time) {
	text := string(m.Get(tagText))
	if s.phase == phaseLogoutSent {
		// The answer to ours, if it comes from the counterparty.
		if _, wrong := s.compIDProblem(m); wrong != "" {
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "Logout answer with wrong identities ignored", slog.String("problem", wrong))
			return
		}
		if in.Seq == s.st.NextTarget {
			s.markLocked(in.Seq)
			s.advanceLocked(c, in.Seq+1)
		}
		// Why we logged out, rather than the answer's Text: an answer
		// usually has none, and crossing Logouts both give theirs.
		if c.logoutText != "" {
			text = c.logoutText
		}
		// After what is queued: crossing Logouts, ours may not be written
		// yet.
		s.closeAfterFlushLocked(c, &LogoutError{ByCounterparty: false, Text: text})
		return
	}
	if !s.checkHeaderLocked(c, m, in.Seq, now) {
		return
	}
	if in.Seq > s.st.NextTarget && s.cfg.Compatibility == CompatStrict2020 {
		// Recover the messages before it first: it is answered when its
		// turn comes, if the counterparty waits that long. QuickFIX
		// answers at once, and its acceptance tests expect it.
		s.tooHighLocked(c, in, frame)
		return
	}
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "counterparty logged out", slog.String("text", text))
	if in.Seq == s.st.NextTarget {
		s.markLocked(in.Seq)
		s.advanceLocked(c, in.Seq+1)
	}
	s.sendLogoutLocked(c, "")
	s.phase = phaseLogoutSent
	s.closeAfterFlushLocked(c, &LogoutError{ByCounterparty: true, Text: text})
}

// onSequenceResetLocked handles a SequenceReset: in GapFill mode it fills a
// gap in sequence; in Reset mode it moves the expected MsgSeqNum forward
// whatever its own.
func (s *Session) onSequenceResetLocked(c *conn, m *fix2.Message, frame []byte, in Inbound, now time.Time) {
	gapFill := bytes.Equal(m.Get(tagGapFillFlag), yes)
	if !s.checkHeaderLocked(c, m, in.Seq, now) {
		return
	}
	if gapFill {
		switch s.checkSeqLocked(c, m, in) {
		case seqTooHigh:
			s.tooHighLocked(c, in, frame)
			return
		case seqIgnore, seqDone:
			return
		}
	}
	newSeq, err := fix2.ParseInt(m.Get(tagNewSeqNo))
	if err != nil || newSeq <= 0 {
		if m.Get(tagNewSeqNo) == nil {
			s.rejectLocked(m, dictionary.ReasonRequiredTagMissing, tagNewSeqNo, rejectText(dictionary.ReasonRequiredTagMissing, fieldDesc(nil, tagNewSeqNo)))
		} else {
			s.rejectLocked(m, dictionary.ReasonIncorrectDataFormat, tagNewSeqNo, rejectText(dictionary.ReasonIncorrectDataFormat, fieldDesc(nil, tagNewSeqNo)+" is "+quoteValue(m.Get(tagNewSeqNo))))
		}
		if gapFill {
			s.markLocked(in.Seq)
			s.advanceLocked(c, in.Seq+1)
		}
		return
	}
	expected := s.st.NextTarget
	switch {
	case uint64(newSeq) > expected:
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "SequenceReset",
			slog.Bool("gap_fill", gapFill), slog.Uint64("from", expected), slog.Int("to", newSeq))
		if !gapFill && s.recov.active {
			s.deliv.push(item{kind: itemLoss, gen: s.st.Generation, seq: expected, end: uint64(newSeq) - 1, err: errors.New("counterparty reset its sequence numbers past them")})
		}
		s.deliv.push(item{kind: itemMark, gen: s.st.Generation, seq: uint64(newSeq) - 1})
		s.gapFilling = gapFill
		s.advanceLocked(c, uint64(newSeq))
		s.gapFilling = false
	case uint64(newSeq) == expected && !gapFill:
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "SequenceReset to the expected MsgSeqNum, nothing to do", slog.Int("new_seq_no", newSeq))
	default:
		reason := fmt.Sprintf("NewSeqNo %d is not greater than the expected MsgSeqNum %d", newSeq, expected)
		tag := tagNewSeqNo
		if !gapFill {
			// QuickFIX's acceptance tests expect no RefTagID here.
			tag = noTag
		}
		s.rejectLocked(m, dictionary.ReasonValueIsIncorrect, tag, rejectText(dictionary.ReasonValueIsIncorrect, reason))
		if gapFill && in.Seq == expected {
			s.markLocked(in.Seq)
			s.advanceLocked(c, in.Seq+1)
		}
	}
}

// onResendRequestLocked handles a ResendRequest: it is served whatever its
// own MsgSeqNum, then that is evaluated, which may make us ask for messages
// ourselves once the replay is done.
func (s *Session) onResendRequestLocked(c *conn, m *fix2.Message, in Inbound, now time.Time) {
	if !s.checkHeaderLocked(c, m, in.Seq, now) {
		return
	}
	begin, err1 := fix2.ParseInt(m.Get(tagBeginSeqNo))
	end, err2 := fix2.ParseInt(m.Get(tagEndSeqNo))
	switch {
	case m.Get(tagBeginSeqNo) == nil:
		s.rejectLocked(m, dictionary.ReasonRequiredTagMissing, tagBeginSeqNo, rejectText(dictionary.ReasonRequiredTagMissing, fieldDesc(nil, tagBeginSeqNo)))
	case m.Get(tagEndSeqNo) == nil:
		s.rejectLocked(m, dictionary.ReasonRequiredTagMissing, tagEndSeqNo, rejectText(dictionary.ReasonRequiredTagMissing, fieldDesc(nil, tagEndSeqNo)))
	case err1 != nil || begin <= 0:
		s.rejectLocked(m, dictionary.ReasonIncorrectDataFormat, tagBeginSeqNo, rejectText(dictionary.ReasonIncorrectDataFormat, fieldDesc(nil, tagBeginSeqNo)+" is "+quoteValue(m.Get(tagBeginSeqNo))))
	case err2 != nil || end < 0:
		s.rejectLocked(m, dictionary.ReasonIncorrectDataFormat, tagEndSeqNo, rejectText(dictionary.ReasonIncorrectDataFormat, fieldDesc(nil, tagEndSeqNo)+" is "+quoteValue(m.Get(tagEndSeqNo))))
	default:
		last := s.st.NextSender - 1
		e := uint64(end)
		if s.prof.isInfinity(e) || e > last {
			e = last
		}
		if s.cfg.Compatibility == CompatStrict2020 && in.Seq < s.st.NextTarget && !in.PossDup {
			// Handled below: too low without PossDup logs out.
		} else if uint64(begin) <= e {
			s.log.LogAttrs(context.Background(), slog.LevelInfo, "resending at the counterparty's request",
				slog.Int("begin", begin), slog.Uint64("end", e))
			s.startReplayLocked(c, uint64(begin), e, false)
		} else {
			s.log.LogAttrs(context.Background(), slog.LevelInfo, "ResendRequest for nothing sent yet",
				slog.Int("begin", begin), slog.Int("end", end), slog.Uint64("last_sent", last))
		}
	}
	switch s.checkSeqLocked(c, m, in) {
	case seqTooHigh:
		// Our ResendRequest is held behind the replay: FIX wants the replay
		// first (simultaneous resend requests).
		s.markAheadLocked(in.Seq)
		s.tooHighLocked(c, in, nil)
	case seqOK:
		s.markLocked(in.Seq)
		s.advanceLocked(c, in.Seq+1)
	}
}

// markAheadLocked keeps a marker for message seq, received ahead of a gap
// and already handled: when its turn comes it only moves the sequence on.
func (s *Session) markAheadLocked(seq uint64) {
	if s.stash == nil {
		s.stash = make(map[uint64][]byte)
	}
	if _, ok := s.stash[seq]; !ok && len(s.stash) < s.cfg.MaxStash {
		s.stash[seq] = handledMarker(s.id.BeginString, seq)
	}
}

// handledMarker returns a minimal Logon-like frame standing for a message
// already handled, which replayStashedLocked only counts.
func handledMarker(beginString string, seq uint64) []byte {
	body := fix2.AppendTagInt(fix2.AppendTagValue(nil, tagMsgType, "A"), tagMsgSeqNum, int64(seq))
	b := fix2.AppendTagValue(nil, 8, beginString)
	b = fix2.AppendTagInt(b, 9, int64(len(body)))
	b = append(b, body...)
	return fix2.AppendTrailer(b, 0)
}

// logonRefusal describes m, a Logout or Reject refusing our Logon.
func logonRefusal(m *fix2.Message) string {
	text := "Logon refused"
	if string(m.MsgType()) == "3" {
		text = "Logon rejected"
		if v := m.Get(tagSessionRejectReason); v != nil {
			if n, err := fix2.ParseInt(v); err == nil {
				text += fmt.Sprintf(" (SessionRejectReason %d: %s)", n, dictionary.RejectReason(n))
			} else {
				text += " (SessionRejectReason " + quoteValue(v) + ")"
			}
		}
	}
	if t := m.Get(tagText); len(t) > 0 {
		text += ": " + string(t)
	}
	return text
}
