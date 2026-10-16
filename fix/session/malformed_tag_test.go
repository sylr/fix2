package session_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary/xmldict"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// A message whose tag after the header is malformed is rejected in
// sequence, and counted: the session goes on.
func TestMalformedTagRejected(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	p.writeRaw(rawFrame("35=D|49=ACC|56=INI|34=2|52="+sendingTime()+"|11=O1|abc=1|", ""))
	p.expect("3", "45", "2", "373", "0", "58", "Invalid tag number")
	p.write(from(session.FIX44, 3), "1", "112", "AFTER")
	p.expect("0", "112", "AFTER")
	if got := s.NextTargetSeqNum(); got != 4 {
		t.Errorf("NextTargetSeqNum = %d, want 4", got)
	}
	st := s.Stats()
	if got := st.RejectsSent.Load(); got != 1 {
		t.Errorf("%d rejects sent, want 1", got)
	}
	// The Logon, the message rejected and the TestRequest.
	if got := st.MessagesReceived.Load(); got != 3 {
		t.Errorf("%d messages received, want 3", got)
	}
}

// A malformed tag does not spare a message the header checks, whether it
// is after the header or within it: a wrong CompID or an inaccurate
// SendingTime is rejected and logs out, as in a well-formed message.
func TestMalformedTagHeaderChecked(t *testing.T) {
	now := sendingTime()
	stale := string(fix.AppendUTCTimestamp(nil, time.Now().Add(-time.Hour), fix.Milliseconds))
	for _, tc := range []struct {
		name, fields string
		reason       string
	}{
		{"wrong SenderCompID", "49=OTHER|56=INI|34=2|52=" + now + "|11=O1|abc=1|", "9"},
		{"wrong SenderCompID, tag within the header", "49=OTHER|56=INI|34=2|abc=1|52=" + now + "|11=O1|", "9"},
		{"wrong TargetCompID", "49=ACC|56=OTHER|34=2|52=" + now + "|11=O1|abc=1|", "9"},
		{"wrong TargetCompID, tag within the header", "49=ACC|56=OTHER|34=2|abc=1|52=" + now + "|11=O1|", "9"},
		{"inaccurate SendingTime", "49=ACC|56=INI|34=2|52=" + stale + "|11=O1|abc=1|", "10"},
		{"inaccurate SendingTime, tag within the header", "49=ACC|56=INI|34=2|52=" + stale + "|abc=1|11=O1|", "10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, p := loggedOn(t, nil)
			p.writeRaw(rawFrame("35=D|"+tc.fields, ""))
			p.expect("3", "45", "2", "373", tc.reason)
			p.expect("5")
			if got := s.NextTargetSeqNum(); got != 3 {
				t.Errorf("NextTargetSeqNum = %d, want 3", got)
			}
		})
	}
}

// A header field the message lacks is missing if a field after the header
// was parsed, FIX putting the header first; if the malformed tag is within
// the header, it may follow the tag, and is not judged.
func TestMalformedTagAbsentHeaderField(t *testing.T) {
	now := sendingTime()
	for _, tc := range []struct {
		name, fields string
		reject       []string // the Reject's fields
	}{
		{"SendingTime missing", "49=ACC|56=INI|34=2|11=O1|abc=1|52=" + now + "|", []string{"371", "52", "373", "1"}},
		{"SendingTime after the tag", "49=ACC|56=INI|34=2|abc=1|52=" + now + "|11=O1|", []string{"373", "0"}},
		{"OrigSendingTime missing", "49=ACC|56=INI|34=2|52=" + now + "|43=Y|11=O1|abc=1|", []string{"371", "122", "373", "1"}},
		{"OrigSendingTime after the tag", "49=ACC|56=INI|34=2|52=" + now + "|43=Y|abc=1|122=" + now + "|11=O1|", []string{"373", "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, p := loggedOn(t, nil)
			p.writeRaw(rawFrame("35=D|"+tc.fields, ""))
			p.expect("3", append([]string{"45", "2"}, tc.reject...)...)
			p.write(from(session.FIX44, 3), "1", "112", "AFTER")
			p.expect("0", "112", "AFTER")
			if got := s.NextTargetSeqNum(); got != 4 {
				t.Errorf("NextTargetSeqNum = %d, want 4", got)
			}
		})
	}
}

