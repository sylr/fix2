package session_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/fix/dictionary/xmldict"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/tests/dicts/fix44"
	"github.com/sylr/fix2/transport/mem"
)

// QuickFIXTolerations holds what QuickFIX's acceptance tests need, a fresh
// value every call; a zero Config tolerates nothing.
func TestQuickFIXTolerations(t *testing.T) {
	want := session.Tolerations{
		TolerateUnsupportedDefaultApplVerID:    true,
		TolerateLogoutBeforeGapRecovery:        true,
		TolerateNoHeartbeatAwaitingTestRequest: true,
	}
	got := session.QuickFIXTolerations()
	if got != want {
		t.Errorf("QuickFIXTolerations() = %+v, want %+v", got, want)
	}
	got.TolerateLogoutBeforeGapRecovery = false
	if !session.QuickFIXTolerations().TolerateLogoutBeforeGapRecovery {
		t.Error("QuickFIXTolerations shares its value")
	}
	var c session.Config
	if c.Tolerations != (session.Tolerations{}) {
		t.Errorf("a zero Config tolerates %+v", c.Tolerations)
	}
}

// A Logout ahead of a gap is answered once the gap is recovered, or, with
// TolerateLogoutBeforeGapRecovery, at once, as QuickFIX does.
func TestTolerateLogoutBeforeGapRecovery(t *testing.T) {
	t.Run("strict", func(t *testing.T) {
		_, rec, p := loggedOn(t, nil)
		p.write(from(session.FIX44, 3), "5")
		p.expect("2", "7", "2", "16", "0")
		p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
		p.expect("5")
		rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	})
	t.Run("tolerated", func(t *testing.T) {
		_, rec, p := loggedOn(t, func(c *session.Config) { c.TolerateLogoutBeforeGapRecovery = true })
		p.write(from(session.FIX44, 3), "5")
		p.expect("5")
		rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	})
}

// A ResendRequest whose MsgSeqNum is too low, without PossDupFlag, logs out
// unserved, or, with ServeResendRequestSeqTooLow, is served first, as
// QuickFIX does.
func TestServeResendRequestSeqTooLow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve bool
	}{{"default", false}, {"served", true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, rec, p := loggedOn(t, func(c *session.Config) { c.ServeResendRequestSeqTooLow = tc.serve })
			p.send("0", 0)
			p.send("2", 2, "7", "1", "16", "0")
			if tc.serve {
				// Our Logon, an admin message, gap-filled.
				p.expect("4", "34", "1", "43", "Y", "123", "Y", "36", "2")
			}
			p.expect("5")
			rec.wait(t, func() bool { return len(rec.logouts) == 1 })
		})
	}
}

// A PossDup SequenceReset without OrigSendingTime (122) is rejected, or, with
// TolerateSequenceResetWithoutOrigSendingTime, fills its gap.
func TestTolerateSequenceResetWithoutOrigSendingTime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, p := loggedOn(t, func(c *session.Config) { c.TolerateSequenceResetWithoutOrigSendingTime = tc.tolerate })
			p.write(from(session.FIX44, 5), "0")
			p.expect("2", "7", "2", "16", "0")
			p.write(from(session.FIX44, 2), "4", "43", "Y", "123", "Y", "36", "5")
			if !tc.tolerate {
				p.expect("3", "45", "2", "371", "122")
				return
			}
			p.write(from(session.FIX44, 6), "1", "112", "AFTER")
			p.expect("0", "112", "AFTER")
			if got := s.NextTargetSeqNum(); got != 7 {
				t.Errorf("NextTargetSeqNum = %d, want 7", got)
			}
		})
	}
}

// A FIXT Logon answer whose DefaultApplVerID has no dictionary is refused, or,
// with TolerateUnsupportedDefaultApplVerID, accepted, its messages read with
// our own default version.
func TestTolerateUnsupportedDefaultApplVerID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			l := mem.Listen("acceptor")
			_, rec := startInitiator(t, l, func(c *session.Config) {
				c.ID.BeginString = session.FIXT11
				strictApps(c)
				c.TolerateUnsupportedDefaultApplVerID = tc.tolerate
			})
			p := acceptPeer(t, l)
			p.expect("A")
			p.write(from(session.FIXT11, 1), "A", "98", "0", "108", "30", "1137", "7")
			if !tc.tolerate {
				p.expectSilentEnd()
				rec.mu.Lock()
				defer rec.mu.Unlock()
				if rec.logons != 0 {
					t.Error("logged on")
				}
				return
			}
			rec.wait(t, func() bool { return rec.logons == 1 })
			// Without ApplVerID, read as FIX 5.0 SP2, our default: a valid
			// order is delivered, one without Side (54) rejected for it.
			order := func(seq int, id string, side ...string) {
				f := append([]string{"11", id}, side...)
				f = append(f, "60", "20240101-00:00:00.000", "38", "3", "40", "1")
				p.write(from(session.FIXT11, seq), "D", f...)
			}
			order(2, "O1", "54", "1")
			rec.wait(t, func() bool { return len(rec.apps) == 1 && rec.apps[0] == "O1" })
			order(3, "O2")
			p.expect("3", "45", "3", "371", "54", "373", "1")
		})
	}
}

