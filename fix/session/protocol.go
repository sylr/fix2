package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/transport"
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
	// asked is the BeginSeqNo of the last ResendRequest.
	asked uint64
	// refill tells a GapFill replayed from those held was counted without
	// its NewSeqNo applied: what is missing after them is asked again.
	refill bool
	// pastReset is set while a Reset-mode SequenceReset moves past the
	// messages held: what it skips is not asked for again.
	pastReset bool
	// crawl counts the requests asked again in a row whose previous one
	// moved the sequence by one message only; crawlWarned tells it was
	// warned of. noOrig tells the last GapFill asked again for lacked
	// OrigSendingTime.
	crawl       int
	crawlWarned bool
	noOrig      bool
	// withheld tells a MsgSeqNum received too high may show a credential:
	// the ends of the ranges reported lost, worked out from them, are not
	// logged.
	withheld bool
}

// handleFrame handles a frame the reader read.
func (c *conn) handleFrame(frame []byte) {
	s := c.s
	s.waitDeliveryRoom(c)
	now := time.Now()
	m := &c.msg
	applVerID, err := s.parseInbound(m, frame)
	if err != nil {
		if tag, ok := badTagNumber(frame, m, err); ok {
			// Not logged on, the session has no sequence to reject it in:
			// it is garbled.
			s.mu.Lock()
			loggedOn := s.phase != phaseAwaitingLogon && s.phase != phaseLogonSent
			if loggedOn && s.c == c && !c.closed {
				s.stats.MessagesReceived.Add(1)
				s.stats.BytesReceived.Add(uint64(len(frame)))
				c.lastRead = now
				if c.peerLoggedOut {
					// As a message whole (see below).
					s.log.LogAttrs(context.Background(), slog.LevelInfo, "malformed message after the counterparty's Logout ignored")
				} else {
					s.invalidTagLocked(c, m, tag, now)
				}
				if !c.closed {
					s.armLocked(c)
				}
			}
			s.mu.Unlock()
			if loggedOn {
				return
			}
		}
		c.garbled(frame, err)
		return
	}
	s.stats.MessagesReceived.Add(1)
	s.stats.BytesReceived.Add(uint64(len(frame)))
	if obs := s.cfg.Observer; obs != nil {
		seq, _ := fix.ParseInt(m.Get(tagMsgSeqNum))
		obs.OnInbound(s, Frame{Raw: frame, Seq: uint64(max(seq, 0)), Generation: s.generation(), Epoch: c.epoch, Resend: bytes.Equal(m.Get(tagPossDupFlag), yes)})
	}

	// A Logon is authenticated before the session acts on it, unless the
	// session refuses it anyway: one resetting the sequence numbers is
	// checked first.
	if bytes.Equal(m.MsgType(), msgLogon) {
		s.mu.Lock()
		ph, current := s.phase, s.c == c && !c.closed
		s.mu.Unlock()
		if !current {
			return
		}
		reset := ph == phaseEstablished && s.prof.resetSeqNum && bytes.Equal(m.Get(tagResetSeqNumFlag), yes) &&
			string(m.BeginString()) == s.id.BeginString && s.resetLogonProblem(m, now) == nil
		if ph == phaseAwaitingLogon || ph == phaseLogonSent || reset {
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
	if c.peerLoggedOut {
		// Its last message: what follows is not handled, as what it held
		// after it is not (see advanceLocked).
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "message after the counterparty's Logout ignored", s.msgTypeAttr(m), s.seqAttr(m))
		s.mu.Unlock()
		return
	}
	in, fromAdmin := s.onMessageLocked(c, m, frame, applVerID, now)
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
func badTagNumber(frame []byte, m *fix.Message, err error) (int, bool) {
	var pe *fix.ParseError
	if !errors.As(err, &pe) || !errors.Is(err, fix.ErrInvalidTag) || m.MsgType() == nil || m.Get(tagMsgSeqNum) == nil {
		return 0, false
	}
	n := len(frame)
	if n < 7 || string(frame[n-7:n-4]) != "10=" {
		return 0, false
	}
	want, cerr := strconv.Atoi(string(frame[n-4 : n-1]))
	if cerr != nil || int(fix.CheckSum(frame[:n-7])) != want {
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

// invalidTagLocked handles m, parsed only up to tag, not a valid tag number,
// on a logged on session: as any message, in the same order, its
// BeginString, its header and its MsgSeqNum are checked, then it is rejected
// and counted as received if it is the one expected. Only the fields before
// tag are known: the header fields after it, if it is within the header,
// are not judged (see unseen), and a SequenceReset's NewSeqNo is not
// trusted.
func (s *Session) invalidTagLocked(c *conn, m *fix.Message, tag int, now time.Time) {
	s.partial, s.headerCut = m, s.cutInHeader(m)
	defer func() { s.partial, s.headerCut = nil, false }()
	seq, ok := s.msgSeqNumLocked(c, m)
	if !ok {
		return
	}
	s.replyTo = seq
	defer func() { s.replyTo = 0 }()
	// The header first: checking the sequence may move it on, and handle
	// the messages kept ahead of a gap.
	if !s.checkHeaderLocked(c, m, seq, now) {
		return
	}
	text := rejectText(dictionary.ReasonInvalidTagNumber, "")
	if s.resetMode(m) {
		s.rejectLocked(m, dictionary.ReasonInvalidTagNumber, tag, text)
		return
	}
	in := Inbound{Seq: seq, Generation: s.st.Generation, PossDup: bytes.Equal(m.Get(tagPossDupFlag), yes), Received: now}
	switch s.checkSeqLocked(c, m, in) {
	case seqTooHigh:
		// Not kept, it would not parse again: asked for again with those
		// missing before it, it is rejected in sequence.
		s.tooHighLocked(c, m, in, nil)
		return
	case seqIgnore, seqDone:
		return
	}
	s.lastProcessedLocked(m, in)
	s.rejectLocked(m, dictionary.ReasonInvalidTagNumber, tag, text)
	s.markLocked(seq)
	s.advanceLocked(c, seq+1)
	s.gapFillCountedLocked(c, m)
}

// cutInHeader tells whether m, parsed only up to a malformed tag, holds no
// field after its standard header: the tag is within the header. FIX puts
// the header first: once a field after it is seen, a header field m lacks
// is missing. The header is the session dictionary's or, without one, that
// of any version (see anyHeader).
func (s *Session) cutInHeader(m *fix.Message) bool {
	if d := s.dicts.session; d != nil {
		return !slices.ContainsFunc(d.Sections(m), func(sec dictionary.Section) bool { return sec != dictionary.SectionHeader })
	}
	return !slices.ContainsFunc(m.Fields(), func(f fix.Field) bool {
		return !slices.Contains(fixtHeader[:], f.Tag) && !slices.Contains(anyHeader[:], f.Tag)
	})
}

// anyHeader are the standard header fields of FIX 4.x not in FIXT 1.1's:
// OnBehalfOfSendingTime (370), of FIX 4.2 and 4.3.
var anyHeader = [...]int{370}

// unseen tells whether m lacks its header field tag only for being parsed up
// to a malformed tag within its header: the field may follow the tag. Such a
// field is not judged missing.
func (s *Session) unseen(m *fix.Message, tag int) bool {
	return m == s.partial && s.headerCut && m.Get(tag) == nil
}

// resetMode tells whether m is a SequenceReset in Reset mode, without
// GapFillFlag (123) Y. One parsed up to a malformed tag is only with a
// GapFillFlag seen, which may otherwise follow the tag: taken as a GapFill,
// it counts if in sequence, as a GapFill whose NewSeqNo cannot be used does,
// rather than leave the sequence waiting for it. A Reset-mode one without
// GapFillFlag, which its NewSeqNo would move past, is taken as a GapFill
// too: counted, it moves the sequence by one rather than not at all.
func (s *Session) resetMode(m *fix.Message) bool {
	flag := m.Get(tagGapFillFlag)
	if m == s.partial && flag == nil {
		return false
	}
	return string(m.MsgType()) == "4" && !bytes.Equal(flag, yes)
}

// garbled handles a frame that does not parse: FIX ignores it, without
// changing sequence numbers. Nothing it holds is logged, its MsgType and
// MsgSeqNum neither: it cannot be told which of its fields is sensitive,
// and any may hold a credential. Only its length, why, a text naming tags
// and limits only, and where.
func (c *conn) garbled(frame []byte, err error) {
	c.s.stats.Garbled.Add(1)
	attrs := []slog.Attr{slog.Int("bytes", len(frame)), slog.String("error", err.Error())}
	var pe *fix.ParseError
	if errors.As(err, &pe) {
		attrs = append(attrs, slog.Int("offset", pe.Offset))
	}
	c.s.log.LogAttrs(context.Background(), slog.LevelWarn, "garbled message ignored", attrs...)
}

// authenticate asks the handler whether to accept Logon m.
func (s *Session) authenticate(c *conn, m *fix.Message) error {
	req := &LogonRequest{ID: s.id, Logon: m, Response: s.initiator, RemoteAddr: c.tc.RemoteAddr()}
	if tc, ok := c.tc.(transport.TLSConn); ok {
		st := tc.ConnectionState()
		req.TLS = &st
	}
	if pc, ok := c.tc.(transport.PrincipalConn); ok {
		req.Principal = pc.Principal()
	}
	err := s.handler.Authenticate(s.callbackCtx(), req)
	if err == nil && req.byDefault && !s.initiator {
		// Warned of once logged on (see warnUnauthenticatedLocked): the
		// Logon may still be refused.
		s.mu.Lock()
		c.byDefault, c.transportAuth = true, req.TLS != nil || req.Principal != nil
		s.mu.Unlock()
	}
	return err
}

// warnUnauthenticatedLocked warns, once per session, that the Logon c
// logged on with was accepted by BaseHandler's Authenticate.
func (s *Session) warnUnauthenticatedLocked(c *conn) {
	if !c.byDefault || s.authWarned.Swap(true) {
		return
	}
	attrs := []slog.Attr{slog.String("remote_addr", addrString(c.tc.RemoteAddr()))}
	if !c.transportAuth {
		attrs = append(attrs, slog.Bool("transport_authenticated", false))
	}
	s.log.LogAttrs(context.Background(), slog.LevelWarn,
		"Logon accepted by BaseHandler's Authenticate, which accepts any Logon: implement Authenticate to authenticate counterparties", attrs...)
}

// addrString returns addr's String, "" for none.
func addrString(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
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

// profileFor returns the profile raw, a whole message, parses with: for FIXT,
// that of the application version the ApplVerID (1128) of its standard header
// names or, without one, the counterparty's DefaultApplVerID if inbound, ours
// if not. It is chosen before parsing, the body's DATA fields depending on it.
func (s *Session) profileFor(raw []byte, inbound bool) *fix.Profile {
	if s.dicts.parseApps == nil {
		return s.dicts.parse
	}
	if v, ok := s.dicts.applVerID(raw); ok {
		return s.dicts.appProfile(string(v))
	}
	if !inbound {
		return s.dicts.parse
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dicts.appProfile(s.peerApplVerID)
}

// parseInbound is parseApp without the lock held: received frame is parsed
// into m with the profile of its application version, which it returns for
// validation and delivery to use in turn.
func (s *Session) parseInbound(m *fix.Message, frame []byte) (string, error) {
	if !s.prof.fixt {
		return "", fix.ParseInto(m, frame, s.dicts.parse)
	}
	s.mu.Lock()
	peer := s.peerApplVerID
	s.mu.Unlock()
	return s.parseApp(m, frame, peer)
}

// parseApp parses received frame into m with the profile of its application
// version, and returns that version: for FIXT, the ApplVerID (1128) of its
// standard header or, without one, peer, the counterparty's
// DefaultApplVerID. It tries peer's profile first, which reads the standard
// header as any other does (see newDicts): a message of that version, the
// usual one, is parsed once, its ApplVerID read from its parsed header rather
// than looked for in frame. A version without a dictionary keeps that parse:
// validation rejects it.
func (s *Session) parseApp(m *fix.Message, frame []byte, peer string) (string, error) {
	if !s.prof.fixt {
		return "", fix.ParseInto(m, frame, s.dicts.parse)
	}
	first := s.dicts.appProfile(peer)
	err := fix.ParseInto(m, frame, first)
	var v []byte
	var ok bool
	if err == nil {
		v, ok = s.dicts.headerApplVerID(m)
	} else {
		v, ok = s.dicts.applVerID(frame)
	}
	if !ok {
		return peer, err
	}
	version := s.dicts.version(v)
	if p := s.dicts.parseApps[version]; p != nil && p != first {
		err = fix.ParseInto(m, frame, p)
	}
	return version, err
}

// onMessageLocked handles received message m, frame its bytes, of FIXT
// application version applVerID, on c. It returns what to tell the handler's
// FromAdmin, if anything.
func (s *Session) onMessageLocked(c *conn, m *fix.Message, frame []byte, applVerID string, now time.Time) (Inbound, bool) {
	msgType := m.MsgType()
	seq, ok := s.msgSeqNumLocked(c, m)
	if !ok {
		return Inbound{}, false
	}
	in := Inbound{
		Seq:        seq,
		Generation: s.st.Generation,
		PossDup:    bytes.Equal(m.Get(tagPossDupFlag), yes),
		PossResend: bytes.Equal(m.Get(tagPossResend), yes),
		Received:   now,
	}
	if !isAdmin(msgType) {
		in.ApplVerID = applVerID
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
			s.disconnectLocked(c, &LogoutError{Text: withhold(m, tagMsgType, fmt.Sprintf("first message is MsgType %s, not a Logon", msgType), "first message is not a Logon")})
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

// msgSeqNumLocked checks m's BeginString and MsgSeqNum, received on c, and
// returns the latter: 0 for a Reset-mode SequenceReset without a valid one,
// which does not count. On a problem it logs out, or disconnects if not
// logged on, and returns false.
func (s *Session) msgSeqNumLocked(c *conn, m *fix.Message) (uint64, bool) {
	if string(m.BeginString()) != s.id.BeginString {
		received := s.withholdLocked(m, noTag, quoteValue(m.BeginString()), "withheld")
		if s.phase == phaseAwaitingLogon || s.phase == phaseLogonSent {
			s.disconnectLocked(c, &LogoutError{Text: fmt.Sprintf("Logon with BeginString %s, expected %s", received, s.id.BeginString)})
			return 0, false
		}
		s.logoutLocked(c, fmt.Sprintf("Incorrect BeginString: expected %s, received %s", s.id.BeginString, received))
		return 0, false
	}
	seq, err := fix.ParseInt(m.Get(tagMsgSeqNum))
	resetMode := s.resetMode(m)
	if resetMode && (err != nil || seq < 0) {
		seq, err = 0, nil // ignored in Reset mode
	}
	if err != nil || seq <= 0 && !resetMode {
		text := "MsgSeqNum (34) missing"
		if m.Get(tagMsgSeqNum) != nil {
			text = s.withholdLocked(m, tagMsgSeqNum, "MsgSeqNum (34) is not a positive integer: "+quoteValue(m.Get(tagMsgSeqNum)),
				"MsgSeqNum (34) is not a positive integer")
		}
		if s.phase == phaseAwaitingLogon || s.phase == phaseLogonSent {
			s.disconnectLocked(c, &LogoutError{Text: text})
		} else {
			s.logoutLocked(c, text)
		}
		return 0, false
	}
	return uint64(seq), true
}

// checkHeaderLocked checks the CompIDs and SendingTime of an established
// session's message, received with sequence number seq. On a problem it
// rejects the message, counts it as received if it is the one expected, and
// returns false: a wrong CompID or an inaccurate SendingTime then logs out,
// as FIX requires (FIXT 1.1 test cases 2k and 2o). A Reset-mode
// SequenceReset, whose MsgSeqNum does not count, is not counted.
func (s *Session) checkHeaderLocked(c *conn, m *fix.Message, seq uint64, now time.Time) bool {
	reason, tag, text := s.compIDOrTimeProblem(m, now)
	if text == "" {
		return true
	}
	s.rejectLocked(m, reason, tag, text)
	counts := seq == s.st.NextTarget && !s.resetMode(m)
	switch reason {
	case dictionary.ReasonCompIDProblem, dictionary.ReasonSendingTimeAccuracyProblem:
		if counts {
			// Counted, but what is kept ahead of it is not processed now:
			// the session logs out, and asks for it again once logged on,
			// unless the gap is filled while logging out.
			s.markLocked(seq)
			s.st.NextTarget = seq + 1
		}
		s.logoutLocked(c, "")
	default:
		if counts {
			s.markLocked(seq)
			s.advanceLocked(c, seq+1)
			s.gapFillCountedLocked(c, m)
		}
	}
	return false
}

// compIDOrTimeProblem returns the first problem of m's CompIDs or SendingTime.
func (s *Session) compIDOrTimeProblem(m *fix.Message, now time.Time) (dictionary.RejectReason, int, string) {
	if reason, text := s.compIDProblem(m); text != "" {
		return reason, compIDTag(reason, m), text
	}
	return s.sendingTimeProblem(m, now)
}

// compIDProblem checks the counterparty's identity on m.
func (s *Session) compIDProblem(m *fix.Message) (dictionary.RejectReason, string) {
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
		case s.unseen(m, f.tag):
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
func compIDTag(reason dictionary.RejectReason, m *fix.Message) int {
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
func (s *Session) sendingTimeProblem(m *fix.Message, now time.Time) (dictionary.RejectReason, int, string) {
	v := m.Get(tagSendingTime)
	if s.unseen(m, tagSendingTime) {
		return 0, 0, ""
	}
	if v == nil {
		return dictionary.ReasonRequiredTagMissing, tagSendingTime, rejectText(dictionary.ReasonRequiredTagMissing, fieldDesc(nil, tagSendingTime))
	}
	t, err := fix.ParseUTCTimestamp(v)
	if err != nil {
		return dictionary.ReasonIncorrectDataFormat, tagSendingTime, rejectText(dictionary.ReasonIncorrectDataFormat, fieldDesc(nil, tagSendingTime)+" is "+quoteValue(v))
	}
	if max := s.cfg.MaxLatency; max > 0 {
		if d := now.Sub(t); d >= max || d <= -max {
			return dictionary.ReasonSendingTimeAccuracyProblem, noTag, rejectText(dictionary.ReasonSendingTimeAccuracyProblem,
				fmt.Sprintf("SendingTime %s is %s from our clock %s (max %s)", v, d.Round(time.Millisecond).Abs(), fix.AppendUTCTimestamp(nil, now, fix.Milliseconds), max))
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
func (s *Session) checkSeqLocked(c *conn, m *fix.Message, in Inbound) seqVerdict {
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
		if s.unseen(m, tagPossDupFlag) {
			// A retransmission maybe, whose PossDupFlag follows the
			// malformed tag: ignored as one.
			return seqIgnore
		}
		s.logoutLocked(c, s.seqTooLowText(m, s.st.NextTarget, in.Seq))
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
func (s *Session) origTimeOK(c *conn, m *fix.Message, inSequence bool) bool {
	consumed := func() {
		if inSequence {
			seq, _ := fix.ParseInt(m.Get(tagMsgSeqNum))
			s.markLocked(uint64(seq))
			s.advanceLocked(c, uint64(seq)+1)
			s.gapFillCountedLocked(c, m)
		}
	}
	if s.unseen(m, tagOrigSendingTime) || s.unseen(m, tagSendingTime) {
		// It may follow the malformed tag: the two cannot be compared.
		return true
	}
	orig := m.Get(tagOrigSendingTime)
	if orig == nil {
		if s.cfg.TolerateSequenceResetWithoutOrigSendingTime && bytes.Equal(m.MsgType(), []byte("4")) {
			return true
		}
		s.rejectLocked(m, dictionary.ReasonRequiredTagMissing, tagOrigSendingTime,
			rejectText(dictionary.ReasonRequiredTagMissing, fieldDesc(nil, tagOrigSendingTime)+" on a PossDup message"))
		consumed()
		return false
	}
	ot, err1 := fix.ParseUTCTimestamp(orig)
	st, err2 := fix.ParseUTCTimestamp(m.Get(tagSendingTime))
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
func (s *Session) tooHighLocked(c *conn, m *fix.Message, in Inbound, frame []byte) {
	// A nil frame stands for a message handled already, whose marker
	// markAheadLocked could not keep, or for a partial one, never kept.
	if _, ok := s.stash[in.Seq]; !ok && m != s.partial && (frame == nil || !s.stashLocked(in.Seq, frame, in.Received, true)) {
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "too many messages received ahead of a gap: dropped, to be asked for again",
			s.numAttr(m, "seq", tagMsgSeqNum, int64(in.Seq)))
	}
	if in.Seq > s.recov.target {
		s.recov.target = in.Seq
	}
	if s.withholdLocked(m, tagMsgSeqNum, strconv.FormatUint(in.Seq, 10), "") == "" {
		s.recov.withheld = true
	}
	if !s.recov.active {
		s.recov.active = true
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "MsgSeqNum too high, asking for the missing messages",
			slog.Uint64("expected", s.st.NextTarget), s.numAttr(m, "received", tagMsgSeqNum, int64(in.Seq)))
		s.sendResendRequestLocked(0)
	}
}

// sendResendRequestLocked asks for the messages from the one expected up to
// the recovery target, in chunks if configured, or up to upTo if not 0:
// the next request is then made as a chunk's.
func (s *Session) sendResendRequestLocked(upTo uint64) {
	begin := s.st.NextTarget
	end := s.prof.infinity
	s.recov.chunkEnd = 0
	if n := uint64(s.cfg.ResendChunkSize); n > 0 && begin+n-1 < s.recov.target {
		end = begin + n - 1
		s.recov.chunkEnd = end
	}
	if upTo != 0 && (s.recov.chunkEnd == 0 || upTo < end) {
		end = upTo
		s.recov.chunkEnd = end
	}
	s.recov.asked = begin
	s.sendAdminLocked(&header{msgType: "2"}, adminBody(nil).int(tagBeginSeqNo, begin).int(tagEndSeqNo, end))
}

// gapFillCountedLocked follows the counting, on c, of m if it is a GapFill
// whose NewSeqNo was not applied: during a recovery, the messages it may
// have stood for are asked for again, the recovery waiting for them
// otherwise. One replayed from those held is followed once they are handled
// (see advanceLocked). A GapFill whose NewSeqNo, read, is not past it stood
// for no other: it needs none.
func (s *Session) gapFillCountedLocked(c *conn, m *fix.Message) {
	if string(m.MsgType()) != "4" || s.resetMode(m) || !s.recov.active || s.recov.pastReset ||
		s.c != c || c.closed || s.phase != phaseEstablished {
		return
	}
	s.recov.noOrig = m != s.partial && bytes.Equal(m.Get(tagPossDupFlag), yes) && m.Get(tagOrigSendingTime) == nil
	if s.draining {
		s.recov.refill = true
		return
	}
	s.refillLocked()
}

// crawlWarning is how many requests asked again in a row, each after one
// moving the sequence by one message, are warned of.
const crawlWarning = 3

// refillLocked asks again for the messages missing from the one expected:
// only those up to the first held, or to the recovery target, which asked
// each time to the last would be resent over and over. Nothing is asked if
// a request from the one expected was just made. A counterparty whose every
// GapFill is unusable is warned of, once: the gap is then recovered one
// message per request.
func (s *Session) refillLocked() {
	if s.recov.asked == s.st.NextTarget {
		return
	}
	s.recov.crawl++
	if s.st.NextTarget != s.recov.asked+1 {
		s.recov.crawl = 0
	}
	if s.recov.crawl == crawlWarning && !s.recov.crawlWarned {
		s.recov.crawlWarned = true
		text := "the counterparty's GapFills are unusable: the gap is recovered one message per ResendRequest"
		if s.recov.noOrig {
			text += ", its PossDup ones lacking OrigSendingTime (122): see Config.TolerateSequenceResetWithoutOrigSendingTime"
		}
		s.log.LogAttrs(context.Background(), slog.LevelWarn, text, slog.Uint64("expected", s.st.NextTarget))
	}
	end := s.recov.target
	for seq := range s.stash {
		if seq > s.st.NextTarget && seq <= end {
			end = seq - 1
		}
	}
	s.sendResendRequestLocked(end)
}

// advanceLocked records that the messages up to next-1 were received, then
// handles the messages kept ahead of the gap that are now in sequence. Those
// may advance further: a nested call only moves the expected MsgSeqNum, and
// the outermost one goes on. One that starts a Logout stops it, as does
// closing: those kept after it are neither processed nor counted by it, nor
// asked for. They are asked for again once logged on,
// unless the gap is filled while logging out.
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
	// The phase the messages kept are handled in: one that changes it
	// started a Logout, after which nothing more is handled nor asked for,
	// as once closing. While our Logout awaits its answer, they are handled
	// as those received then are.
	phase := s.phase
	pruned := s.st.NextTarget
	for s.c == c && !c.closed && !c.closing && s.phase == phase {
		if s.recov.active {
			switch {
			case s.st.NextTarget > s.recov.target:
				s.recov = recovery{}
			// The next chunk once this one is received, or once a GapFill
			// ends at its last message, as QuickFIX asks for it.
			case s.recov.chunkEnd != 0 && s.stash[s.st.NextTarget].frame == nil && (s.st.NextTarget > s.recov.chunkEnd || s.gapFilling && s.st.NextTarget == s.recov.chunkEnd):
				s.sendResendRequestLocked(0)
			}
		}
		kept, ok := s.stash[s.st.NextTarget]
		if !ok {
			break
		}
		s.unstashLocked(s.st.NextTarget)
		s.replayStashedLocked(c, kept)
		if s.st.NextTarget > pruned+1 {
			// A jump (a GapFill): drop what it skipped over.
			s.pruneStashLocked()
		}
		pruned = s.st.NextTarget
	}
	if len(s.stash) > 0 && s.st.NextTarget > pruned {
		s.pruneStashLocked()
	}
	if s.recov.refill {
		s.recov.refill = false
		if s.recov.active && s.c == c && !c.closed && !c.closing && s.phase == phaseEstablished {
			s.refillLocked()
		}
	}
}

// pruneStashLocked drops the messages kept that are now behind.
func (s *Session) pruneStashLocked() {
	for seq := range s.stash {
		if seq < s.st.NextTarget {
			s.unstashLocked(seq)
		}
	}
}

// keptMsg is a message kept ahead of a gap: its frame, and when it was
// received. It is handled as of then when its turn comes: its SendingTime
// was checked against that time, and recovering the gap before it may take
// longer than MaxLatency.
type keptMsg struct {
	frame    []byte
	received time.Time
}

// stashLocked keeps frame, message seq received ahead of a gap at received,
// a copy if clone is set, if MaxStash and MaxStashBytes leave room for it,
// and reports whether it did.
func (s *Session) stashLocked(seq uint64, frame []byte, received time.Time, clone bool) bool {
	if len(s.stash) >= s.cfg.MaxStash || len(s.stash) > 0 && s.stashSize+len(frame) > s.cfg.MaxStashBytes {
		return false
	}
	if clone {
		frame = bytes.Clone(frame)
	}
	s.keepLocked(seq, keptMsg{frame: frame, received: received})
	return true
}

// keepLocked keeps k for message seq in the stash.
func (s *Session) keepLocked(seq uint64, k keptMsg) {
	if s.stash == nil {
		s.stash = make(map[uint64]keptMsg)
	}
	s.stash[seq] = k
	s.stashSize += len(k.frame)
}

// unstashLocked drops message seq kept ahead of a gap.
func (s *Session) unstashLocked(seq uint64) {
	s.stashSize -= len(s.stash[seq].frame)
	delete(s.stash, seq)
}

// clearStashLocked drops every message kept ahead of a gap.
func (s *Session) clearStashLocked() {
	clear(s.stash)
	s.stashSize = 0
}

// replayStashedLocked handles kept, a message kept ahead of a gap, now in
// sequence, as of when it was received.
func (s *Session) replayStashedLocked(c *conn, kept keptMsg) {
	m, frame := &s.stashMsg, kept.frame
	applVerID, err := s.parseApp(m, frame, s.peerApplVerID)
	if err != nil {
		return
	}
	seq, _ := fix.ParseInt(m.Get(tagMsgSeqNum))
	in := Inbound{
		Seq:        uint64(seq),
		Generation: s.st.Generation,
		PossDup:    bytes.Equal(m.Get(tagPossDupFlag), yes),
		PossResend: bytes.Equal(m.Get(tagPossResend), yes),
		Received:   kept.received,
	}
	if !isAdmin(m.MsgType()) {
		in.ApplVerID = applVerID
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

// onRegularLocked handles a message other than Logon, Logout, ResendRequest
// and SequenceReset.
func (s *Session) onRegularLocked(c *conn, m *fix.Message, frame []byte, in Inbound, now time.Time) (Inbound, bool) {
	if !s.checkHeaderLocked(c, m, in.Seq, now) {
		return in, false
	}
	switch s.checkSeqLocked(c, m, in) {
	case seqTooHigh:
		s.tooHighLocked(c, m, in, frame)
		return in, false
	case seqIgnore, seqDone:
		return in, false
	}
	s.lastProcessedLocked(m, in)
	msgType := m.MsgType()
	admin := isAdmin(msgType)
	if verr := s.validateLocked(m, in.ApplVerID); verr != nil {
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
		if ref, err := fix.ParseInt(m.Get(tagRefSeqNum)); err == nil && ref > 0 {
			s.receipts.rejected(s.st.Generation, uint64(ref), s.st.NextSender, rejectInfo(m))
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

// validateLocked validates m against the session's dictionaries: for a FIXT
// application message, that of applVerID, the version it was parsed as.
func (s *Session) validateLocked(m *fix.Message, applVerID string) *dictionary.ValidationError {
	d := s.dicts.session
	if s.prof.fixt && !isAdmin(m.MsgType()) {
		if s.dicts.apps != nil {
			if d = s.dicts.apps[applVerID]; d == nil {
				return &dictionary.ValidationError{Err: dictionary.ErrValidation, Tag: tagApplVerID, Reason: dictionary.ReasonInvalidApplVerID,
					Msg: fmt.Sprintf("no dictionary for ApplVerID %q", applVerID)}
			}
		}
	}
	if d == nil {
		return nil
	}
	err := d.ValidateWith(m, dictionary.CheckAll&^s.cfg.TolerateValidationFailures)
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

// rejectValidationLocked rejects m for verr. A Reject or BusinessMessageReject
// the dictionaries do not define, as unsupported or as an invalid MsgType, is
// not: a counterparty whose dictionaries do not define ours either would
// reject it in turn, and the two would loop.
func (s *Session) rejectValidationLocked(m *fix.Message, verr *dictionary.ValidationError) {
	if isReject(m.MsgType()) && (verr.Reason == -1 || verr.Reason == dictionary.ReasonInvalidMsgType) {
		s.logUnanswered(m, verr.Tag, verr.Msg)
		return
	}
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

// logUnanswered logs why m, a Reject or BusinessMessageReject, would have been
// rejected, about its field tag (noTag for none), had answering it not
// risked a loop with the counterparty.
func (s *Session) logUnanswered(m *fix.Message, tag int, text string) {
	text = withhold(m, tag, text, "value withheld")
	s.log.LogAttrs(context.Background(), slog.LevelWarn, "not rejecting a reject",
		s.msgTypeAttr(m), s.seqAttr(m),
		slog.String("text", text))
}

// rejectLocked sends a Reject (35=3) of m about tag, noTag for none.
func (s *Session) rejectLocked(m *fix.Message, reason dictionary.RejectReason, tag int, text string) {
	if s.c == nil {
		return
	}
	if s.c.closing {
		// Nothing is sent after our Logout (see queueLocked), nor counted.
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "not rejecting a message after our Logout",
			s.msgTypeAttr(m), s.seqAttr(m), slog.Int("reason", int(reason)))
		return
	}
	fallback := rejectText(reason, "value withheld")
	if m == s.partial {
		// Nothing of it can be told safe to repeat: the reason only,
		// RefTagID telling which field.
		fallback = rejectText(reason, "")
	}
	text = s.withholdLocked(m, tag, text, fallback)
	s.stats.RejectsSent.Add(1)
	attrs := []slog.Attr{s.msgTypeAttr(m), s.seqAttr(m), slog.Int("reason", int(reason))}
	if tag != noTag {
		attrs = append(attrs, slog.Int("tag", tag))
	}
	s.log.LogAttrs(context.Background(), slog.LevelWarn, "rejecting a message", append(attrs, slog.String("text", text))...)
	b := adminBody(nil)
	if ref := m.Get(tagMsgSeqNum); ref != nil {
		// A partial message's only if a sequence number: anything else
		// may be part of a credential.
		if n, err := fix.ParseInt(ref); m != s.partial || err == nil && n > 0 {
			b = b.bytes(tagRefSeqNum, ref)
		}
	}
	if s.prof.rejectReason {
		b = b.str(tagText, cleanText(text))
		if tag != noTag {
			b = fix.AppendTagInt(b, tagRefTagID, int64(tag))
		}
		// As received, a partial message's too: QuickFIX's acceptance
		// tests expect it (14a). An Observer shows it as it is sent.
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
// Reject before FIX 4.2.
func (s *Session) businessRejectLocked(m *fix.Message, reason int, refID, text string) {
	if s.c == nil {
		return
	}
	if !s.prof.businessRej {
		s.rejectLocked(m, dictionary.ReasonOther, noTag, text)
		return
	}
	if s.c.closing {
		// As rejectLocked.
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "not rejecting a message after our Logout",
			s.msgTypeAttr(m), s.seqAttr(m), slog.Int("business_reject_reason", reason))
		return
	}
	text = withhold(m, noTag, text, "value withheld")
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
	if _, err := s.queueLocked(&header{msgType: "j", route: reverseRoute(m, s.id.BeginString)}, b, true); err != nil && !errors.Is(err, errClosing) && s.c != nil {
		s.disconnectLocked(s.c, &LogoutError{Text: "could not send a BusinessMessageReject", Err: err})
	}
}

// logoutLocked sends a Logout with text and waits for the counterparty's,
// at most LogoutTimeout, before disconnecting; not logged on, it disconnects.
func (s *Session) logoutLocked(c *conn, text string) {
	if c.closed || c.closing || s.c != c {
		// Closing, our Logout is queued already.
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
func (s *Session) onLogoutLocked(c *conn, m *fix.Message, frame []byte, in Inbound, now time.Time) {
	text := withhold(m, tagText, string(m.Get(tagText)), "text withheld")
	if s.phase == phaseLogoutSent {
		// The answer to ours, if it comes from the counterparty.
		if _, wrong := s.compIDProblem(m); wrong != "" {
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "Logout answer with wrong identities ignored",
				slog.String("problem", withhold(m, noTag, wrong, "value withheld")))
			return
		}
		next := in.Seq == s.st.NextTarget
		if next {
			s.markLocked(in.Seq)
		}
		// Why we logged out, rather than the answer's Text: an answer
		// usually has none, and crossing Logouts both give theirs.
		if c.logoutText != "" {
			text = c.logoutText
		}
		// After what is queued: crossing Logouts, ours may not be written
		// yet.
		c.peerLoggedOut = true
		s.closeAfterFlushLocked(c, &LogoutError{ByCounterparty: false, Text: text})
		if next {
			// Closing: what is kept after the answer stays kept.
			s.advanceLocked(c, in.Seq+1)
		}
		return
	}
	if !s.checkHeaderLocked(c, m, in.Seq, now) {
		return
	}
	if in.Seq > s.st.NextTarget && !s.cfg.TolerateLogoutBeforeGapRecovery {
		// Recover the messages before it first: it is answered when its
		// turn comes, if the counterparty waits that long. QuickFIX
		// answers at once, and its acceptance tests expect it
		// (TolerateLogoutBeforeGapRecovery).
		s.tooHighLocked(c, m, in, frame)
		return
	}
	s.lastProcessedLocked(m, in)
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "counterparty logged out", slog.String("text", text))
	// Marked before it is answered, for the answer's
	// LastMsgSeqNumProcessed; the answer queued before closing, which
	// refuses anything more.
	next := in.Seq == s.st.NextTarget
	if next {
		s.markLocked(in.Seq)
	}
	s.sendLogoutLocked(c, "")
	s.phase = phaseLogoutSent
	c.peerLoggedOut = true
	s.closeAfterFlushLocked(c, &LogoutError{ByCounterparty: true, Text: text})
	if next {
		// Closing: what is kept after the Logout stays kept, as when the
		// Logout itself was kept.
		s.advanceLocked(c, in.Seq+1)
	}
}

// onSequenceResetLocked handles a SequenceReset: in GapFill mode it fills a
// gap in sequence; in Reset mode it moves the expected MsgSeqNum forward
// whatever its own.
func (s *Session) onSequenceResetLocked(c *conn, m *fix.Message, frame []byte, in Inbound, now time.Time) {
	gapFill := bytes.Equal(m.Get(tagGapFillFlag), yes)
	if !s.checkHeaderLocked(c, m, in.Seq, now) {
		return
	}
	if gapFill {
		switch s.checkSeqLocked(c, m, in) {
		case seqTooHigh:
			s.tooHighLocked(c, m, in, frame)
			return
		case seqIgnore, seqDone:
			return
		}
	}
	newSeq, err := fix.ParseInt(m.Get(tagNewSeqNo))
	if err != nil || newSeq <= 0 {
		if m.Get(tagNewSeqNo) == nil {
			s.rejectLocked(m, dictionary.ReasonRequiredTagMissing, tagNewSeqNo, rejectText(dictionary.ReasonRequiredTagMissing, fieldDesc(nil, tagNewSeqNo)))
		} else {
			s.rejectLocked(m, dictionary.ReasonIncorrectDataFormat, tagNewSeqNo, rejectText(dictionary.ReasonIncorrectDataFormat, fieldDesc(nil, tagNewSeqNo)+" is "+quoteValue(m.Get(tagNewSeqNo))))
		}
		if gapFill {
			s.markLocked(in.Seq)
			s.advanceLocked(c, in.Seq+1)
			s.gapFillCountedLocked(c, m)
		}
		return
	}
	expected := s.st.NextTarget
	switch {
	case uint64(newSeq) > expected:
		if !gapFill && c.closing {
			// As resetPastLocked would: what it passes is asked for again
			// on the next connection.
			s.log.LogAttrs(context.Background(), slog.LevelInfo, "SequenceReset ignored after our Logout",
				slog.Uint64("from", expected), s.numAttr(m, "to", tagNewSeqNo, int64(newSeq)))
			return
		}
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "SequenceReset",
			slog.Bool("gap_fill", gapFill), slog.Uint64("from", expected), s.numAttr(m, "to", tagNewSeqNo, int64(newSeq)))
		if !gapFill {
			s.resetPastLocked(c, uint64(newSeq))
			return
		}
		s.deliv.push(item{kind: itemMark, gen: s.st.Generation, seq: uint64(newSeq) - 1})
		s.gapFilling = true
		s.advanceLocked(c, uint64(newSeq))
		s.gapFilling = false
	case uint64(newSeq) == expected && !gapFill:
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "SequenceReset to the expected MsgSeqNum, nothing to do", s.numAttr(m, "new_seq_no", tagNewSeqNo, int64(newSeq)))
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

// errResetPast is the cause OnRecoveryLoss gets for messages a Reset-mode
// SequenceReset gave up.
var errResetPast = errors.New("counterparty reset its sequence numbers past them")

// resetPastLocked handles a Reset-mode SequenceReset to next, above the
// MsgSeqNum expected. The messages held ahead of a gap below next were
// received, and the counterparty will not send them again: they are processed
// in sequence, each as if the gap before it were filled. A recovery under way
// reports lost the messages missing below the highest one received, one range
// per gap: those above it, the SequenceReset among them, the counterparty may
// never have sent.
func (s *Session) resetPastLocked(c *conn, next uint64) {
	recovering, last, withheld := s.recov.active, s.recov.target, s.recov.withheld
	s.recov.pastReset = true
	defer func() { s.recov.pastReset = false }()
	// The chunks of a chunked ResendRequest below next are given up: none is
	// asked for while the messages held are processed.
	chunkEnd := s.recov.chunkEnd
	s.recov.chunkEnd = 0
	// Looked at once: processing them only removes some.
	stashed := slices.Sorted(maps.Keys(s.stash))
	s.heldScanned += uint64(len(stashed))
	// As in advanceLocked, one held that starts a Logout, or closing, stops
	// it, the last drain to next too: one that reached next would otherwise
	// start another, in the Logout's phase.
	phase := s.phase
	for {
		if s.c != c || c.closed || c.closing || s.phase != phase {
			// What is left is asked for again once logged on, unless the
			// gap is filled while logging out.
			return
		}
		if s.st.NextTarget >= next {
			break
		}
		for len(stashed) > 0 && (stashed[0] < s.st.NextTarget || s.stash[stashed[0]].frame == nil) {
			stashed = stashed[1:]
		}
		held := next
		if len(stashed) > 0 && stashed[0] < next {
			held = stashed[0]
		}
		if from := s.st.NextTarget; from < held {
			if recovering && from <= last {
				s.deliv.push(item{kind: itemLoss, gen: s.st.Generation, seq: from, end: min(held-1, last), err: errResetPast, withheld: withheld})
			}
			// Queued after the loss, rather than marked: the messages lost
			// count as processed once OnRecoveryLoss has told of them.
			s.deliv.push(item{kind: itemMark, gen: s.st.Generation, seq: held - 1})
		}
		if held == next {
			break
		}
		// Processes it, and those held after it in sequence.
		s.advanceLocked(c, held)
	}
	if s.recov.active {
		s.recov.chunkEnd = chunkEnd
	}
	s.advanceLocked(c, next)
}

// onResendRequestLocked handles a ResendRequest: it is served whatever its
// own MsgSeqNum, then that is evaluated, which may make us ask for messages
// ourselves once the replay is done.
func (s *Session) onResendRequestLocked(c *conn, m *fix.Message, in Inbound, now time.Time) {
	if !s.checkHeaderLocked(c, m, in.Seq, now) {
		return
	}
	begin, err1 := fix.ParseInt(m.Get(tagBeginSeqNo))
	end, err2 := fix.ParseInt(m.Get(tagEndSeqNo))
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
		// The counterparty has our messages up to the one before begin,
		// and none after: what it asks for is not to be evicted.
		s.lastProcessedLocked(m, in)
		s.ackedLocked(m, uint64(begin)-1, tagBeginSeqNo, in.Seq)
		last := s.st.NextSender - 1
		e := uint64(end)
		if s.prof.isInfinity(e) || e > last {
			e = last
		}
		if !s.cfg.ServeResendRequestSeqTooLow && in.Seq < s.st.NextTarget && !in.PossDup {
			// Handled below: too low without PossDup logs out.
		} else if uint64(begin) <= e {
			s.log.LogAttrs(context.Background(), slog.LevelInfo, "resending at the counterparty's request",
				s.numAttr(m, "begin", tagBeginSeqNo, int64(begin)), s.numAttr(m, "end", tagEndSeqNo, int64(e)))
			s.startReplayLocked(c, uint64(begin), e, false)
		} else {
			s.log.LogAttrs(context.Background(), slog.LevelInfo, "ResendRequest for nothing sent yet",
				s.numAttr(m, "begin", tagBeginSeqNo, int64(begin)), s.numAttr(m, "end", tagEndSeqNo, int64(end)), slog.Uint64("last_sent", last))
		}
	}
	switch s.checkSeqLocked(c, m, in) {
	case seqTooHigh:
		// Our ResendRequest is held behind the replay: FIX wants the replay
		// first (simultaneous resend requests).
		s.markAheadLocked(in.Seq)
		s.tooHighLocked(c, m, in, nil)
	case seqOK:
		s.markLocked(in.Seq)
		s.advanceLocked(c, in.Seq+1)
	}
}

// markAheadLocked keeps a marker for message seq, received ahead of a gap
// and already handled: when its turn comes it only moves the sequence on.
// A marker is a few dozen bytes: MaxStash alone bounds them.
func (s *Session) markAheadLocked(seq uint64) {
	if _, ok := s.stash[seq]; !ok && len(s.stash) < s.cfg.MaxStash {
		// Only counted when its turn comes: it needs no time.
		s.keepLocked(seq, keptMsg{frame: handledMarker(s.id.BeginString, seq)})
	}
}

// handledMarker returns a minimal Logon-like frame standing for a message
// already handled, which replayStashedLocked only counts.
func handledMarker(beginString string, seq uint64) []byte {
	body := fix.AppendTagInt(fix.AppendTagValue(nil, tagMsgType, "A"), tagMsgSeqNum, int64(seq))
	b := fix.AppendTagValue(nil, 8, beginString)
	b = fix.AppendTagInt(b, 9, int64(len(body)))
	b = append(b, body...)
	return fix.AppendTrailer(b, 0)
}

// logonRefusal describes m, a Logout or Reject refusing our Logon.
func logonRefusal(m *fix.Message) string {
	text := "Logon refused"
	if string(m.MsgType()) == "3" {
		text = "Logon rejected"
		if v := m.Get(tagSessionRejectReason); v != nil {
			if n, err := fix.ParseInt(v); err == nil {
				text += fmt.Sprintf(" (SessionRejectReason %d: %s)", n, dictionary.RejectReason(n))
			} else {
				text += " (SessionRejectReason " + quoteValue(v) + ")"
			}
		}
	}
	if t := m.Get(tagText); len(t) > 0 {
		text += ": " + string(t)
	}
	return withhold(m, noTag, text, "Logon refused, text withheld")
}

// seqTooLowText is the Text of the Logout answering m, received with
// MsgSeqNum seq while next was expected.
func (s *Session) seqTooLowText(m *fix.Message, next, seq uint64) string {
	return s.withholdLocked(m, tagMsgSeqNum, fmt.Sprintf("MsgSeqNum too low, expecting %d but received %d", next, seq), "MsgSeqNum too low")
}

// msgTypeAttr and seqAttr log m's MsgType and MsgSeqNum, withheld if they
// may show a credential.
func (s *Session) msgTypeAttr(m *fix.Message) slog.Attr {
	return slog.String("msg_type", s.withholdLocked(m, tagMsgType, string(m.MsgType()), "withheld"))
}

func (s *Session) seqAttr(m *fix.Message) slog.Attr {
	return slog.String("seq", s.withholdLocked(m, tagMsgSeqNum, string(m.Get(tagMsgSeqNum)), "withheld"))
}