// With SubIDs and LocationIDs configured, those of the message are checked
// as its CompIDs are: missing once a field after the header was parsed, not
// judged if they may follow a malformed tag within it.
func TestMalformedTagSubAndLocationIDs(t *testing.T) {
	now := sendingTime()
	for _, tc := range []struct {
		name, fields string
		reason       string
		logout       bool
	}{
		{"before the tag", "49=ACC|56=INI|50=SUB|142=LOC|34=2|52=" + now + "|11=O1|abc=1|", "0", false},
		{"after the tag, within the header", "49=ACC|56=INI|34=2|52=" + now + "|abc=1|50=SUB|142=LOC|11=O1|", "0", false},
		{"missing", "49=ACC|56=INI|34=2|52=" + now + "|11=O1|abc=1|50=SUB|142=LOC|", "9", true},
		{"wrong SubID, tag within the header", "49=ACC|56=INI|50=OTHER|34=2|52=" + now + "|abc=1|142=LOC|", "9", true},
		{"wrong LocationID, tag within the header", "49=ACC|56=INI|50=SUB|142=OTHER|34=2|52=" + now + "|abc=1|", "9", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := mem.Listen("acceptor")
			s, rec := startInitiator(t, l, func(c *session.Config) { c.ID.TargetSubID, c.ID.TargetLocationID = "SUB", "LOC" })
			p := acceptPeer(t, l)
			p.expect("A")
			p.send("A", 0, "50", "SUB", "142", "LOC", "98", "0", "108", "30")
			rec.wait(t, func() bool { return rec.logons == 1 })
			p.writeRaw(rawFrame("35=D|"+tc.fields, ""))
			p.expect("3", "45", "2", "373", tc.reason)
			if tc.logout {
				p.expect("5")
				return
			}
			p.writeRaw(rawFrame("35=1|49=ACC|56=INI|50=SUB|142=LOC|34=3|52="+sendingTime()+"|112=AFTER|", ""))
			p.expect("0", "112", "AFTER")
			if got := s.NextTargetSeqNum(); got != 4 {
				t.Errorf("NextTargetSeqNum = %d, want 4", got)
			}
		})
	}
}

// A malformed tag does not spare a message the BeginString check either.
func TestMalformedTagBeginString(t *testing.T) {
	_, _, p := loggedOn(t, nil)
	p.writeRaw(frameOf("FIX.4.2", "35=D|49=ACC|56=INI|34=2|52="+sendingTime()+"|11=O1|abc=1|"))
	// What the session cannot see of the message may hold a credential:
	// the BeginString received is withheld.
	p.expect("5", "58", "Incorrect BeginString: expected FIX.4.4, received withheld")
}

// frameOf returns a frame of BeginString begin and body ("|" for SOH).
func frameOf(begin, body string) []byte {
	body = strings.ReplaceAll(body, "|", "\x01")
	msg := "8=" + begin + "\x019=" + strconv.Itoa(len(body)) + "\x01" + body
	return []byte(msg + fmt.Sprintf("10=%03d\x01", fix.CheckSum([]byte(msg))))
}

// origSendingTime is an OrigSendingTime a second ago: before the
// SendingTime of a message written after, whatever the millisecond.
func origSendingTime() string {
	return string(fix.AppendUTCTimestamp(nil, time.Now().Add(-time.Second), fix.Milliseconds))
}

// One received too low without PossDupFlag logs out, as any other; one
// whose PossDupFlag may follow a malformed tag within its header is
// ignored, as a retransmission would be.
func TestMalformedTagTooLow(t *testing.T) {
	now := sendingTime()
	t.Run("PossDupFlag missing", func(t *testing.T) {
		s, _, p := loggedOn(t, nil)
		p.writeRaw(rawFrame("35=D|49=ACC|56=INI|34=1|52="+now+"|11=O1|abc=1|", ""))
		p.expect("5", "58", "MsgSeqNum too low")
		if got := s.NextTargetSeqNum(); got != 2 {
			t.Errorf("NextTargetSeqNum = %d, want 2", got)
		}
	})
	t.Run("PossDupFlag after the tag", func(t *testing.T) {
		s, _, p := loggedOn(t, nil)
		p.writeRaw(rawFrame("35=D|49=ACC|56=INI|34=1|52="+now+"|abc=1|43=Y|122="+now+"|11=O1|", ""))
		// Nothing answers it: the TestRequest is answered first.
		p.write(from(session.FIX44, 2), "1", "112", "AFTER")
		p.expect("0", "112", "AFTER")
		if got := s.NextTargetSeqNum(); got != 3 {
			t.Errorf("NextTargetSeqNum = %d, want 3", got)
		}
	})
}