// acceptorPeer runs a FIX 4.4 acceptor ACC for INI, cfg changed by change,
// and connects a scripted peer to it.
func acceptorPeer(t *testing.T, change func(*session.Config)) (*session.Session, *recorder, *peer) {
	t.Helper()
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	rec := newRecorder()
	cfg := session.Config{
		ID:            session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"},
		Handler:       rec,
		LogoutTimeout: 200 * time.Millisecond,
	}
	if change != nil {
		change(&cfg)
	}
	s, err := a.Add(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return s, rec, dialPeer(t, l)
}

// fromInitiator returns the header of message seq of INI to the acceptor
// ACC.
func fromInitiator(seq int) hdr {
	return hdr{begin: session.FIX44, sender: "INI", target: "ACC", seq: seq, sent: time.Now()}
}

// refused checks the connection ends without a word, rec not logged on.
func refused(t *testing.T, rec *recorder, p *peer) {
	t.Helper()
	p.expectSilentEnd()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.logons != 0 {
		t.Error("logged on")
	}
}

// A Heartbeat goes out in every idle interval, our TestRequest awaiting its
// answer or not, or, with TolerateNoHeartbeatAwaitingTestRequest, none while
// it waits, as QuickFIX sends: the disconnection follows.
func TestTolerateNoHeartbeatAwaitingTestRequest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, p := loggedOn(t, func(c *session.Config) {
				c.HeartBtInt = 2 * time.Second
				c.TolerateNoHeartbeatAwaitingTestRequest = tc.tolerate
			})
			p.expect("0", "112", "")     // 2s after our Logon
			p.expect("1", "112", "TEST") // 2.4s after the peer's
			if !tc.tolerate {
				p.expect("0", "112", "") // 2s after the TestRequest
			}
			p.expectSilentEnd() // 2.4s after the TestRequest
		})
	}
}

// A first Logon with ResetSeqNumFlag whose MsgSeqNum is not 1 is refused,
// the connection closed without a word, or, with
// TolerateResetLogonSeqNumNotOne, accepted: the sequence numbers reset, the
// messages before it are asked for.
func TestTolerateResetLogonSeqNumNotOne(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, rec, p := acceptorPeer(t, func(c *session.Config) { c.TolerateResetLogonSeqNumNotOne = tc.tolerate })
			p.write(fromInitiator(5), "A", "98", "0", "108", "30", "141", "Y")
			if !tc.tolerate {
				refused(t, rec, p)
				return
			}
			p.expect("A", "34", "1", "141", "Y")
			p.expect("2", "7", "1", "16", "0")
			rec.wait(t, func() bool { return rec.logons == 1 })
		})
	}
}

// A Logon whose NextExpectedMsgSeqNum (789) is not a positive integer is
// refused, the connection closed without a word, when the session honours
// 789 or its dictionary types it, or, with
// TolerateMalformedNextExpectedMsgSeqNum, logs on, 789 ignored.
func TestTolerateMalformedNextExpectedMsgSeqNum(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*session.Config)
	}{
		{"honoured", func(c *session.Config) { c.NextExpectedMsgSeqNum = true }},
		{"dictionary", func(c *session.Config) { c.Dictionaries.Session = fix44.Dictionary() }},
		{"both", func(c *session.Config) { c.NextExpectedMsgSeqNum = true; c.Dictionaries.Session = fix44.Dictionary() }},
	} {
		for _, tolerate := range []bool{false, true} {
			name := tc.name + "/strict"
			if tolerate {
				name = tc.name + "/tolerated"
			}
			t.Run(name, func(t *testing.T) {
				_, rec, p := acceptorPeer(t, func(c *session.Config) {
					tc.change(c)
					c.TolerateMalformedNextExpectedMsgSeqNum = tolerate
				})
				p.write(fromInitiator(1), "A", "98", "0", "108", "30", "789", "x")
				if !tolerate {
					refused(t, rec, p)
					return
				}
				p.expect("A", "34", "1")
				rec.wait(t, func() bool { return rec.logons == 1 })
			})
		}
	}
}