// One received too high is not kept, a message that does not parse being
// dropped when its turn comes: it is asked for again with those missing
// before it, then rejected in sequence, and the session goes on. A
// malformed GapFill counts, its NewSeqNo unusable: what it stood for, up to
// the first message held, is asked for again.
func TestMalformedTagTooHigh(t *testing.T) {
	// malformed returns message seq with a malformed tag, resent if dup.
	malformed := func(seq int, dup bool) []byte {
		h := "49=ACC|56=INI|34=" + strconv.Itoa(seq) + "|52=" + sendingTime()
		if dup {
			h += "|43=Y|122=" + origSendingTime()
		}
		return rawFrame("35=D|"+h+"|11=O1|abc=1|", "")
	}
	// gapFill writes a GapFill, resent, from seq to next.
	gapFill := func(p *peer, seq, next int) {
		p.write(from(session.FIX44, seq), "4", "43", "Y", "122", origSendingTime(), "123", "Y", "36", strconv.Itoa(next))
	}
	// badGapFill writes a GapFill, resent, from seq, fields after its header
	// and a malformed tag.
	badGapFill := func(p *peer, seq int, fields string) {
		p.writeRaw(rawFrame("35=4|49=ACC|56=INI|34="+strconv.Itoa(seq)+"|52="+sendingTime()+"|43=Y|122="+origSendingTime()+"|"+fields, ""))
	}
	for _, tc := range []struct {
		name   string
		change func(*session.Config)
		run    func(p *peer) int // returns the next MsgSeqNum the peer sends
	}{
		{"recovery started by it", nil, func(p *peer) int {
			p.writeRaw(malformed(3, false))
			p.expect("2", "7", "2", "16", "0")
			gapFill(p, 2, 3)
			p.writeRaw(malformed(3, true))
			p.expect("3", "45", "3", "373", "0")
			return 4
		}},
		{"the recovery's only missing one", nil, func(p *peer) int {
			p.write(from(session.FIX44, 3), "0")
			p.expect("2", "7", "2", "16", "0")
			// Rejected, it lets the Heartbeat kept ahead of it in.
			p.writeRaw(malformed(2, true))
			p.expect("3", "45", "2", "373", "0")
			return 4
		}},
		{"recovery under way", nil, func(p *peer) int {
			p.write(from(session.FIX44, 3), "0")
			p.expect("2", "7", "2", "16", "0")
			// No other ResendRequest: the one sent asks for everything.
			p.writeRaw(malformed(4, false))
			gapFill(p, 2, 3)
			p.writeRaw(malformed(4, true))
			p.expect("3", "45", "4", "373", "0")
			return 5
		}},
		{"chunked ResendRequests", func(c *session.Config) { c.ResendChunkSize = 2 }, func(p *peer) int {
			p.writeRaw(malformed(5, false))
			p.expect("2", "7", "2", "16", "3")
			gapFill(p, 2, 4)
			p.expect("2", "7", "4", "16", "0")
			gapFill(p, 4, 5)
			p.writeRaw(malformed(5, true))
			p.expect("3", "45", "5", "373", "0")
			return 6
		}},
		{"GapFill past it", nil, func(p *peer) int {
			p.writeRaw(malformed(3, false))
			p.expect("2", "7", "2", "16", "0")
			gapFill(p, 2, 4)
			return 4
		}},
		{"GapFillFlag after the tag", nil, func(p *peer) int {
			p.write(from(session.FIX44, 3), "0")
			p.expect("2", "7", "2", "16", "0")
			badGapFill(p, 2, "abc=1|123=Y|36=3|")
			p.expect("3", "45", "2", "373", "0")
			return 4
		}},
		{"malformed GapFill of several", nil, func(p *peer) int {
			p.write(from(session.FIX44, 4), "0")
			p.expect("2", "7", "2", "16", "0")
			badGapFill(p, 2, "123=Y|abc=1|36=4|")
			p.expect("3", "45", "2", "373", "0")
			p.expect("2", "7", "3", "16", "3")
			gapFill(p, 3, 4)
			return 5
		}},
		{"malformed GapFills, over and over", nil, func(p *peer) int {
			p.write(from(session.FIX44, 7), "0")
			p.expect("2", "7", "2", "16", "0")
			for seq := 2; seq < 6; seq++ {
				badGapFill(p, seq, "123=Y|abc=1|36=7|")
				p.expect("3", "45", strconv.Itoa(seq), "373", "0")
				p.expect("2", "7", strconv.Itoa(seq+1), "16", "6")
			}
			gapFill(p, 6, 7)
			return 8
		}},
		{"malformed GapFill ending a chunk", func(c *session.Config) { c.ResendChunkSize = 2 }, func(p *peer) int {
			p.write(from(session.FIX44, 5), "0")
			p.expect("2", "7", "2", "16", "3")
			p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
			badGapFill(p, 3, "123=Y|abc=1|36=4|")
			p.expect("3", "45", "3", "373", "0")
			// The next chunk only: no other request from 4.
			p.expect("2", "7", "4", "16", "0")
			gapFill(p, 4, 5)
			return 6
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, p := loggedOn(t, tc.change)
			next := tc.run(p)
			p.write(from(session.FIX44, next), "1", "112", "AFTER")
			p.expect("0", "112", "AFTER")
			if got := s.NextTargetSeqNum(); got != uint64(next)+1 {
				t.Errorf("NextTargetSeqNum = %d, want %d", got, next+1)
			}
			if got := s.State(); got != session.StateLoggedOn {
				t.Errorf("State = %v, want %v", got, session.StateLoggedOn)
			}
		})
	}
}

// A Reset-mode SequenceReset with a malformed tag is rejected, its MsgSeqNum
// not counted, as that of any Reset-mode one, nor its NewSeqNo trusted.
func TestMalformedTagSequenceReset(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	p.writeRaw(rawFrame("35=4|49=ACC|56=INI|34=2|52="+sendingTime()+"|123=N|36=10|abc=1|", ""))
	p.expect("3", "45", "2", "373", "0")
	// Counted, or moved to 10, it would make the TestRequest too low.
	p.write(from(session.FIX44, 2), "1", "112", "AFTER")
	p.expect("0", "112", "AFTER")
	if got := s.NextTargetSeqNum(); got != 3 {
		t.Errorf("NextTargetSeqNum = %d, want 3", got)
	}
}

// The Reject of a Reset-mode SequenceReset whose MsgSeqNum, ignored, is no
// sequence number has no RefSeqNum: what it holds instead, of a message parsed
// up to a malformed tag, may be part of a credential.
func TestMalformedTagRefSeqNum(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	p.writeRaw(rawFrame("35=4|49=ACC|56=INI|34=S3CRET|52="+sendingTime()+"|123=N|36=10|abc=1|", ""))
	p.expect("3", "45", "", "373", "0")
	p.write(from(session.FIX44, 2), "1", "112", "AFTER")
	p.expect("0", "112", "AFTER")
	if got := s.NextTargetSeqNum(); got != 3 {
		t.Errorf("NextTargetSeqNum = %d, want 3", got)
	}
}

// A SequenceReset whose GapFillFlag may follow a malformed tag is taken as a
// GapFill, a Reset-mode one included: counted, its NewSeqNo not applied.
func TestMalformedTagSequenceResetWithoutGapFillFlag(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	p.writeRaw(rawFrame("35=4|49=ACC|56=INI|34=2|52="+sendingTime()+"|36=10|abc=1|", ""))
	p.expect("3", "45", "2", "373", "0")
	p.write(from(session.FIX44, 3), "1", "112", "AFTER")
	p.expect("0", "112", "AFTER")
	if got := s.NextTargetSeqNum(); got != 4 {
		t.Errorf("NextTargetSeqNum = %d, want 4", got)
	}
}