// With a session dictionary requiring NextExpectedMsgSeqNum (789),
// TolerateMalformedNextExpectedMsgSeqNum accepts it malformed, not absent.
func TestTolerateMalformedNextExpectedMsgSeqNumRequired(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(`<fix type="FIX" major="4" minor="4" servicepack="0">
  <header>
    <field name="BeginString" required="Y"/><field name="BodyLength" required="Y"/>
    <field name="MsgType" required="Y"/><field name="SenderCompID" required="Y"/>
    <field name="TargetCompID" required="Y"/><field name="MsgSeqNum" required="Y"/>
    <field name="SendingTime" required="Y"/>
  </header>
  <trailer><field name="CheckSum" required="Y"/></trailer>
  <messages>
    <message name="Heartbeat" msgtype="0" msgcat="admin"><field name="TestReqID" required="N"/></message>
    <message name="Logon" msgtype="A" msgcat="admin">
      <field name="EncryptMethod" required="Y"/><field name="HeartBtInt" required="Y"/>
      <field name="NextExpectedMsgSeqNum" required="Y"/>
    </message>
  </messages>
  <fields>
    <field number="8" name="BeginString" type="STRING"/><field number="9" name="BodyLength" type="LENGTH"/>
    <field number="10" name="CheckSum" type="STRING"/><field number="34" name="MsgSeqNum" type="SEQNUM"/>
    <field number="35" name="MsgType" type="STRING"/><field number="49" name="SenderCompID" type="STRING"/>
    <field number="52" name="SendingTime" type="UTCTIMESTAMP"/><field number="56" name="TargetCompID" type="STRING"/>
    <field number="98" name="EncryptMethod" type="INT"/><field number="108" name="HeartBtInt" type="INT"/>
    <field number="112" name="TestReqID" type="STRING"/><field number="789" name="NextExpectedMsgSeqNum" type="SEQNUM"/>
  </fields>
</fix>`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		fields []string
		logon  bool
	}{
		{"malformed", []string{"98", "0", "108", "30", "789", "x"}, true},
		{"absent", []string{"98", "0", "108", "30"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rec, p := acceptorPeer(t, func(c *session.Config) {
				c.Dictionaries.Session = d
				c.TolerateMalformedNextExpectedMsgSeqNum = true
			})
			p.write(fromInitiator(1), "A", tc.fields...)
			if !tc.logon {
				refused(t, rec, p)
				return
			}
			p.expect("A", "34", "1")
			rec.wait(t, func() bool { return rec.logons == 1 })
		})
	}
}

// With a session dictionary requiring EncryptMethod (98), a Logon answer
// without it is refused, or, with TolerateLogonWithoutEncryptMethod,
// accepted unless the dictionary refuses something else.
func TestTolerateLogonWithoutEncryptMethodDictionary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
		extra    []string // fields of the answer after HeartBtInt
		logon    bool
	}{
		{"strict", false, nil, false},
		{"tolerated", true, nil, true},
		{"tolerated, a tag not for Logon", true, []string{"55", "X"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := mem.Listen("acceptor")
			_, rec := startInitiator(t, l, func(c *session.Config) {
				c.Dictionaries.Session = fix44.Dictionary()
				c.TolerateLogonWithoutEncryptMethod = tc.tolerate
			})
			p := acceptPeer(t, l)
			p.expect("A")
			p.write(from(session.FIX44, 1), "A", append([]string{"108", "30"}, tc.extra...)...)
			if !tc.logon {
				refused(t, rec, p)
				return
			}
			rec.wait(t, func() bool { return rec.logons == 1 })
			p.write(from(session.FIX44, 2), "1", "112", "PING")
			p.expect("0", "112", "PING")
		})
	}
}

// A received tag number with leading zeros is malformed: a TestRequest
// whose TestReqID is written "0112" is rejected, SessionRejectReason 0, and
// counted; one whose MsgType is written "035", its type unknown, is
// disregarded as garbled. With TolerateTagLeadingZeros, both are read as
// their tag numbers say, and answered.
// A first Logon whose tag numbers have leading zeros is no valid Logon to an
// acceptor's strict session, which closes the connection (rather than
// waiting for another first message), and logs on a tolerating one.
func TestTolerateTagLeadingZerosFirstLogon(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, rec, p := acceptorPeer(t, func(c *session.Config) { c.TolerateTagLeadingZeros = tc.tolerate })
			p.writeRaw(rawFrame("35=A|49=INI|56=ACC|34=1|52="+sendingTime()+"|98=0|0108=30|", ""))
			if !tc.tolerate {
				refused(t, rec, p)
				return
			}
			p.expect("A", "34", "1")
			rec.wait(t, func() bool { return rec.logons == 1 })
		})
	}
}

func TestTolerateTagLeadingZeros(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, p := loggedOn(t, func(c *session.Config) { c.TolerateTagLeadingZeros = tc.tolerate })
			p.writeRaw(rawFrame("35=1|49=ACC|56=INI|34=2|52="+sendingTime()+"|0112=BODY|", ""))
			if tc.tolerate {
				p.expect("0", "112", "BODY")
			} else {
				p.expect("3", "45", "2", "371", "112", "373", "0", "58", "Invalid tag number")
			}
			p.writeRaw(rawFrame("035=1|49=ACC|56=INI|34=3|52="+sendingTime()+"|112=TYPE|", ""))
			next := 3
			if tc.tolerate {
				p.expect("0", "112", "TYPE")
				next = 4
			}
			p.write(from(session.FIX44, next), "1", "112", "AFTER")
			p.expect("0", "112", "AFTER")
			if got, want := s.NextTargetSeqNum(), uint64(next+1); got != want {
				t.Errorf("NextTargetSeqNum = %d, want %d", got, want)
			}
		})
	}
}