// A GapFill counted though its NewSeqNo was not applied, rejected, during a
// recovery: what it stood for, up to the message held, is asked for again,
// malformed tag or not, or the recovery would wait for it for good.
func TestUnusableGapFillAsksAgain(t *testing.T) {
	now, orig := sendingTime(), origSendingTime()
	for _, tc := range []struct {
		name, fields string
		reject       []string // the Reject's fields
	}{
		{"OrigSendingTime missing", "52=" + now + "|43=Y|123=Y|36=5|", []string{"371", "122", "373", "1"}},
		{"SendingTime missing", "43=Y|122=" + orig + "|123=Y|36=5|", []string{"371", "52", "373", "1"}},
		{"NewSeqNo invalid", "52=" + now + "|43=Y|122=" + orig + "|123=Y|36=x|", []string{"371", "36", "373", "6"}},
		{"malformed tag, OrigSendingTime missing", "52=" + now + "|43=Y|123=Y|abc=1|36=5|", []string{"371", "122", "373", "1"}},
		{"malformed tag, SendingTime missing", "43=Y|122=" + orig + "|123=Y|abc=1|36=5|", []string{"371", "52", "373", "1"}},
		{"malformed tag", "52=" + now + "|43=Y|122=" + orig + "|123=Y|abc=1|36=5|", []string{"373", "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, p := loggedOn(t, nil)
			p.write(from(session.FIX44, 5), "0")
			p.expect("2", "7", "2", "16", "0")
			p.writeRaw(rawFrame("35=4|49=ACC|56=INI|34=2|"+tc.fields, ""))
			p.expect("3", append([]string{"45", "2"}, tc.reject...)...)
			p.expect("2", "7", "3", "16", "4")
			p.write(from(session.FIX44, 3), "4", "43", "Y", "122", origSendingTime(), "123", "Y", "36", "5")
			p.write(from(session.FIX44, 6), "1", "112", "AFTER")
			p.expect("0", "112", "AFTER")
			if got := s.NextTargetSeqNum(); got != 7 {
				t.Errorf("NextTargetSeqNum = %d, want 7", got)
			}
		})
	}
}

// A GapFill held ahead of a gap and counted, its NewSeqNo not applied, once
// its turn comes asks again for what is missing after the messages held
// behind it: not before they are handled.
func TestUnusableHeldGapFillAsksAgain(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	p.write(from(session.FIX44, 3), "4", "43", "Y", "123", "Y", "36", "4")
	p.expect("2", "7", "2", "16", "0")
	p.write(from(session.FIX44, 4), "0")
	p.write(from(session.FIX44, 6), "0")
	p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
	p.expect("3", "45", "3", "371", "122", "373", "1")
	p.expect("2", "7", "5", "16", "5")
	p.write(from(session.FIX44, 5), "0", "43", "Y", "122", origSendingTime())
	p.write(from(session.FIX44, 7), "1", "112", "AFTER")
	p.expect("0", "112", "AFTER")
	if got := s.NextTargetSeqNum(); got != 8 {
		t.Errorf("NextTargetSeqNum = %d, want 8", got)
	}
}

// A Reset-mode SequenceReset past a held GapFill counted without its NewSeqNo
// applied asks for nothing it skips.
func TestUnusableGapFillResetPast(t *testing.T) {
	_, _, p := loggedOn(t, nil)
	p.write(from(session.FIX44, 5), "0")
	p.expect("2", "7", "2", "16", "0")
	p.write(from(session.FIX44, 3), "4", "43", "Y", "123", "Y", "36", "5")
	p.write(from(session.FIX44, 6), "0")
	p.write(from(session.FIX44, 7), "4", "36", "10")
	p.expect("3", "45", "3", "371", "122")
	p.write(from(session.FIX44, 10), "1", "112", "AFTER")
	p.expect("0", "112", "AFTER")
}

// Our Logout sent, a held GapFill counted without its NewSeqNo applied asks
// for nothing.
func TestUnusableGapFillAfterLogout(t *testing.T) {
	_, _, p := loggedOn(t, func(c *session.Config) { c.MaxLatency = 500 * time.Millisecond })
	p.write(from(session.FIX44, 3), "4", "43", "Y", "123", "Y", "36", "5")
	p.expect("2", "7", "2", "16", "0")
	p.write(from(session.FIX44, 6), "0")
	// Stale when received, 7 is rejected: it logs out.
	stale := from(session.FIX44, 7)
	stale.sent = time.Now().Add(-time.Second)
	p.write(stale, "0")
	p.expect("3", "45", "7", "373", "10")
	p.expect("5")
	// Received while logging out, 2 is handled, and the GapFill held after
	// it: counted, its NewSeqNo not applied.
	p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
	p.expect("3", "45", "3", "371", "122")
	for {
		select {
		case ev := <-p.frames:
			if ev.err != nil {
				return
			}
			if m, err := fix.Parse(ev.frame); err == nil && string(m.MsgType()) == "2" {
				t.Fatalf("got %s after our Logout", visible(ev.frame))
			}
		case <-time.After(5 * time.Second):
			t.Fatal("connection still open")
		}
	}
}

// Chunked, a request asked again shorter than a chunk is followed by the
// chunks after it.
func TestUnusableGapFillChunked(t *testing.T) {
	s, _, p := loggedOn(t, func(c *session.Config) { c.ResendChunkSize = 10 })
	gapFill := func(seq, next int) {
		p.write(from(session.FIX44, seq), "4", "43", "Y", "122", origSendingTime(), "123", "Y", "36", strconv.Itoa(next))
	}
	p.write(from(session.FIX44, 30), "0")
	p.expect("2", "7", "2", "16", "11")
	p.write(from(session.FIX44, 5), "0")
	p.write(from(session.FIX44, 2), "4", "43", "Y", "123", "Y", "36", "5")
	p.expect("3", "45", "2", "371", "122")
	p.expect("2", "7", "3", "16", "4")
	gapFill(3, 5)
	p.expect("2", "7", "6", "16", "15")
	gapFill(6, 16)
	p.expect("2", "7", "16", "16", "25")
	gapFill(16, 30)
	p.write(from(session.FIX44, 31), "1", "112", "AFTER")
	p.expect("0", "112", "AFTER")
	if got := s.NextTargetSeqNum(); got != 32 {
		t.Errorf("NextTargetSeqNum = %d, want 32", got)
	}
}

// A counterparty whose GapFills are all unusable is warned of, once a
// recovery: the gap is then recovered one message per request. Without
// OrigSendingTime, the warning names the leniency that accepts them.
func TestUnusableGapFillsWarned(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		leniency     bool
	}{
		{"OrigSendingTime missing", "43=Y|123=Y|36=10|", true},
		{"malformed tag", "43=Y|122=" + origSendingTime() + "|123=Y|abc=1|36=10|", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := &logBuffer{}
			_, _, p := loggedOn(t, func(c *session.Config) { c.Logger = slog.New(slog.NewTextHandler(buf, nil)) })
			p.write(from(session.FIX44, 10), "0")
			p.expect("2", "7", "2", "16", "0")
			for seq := 2; seq < 7; seq++ {
				p.writeRaw(rawFrame("35=4|49=ACC|56=INI|34="+strconv.Itoa(seq)+"|52="+sendingTime()+"|"+tc.fields, ""))
				p.expect("3", "45", strconv.Itoa(seq))
				p.expect("2", "7", strconv.Itoa(seq+1), "16", "9")
			}
			p.write(from(session.FIX44, 7), "4", "43", "Y", "122", origSendingTime(), "123", "Y", "36", "10")
			p.write(from(session.FIX44, 11), "1", "112", "AFTER")
			p.expect("0", "112", "AFTER")
			out := buf.String()
			if n := strings.Count(out, "GapFills are unusable"); n != 1 {
				t.Errorf("warned %d times, want 1:\n%s", n, out)
			}
			if got := strings.Contains(out, "Leniency.SequenceResetWithoutOrigSendingTime"); got != tc.leniency {
				t.Errorf("leniency named: %v, want %v:\n%s", got, tc.leniency, out)
			}
		})
	}
}

// Whether the malformed tag is within the header is told by the session's
// dictionary, if it has one, for FIX 4.x as for FIXT.
func TestMalformedTagHeaderOfDictionary(t *testing.T) {
	now := sendingTime()
	for _, v := range []struct {
		name  string
		begin string
		start func(t *testing.T) (*session.Session, *peer)
	}{
		{"FIX 4.4", session.FIX44, func(t *testing.T) (*session.Session, *peer) {
			// The whole of FIX 4.4, whose header has NoHops.
			d, err := xmldict.LoadFile("../tests/benchmarks/spec/FIX44.xml")
			if err != nil {
				t.Fatal(err)
			}
			s, _, p := loggedOn(t, func(c *session.Config) { c.Dictionaries.Session = d })
			return s, p
		}},
		{"FIXT 1.1", session.FIXT11, func(t *testing.T) (*session.Session, *peer) {
			s, _, p := fixtSession(t, "9", nil)
			return s, p
		}},
	} {
		for _, tc := range []struct {
			name, fields string
			reject       []string // the Reject's fields
		}{
			{"tag within the header", "49=ACC|56=INI|34=2|abc=1|52=" + now + "|11=O1|", []string{"373", "0"}},
			{"tag within a header group", "49=ACC|56=INI|34=2|627=1|628=H|abc=1|52=" + now + "|11=O1|", []string{"373", "0"}},
			{"SendingTime missing", "49=ACC|56=INI|34=2|11=O1|abc=1|52=" + now + "|", []string{"371", "52", "373", "1"}},
		} {
			t.Run(v.name+"/"+tc.name, func(t *testing.T) {
				s, p := v.start(t)
				p.writeRaw(frameOf(v.begin, "35=D|"+tc.fields))
				p.expect("3", append([]string{"45", "2"}, tc.reject...)...)
				p.write(from(v.begin, 3), "1", "112", "AFTER")
				p.expect("0", "112", "AFTER")
				if got := s.NextTargetSeqNum(); got != 4 {
					t.Errorf("NextTargetSeqNum = %d, want 4", got)
				}
			})
		}
	}
}

// The answer to our Logout with a malformed tag is rejected, not taken as
// the answer: the session waits for one until LogoutTimeout.
func TestMalformedTagLogoutAnswer(t *testing.T) {
	s, rec, p := loggedOn(t, func(c *session.Config) { c.LogoutTimeout = 300 * time.Millisecond })
	go s.Logout(context.Background(), "bye")
	p.expect("5", "58", "bye")
	p.writeRaw(rawFrame("35=5|49=ACC|56=INI|34=2|52="+sendingTime()+"|abc=1|", ""))
	p.expect("3", "45", "2", "373", "0")
	p.expectEnd(5 * time.Second)
	rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	rec.mu.Lock()
	reason := rec.logouts[0]
	rec.mu.Unlock()
	var le *session.LogoutError
	if !errors.As(reason, &le) || !strings.HasPrefix(le.Text, "no Logout reply") {
		t.Errorf("logout reason %v, want no Logout reply", reason)
	}
}

// The LastMsgSeqNumProcessed of one rejected in sequence acknowledges our
// messages, as a well-formed message's does.
func TestMalformedTagLastMsgSeqNumProcessed(t *testing.T) {
	buf := &logBuffer{}
	_, _, p := loggedOn(t, func(c *session.Config) {
		c.Retention.Interval = time.Hour
		c.Logger = slog.New(slog.NewTextHandler(buf, nil))
	})
	p.writeRaw(rawFrame("35=D|49=ACC|56=INI|34=2|52="+sendingTime()+"|369=99|11=O1|abc=1|", ""))
	p.expect("3", "45", "2", "373", "0")
	waitFor(t, func() bool { return strings.Contains(buf.String(), "counterparty acknowledges messages we never sent") })
}

// Our Logout sent, one is still checked and rejected; why the session
// logged out, which a partial message's values do not show, is what the
// handler is told.
func TestMalformedTagLogoutSent(t *testing.T) {
	_, rec, p := loggedOn(t, func(c *session.Config) { c.LogoutTimeout = 5 * time.Second })
	const why = "Incorrect BeginString: expected FIX.4.4, received withheld"
	p.writeRaw(frameOf("FIX.4.2", "35=D|49=ACC|56=INI|34=2|52="+sendingTime()+"|11=O1|abc=1|"))
	p.expect("5", "58", why)
	p.writeRaw(rawFrame("35=D|49=ACC|56=INI|34=2|52="+sendingTime()+"|11=O1|abc=1|", ""))
	p.expect("3", "45", "2", "373", "0")
	p.write(from(session.FIX44, 3), "5")
	p.expectEnd(5 * time.Second)
	rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	rec.mu.Lock()
	reason := rec.logouts[0]
	rec.mu.Unlock()
	var le *session.LogoutError
	if !errors.As(reason, &le) || le.Text != why {
		t.Errorf("logout reason %v, want Text %q", reason, why)
	}
}

// A message parsed only up to a malformed tag may hold a credential after
// it, which the session cannot see: neither what it logs nor the Texts it
// sends, which an Observer logs, show the message's values. Only RefMsgType
// does, as received.
func TestMalformedTagLogRedaction(t *testing.T) {
	const secret = "S3CRET-VALUE"
	// The Reject written: counted, it may be not yet, and the connection
	// closed before it is.
	rejected := func(_ *session.Session, log string) bool {
		for l := range strings.SplitSeq(log, "\n") {
			if strings.Contains(l, `msg="FIX out"`) && strings.Contains(l, "|35=3|") {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct {
		name, msg, secret string
		done              func(*session.Session, string) bool
		refMsgType        bool // the Reject's RefMsgType shows it
	}{
		{name: "MsgType", msg: "35=" + secret + "|49=C|56=S|34=2|52=" + sendingTime() + "|11=O1|abc=1|554=" + secret + "|", done: rejected, refMsgType: true},
		{name: "wrong SenderCompID", msg: "35=D|49=" + secret + "|56=S|34=2|52=" + sendingTime() + "|11=O1|abc=1|554=" + secret + "|", done: rejected},
		{name: "SendingTime", msg: "35=D|49=C|56=S|34=2|52=" + secret + "|11=O1|abc=1|554=" + secret + "|", done: rejected},
		{name: "MsgSeqNum too low", msg: "35=D|49=C|56=S|34=1|52=" + sendingTime() + "|11=O1|abc=1|554=" + secret + "|",
			done: func(_ *session.Session, log string) bool { return strings.Contains(log, "logging out") }},
		{name: "MsgSeqNum too high", msg: "35=D|49=C|56=S|34=42424242|52=" + sendingTime() + "|11=O1|abc=1|554=42424242|", secret: "42424242",
			done: func(_ *session.Session, log string) bool { return strings.Contains(log, "MsgSeqNum too high") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logon := rawFrame("35=A|49=C|56=S|34=1|52="+sendingTime()+"|98=0|108=30|", "")
			out := logged(t, nil, logon, tc.done, rawFrame(tc.msg, ""))
			secret := cmp.Or(tc.secret, secret)
			echoed := false
			for l := range strings.SplitSeq(out, "\n") {
				n := strings.Count(l, secret)
				if n == 0 {
					continue
				}
				if !tc.refMsgType || !strings.Contains(l, `msg="FIX out"`) || strings.Count(l, "|372="+secret+"|") != n {
					t.Errorf("secret logged: %s", l)
				}
				echoed = true
			}
			if tc.refMsgType && !echoed {
				t.Errorf("no Reject with RefMsgType %s logged:\n%s", secret, out)
			}
		})
	}
}

// An initiator's Logon answered with a malformed tag is garbled: counted
// and logged, not dropped unsaid until the Logon times out.
func TestMalformedTagLogonAnswer(t *testing.T) {
	buf := &logBuffer{}
	l := mem.Listen("acceptor")
	s, _ := startInitiator(t, l, func(c *session.Config) { c.Logger = slog.New(slog.NewTextHandler(buf, nil)) })
	p := acceptPeer(t, l)
	p.expect("A")
	p.writeRaw(rawFrame("35=A|49=ACC|56=INI|34=1|52="+sendingTime()+"|98=0|108=30|abc=1|", ""))
	waitFor(t, func() bool { return s.Stats().Garbled.Load() == 1 })
	waitFor(t, func() bool { return strings.Contains(buf.String(), "garbled message ignored") })
}
