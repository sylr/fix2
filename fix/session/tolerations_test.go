package session_test

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary/xmldict"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/tests/dicts/fix40"
	"github.com/sylr/fix2/fix/tests/dicts/fix44"
	"github.com/sylr/fix2/transport/mem"
)

// QuickFIXTolerations holds what QuickFIX's acceptance tests need, a fresh
// value every call; a zero Config tolerates nothing.
func TestQuickFIXTolerations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

// A Logout ahead of a gap is answered once the gap is recovered, or, with
// TolerateLogoutBeforeGapRecovery, at once, as QuickFIX does.
func TestTolerateLogoutBeforeGapRecovery(t *testing.T) {
	t.Run("strict", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, rec, p := loggedOn(t, nil)
			p.write(from(session.FIX44, 3), "5")
			p.expect("2", "7", "2", "16", "0")
			p.write(from(session.FIX44, 2), "0", "43", "Y", "122", origSendingTime())
			p.expect("5")
			rec.wait(t, func() bool { return len(rec.logouts) == 1 })
		})
	})
	t.Run("tolerated", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, rec, p := loggedOn(t, func(c *session.Config) { c.TolerateLogoutBeforeGapRecovery = true })
			p.write(from(session.FIX44, 3), "5")
			p.expect("5")
			rec.wait(t, func() bool { return len(rec.logouts) == 1 })
		})
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
			synctest.Test(t, func(t *testing.T) {
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
			synctest.Test(t, func(t *testing.T) {
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
			synctest.Test(t, func(t *testing.T) {
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
			synctest.Test(t, func(t *testing.T) {
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
			synctest.Test(t, func(t *testing.T) {
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
				synctest.Test(t, func(t *testing.T) {
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
			synctest.Test(t, func(t *testing.T) {
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
			synctest.Test(t, func(t *testing.T) {
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
			synctest.Test(t, func(t *testing.T) {
				_, rec, p := acceptorPeer(t, func(c *session.Config) { c.TolerateTagLeadingZeros = tc.tolerate })
				p.writeRaw(rawFrame("35=A|49=INI|56=ACC|34=1|52="+sendingTime()+"|98=0|0108=30|", ""))
				if !tc.tolerate {
					refused(t, rec, p)
					return
				}
				p.expect("A", "34", "1")
				rec.wait(t, func() bool { return rec.logons == 1 })
			})
		})
	}
}

// A tag the dictionary does not define is rejected with SessionRejectReason
// 3, Undefined tag, as the Session Layer's code list has it, or 0, Invalid
// tag number, with RejectUndefinedTagAsInvalidTagNumber, as FIXT 1.1's test
// case 14a and QuickFIX expect. A tag defined but not for the message type is
// 2, a malformed tag number 0, either way.
func TestRejectUndefinedTagAsInvalidTagNumber(t *testing.T) {
	for _, tc := range []struct {
		name      string
		invalid   bool
		undefined string
	}{{"default", false, "3"}, {"invalid tag number", true, "0"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				_, _, p := loggedOn(t, func(c *session.Config) {
					c.Dictionaries.Session = fix44.Dictionary()
					c.RejectUndefinedTagAsInvalidTagNumber = tc.invalid
				})
				p.send("1", 0, "112", "U", "4999", "x")
				p.expect("3", "45", "2", "371", "4999", "373", tc.undefined)
				p.send("1", 0, "112", "N", "11", "x")
				p.expect("3", "45", "3", "371", "11", "373", "2")
				p.writeRaw(rawFrame("35=1|49=ACC|56=INI|34=4|52="+sendingTime()+"|112=M|abc=x|", ""))
				p.expect("3", "45", "4", "373", "0")
				p.send("1", 5, "112", "AFTER")
				p.expect("0", "112", "AFTER")
			})
		})
	}
}

// timestamp returns at as a UTCTimestamp of digits fraction digits, 12 for
// picoseconds.
func timestamp(at time.Time, digits int) string {
	if digits == 12 {
		return string(fix.AppendUTCTimestamp(nil, at, fix.Nanoseconds)) + "000"
	}
	return string(fix.AppendUTCTimestamp(nil, at, fix.TimePrecision(digits)))
}

// A received SendingTime or OrigSendingTime finer than the session's FIX
// version allows is an incorrect data format, rejected (SessionRejectReason
// 6 from FIX 4.2): FIX 4.0 and 4.1 timestamps have whole seconds, FIX 4.3's
// milliseconds, while the Time Precision Addendum takes FIX 4.2 and 4.4 down
// to picoseconds. TolerateFinerTimestamps accepts them.
func TestTolerateFinerTimestamps(t *testing.T) {
	for _, v := range []struct {
		begin    string
		ok, fine int // fraction digits allowed, and finer; -1 for none
	}{
		{session.FIX40, 0, 3}, {session.FIX41, 0, 3}, {session.FIX42, 12, -1}, {session.FIX43, 3, 6}, {session.FIX44, 12, -1},
	} {
		for _, tolerate := range []bool{false, true} {
			t.Run(v.begin+"/"+map[bool]string{false: "strict", true: "tolerated"}[tolerate], func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					l := mem.Listen("acceptor")
					_, rec := startInitiator(t, l, func(c *session.Config) {
						c.ID.BeginString = v.begin
						c.TolerateFinerTimestamps = tolerate
					})
					p := acceptPeer(t, l)
					p.expect("A")
					now := func(digits int) string { return timestamp(time.Now(), digits) }
					p.writeRaw(frameOf(v.begin, "35=A|49=ACC|56=INI|34=1|52="+now(v.ok)+"|98=0|108=30|"))
					rec.wait(t, func() bool { return rec.logons == 1 })
					p.writeRaw(frameOf(v.begin, "35=1|49=ACC|56=INI|34=2|52="+now(v.ok)+"|112=OK|"))
					p.expect("0", "112", "OK")
					if v.fine < 0 {
						return
					}
					p.writeRaw(frameOf(v.begin, "35=1|49=ACC|56=INI|34=3|52="+now(v.fine)+"|112=FINE|"))
					orig := timestamp(time.Now().Add(-time.Second), v.fine)
					p.writeRaw(frameOf(v.begin, "35=1|49=ACC|56=INI|34=4|43=Y|52="+now(v.ok)+"|122="+orig+"|112=ORIG|"))
					switch {
					case tolerate:
						p.expect("0", "112", "FINE")
						p.expect("0", "112", "ORIG")
					case v.begin == session.FIX43:
						p.expect("3", "45", "3", "371", "52", "373", "6")
						p.expect("3", "45", "4", "371", "122", "373", "6")
					default:
						// No SessionRejectReason before FIX 4.2: the Text says why.
						if m := p.expect("3", "45", "3"); !strings.HasPrefix(string(m.Get(58)), "Incorrect data format for value") {
							t.Errorf("Reject Text %q, want an incorrect data format", m.Get(58))
						}
						p.expect("3", "45", "4")
					}
					p.writeRaw(frameOf(v.begin, "35=1|49=ACC|56=INI|34=5|52="+now(v.ok)+"|112=AFTER|"))
					p.expect("0", "112", "AFTER")
				})
			})
		}
	}
}

// An acceptor refuses a first Logon whose SendingTime is finer than its
// version allows, without a word, as any invalid first Logon, and logs on
// with TolerateFinerTimestamps.
func TestTolerateFinerTimestampsFirstLogon(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				_, rec, p := acceptorPeer(t, func(c *session.Config) {
					c.ID.BeginString = session.FIX43
					c.TolerateFinerTimestamps = tc.tolerate
				})
				p.writeRaw(frameOf(session.FIX43, "35=A|49=INI|56=ACC|34=1|52="+timestamp(time.Now(), 6)+"|98=0|108=30|"))
				if !tc.tolerate {
					refused(t, rec, p)
					return
				}
				p.expect("A", "34", "1")
				rec.wait(t, func() bool { return rec.logons == 1 })
			})
		})
	}
}

// The dictionaries refuse a UTCTimestamp field finer than their version
// allows (dictionary.CheckTimestampPrecision): a FIX 4.3 TransactTime of
// microseconds, unless TolerateFinerTimestamps.
func TestTolerateFinerTimestampsDictionary(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(`<fix type="FIX" major="4" minor="3" servicepack="0">
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
    </message>
    <message name="NewOrderSingle" msgtype="D" msgcat="app">
      <field name="ClOrdID" required="Y"/><field name="TransactTime" required="Y"/>
    </message>
  </messages>
  <fields>
    <field number="8" name="BeginString" type="STRING"/><field number="9" name="BodyLength" type="LENGTH"/>
    <field number="10" name="CheckSum" type="STRING"/><field number="11" name="ClOrdID" type="STRING"/>
    <field number="34" name="MsgSeqNum" type="SEQNUM"/>
    <field number="35" name="MsgType" type="STRING"/><field number="49" name="SenderCompID" type="STRING"/>
    <field number="52" name="SendingTime" type="UTCTIMESTAMP"/><field number="56" name="TargetCompID" type="STRING"/>
    <field number="60" name="TransactTime" type="UTCTIMESTAMP"/>
    <field number="98" name="EncryptMethod" type="INT"/><field number="108" name="HeartBtInt" type="INT"/>
    <field number="112" name="TestReqID" type="STRING"/>
  </fields>
</fix>`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				l := mem.Listen("acceptor")
				_, rec := startInitiator(t, l, func(c *session.Config) {
					c.ID.BeginString = session.FIX43
					c.Dictionaries.Session = d
					c.TolerateFinerTimestamps = tc.tolerate
				})
				p := acceptPeer(t, l)
				p.expect("A")
				p.writeRaw(frameOf(session.FIX43, "35=A|49=ACC|56=INI|34=1|52="+sendingTime()+"|98=0|108=30|"))
				rec.wait(t, func() bool { return rec.logons == 1 })
				p.writeRaw(frameOf(session.FIX43, "35=D|49=ACC|56=INI|34=2|52="+sendingTime()+"|11=O1|60="+timestamp(time.Now(), 6)+"|"))
				if tc.tolerate {
					rec.wait(t, func() bool { return len(rec.apps) == 1 })
					return
				}
				p.expect("3", "45", "2", "371", "60", "373", "6")
			})
		})
	}
}

// A FIX 4.0 dictionary's timestamps, of type TIME, have whole seconds: its
// checks refuse a SendingTime of milliseconds, as the session's do, and
// TolerateFinerTimestamps lets both pass it.
func TestTolerateFinerTimestampsFIX40Dictionary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				l := mem.Listen("acceptor")
				_, rec := startInitiator(t, l, func(c *session.Config) {
					c.ID.BeginString = session.FIX40
					c.Dictionaries.Session = fix40.Dictionary()
					c.TolerateFinerTimestamps = tc.tolerate
				})
				p := acceptPeer(t, l)
				p.expect("A")
				p.writeRaw(frameOf(session.FIX40, "35=A|49=ACC|56=INI|34=1|52="+timestamp(time.Now(), 0)+"|98=0|108=30|"))
				rec.wait(t, func() bool { return rec.logons == 1 })
				p.writeRaw(frameOf(session.FIX40, "35=1|49=ACC|56=INI|34=2|52="+timestamp(time.Now(), 3)+"|112=FINE|"))
				if tc.tolerate {
					p.expect("0", "112", "FINE")
				} else {
					p.expect("3", "45", "2")
				}
				p.writeRaw(frameOf(session.FIX40, "35=1|49=ACC|56=INI|34=3|52="+timestamp(time.Now(), 0)+"|112=AFTER|"))
				p.expect("0", "112", "AFTER")
			})
		})
	}
}

// A required group whose NumInGroup is 0 is rejected, SessionRejectReason
// 16, as FIX requires its count above zero; TolerateEmptyRequiredGroups
// accepts it, as an empty book's MarketDataSnapshotFullRefresh, and nothing
// else: a count its entries do not match is still rejected.
func TestTolerateEmptyRequiredGroups(t *testing.T) {
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
    </message>
    <message name="MarketDataSnapshotFullRefresh" msgtype="W" msgcat="app">
      <field name="Symbol" required="Y"/>
      <group name="NoMDEntries" required="Y"><field name="MDEntryType" required="Y"/><field name="MDEntryPx" required="N"/></group>
    </message>
  </messages>
  <fields>
    <field number="8" name="BeginString" type="STRING"/><field number="9" name="BodyLength" type="LENGTH"/>
    <field number="10" name="CheckSum" type="STRING"/><field number="34" name="MsgSeqNum" type="SEQNUM"/>
    <field number="35" name="MsgType" type="STRING"/><field number="49" name="SenderCompID" type="STRING"/>
    <field number="52" name="SendingTime" type="UTCTIMESTAMP"/><field number="55" name="Symbol" type="STRING"/>
    <field number="56" name="TargetCompID" type="STRING"/>
    <field number="98" name="EncryptMethod" type="INT"/><field number="108" name="HeartBtInt" type="INT"/>
    <field number="112" name="TestReqID" type="STRING"/><field number="268" name="NoMDEntries" type="NUMINGROUP"/>
    <field number="269" name="MDEntryType" type="CHAR"/><field number="270" name="MDEntryPx" type="PRICE"/>
  </fields>
</fix>`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				_, rec, p := loggedOn(t, func(c *session.Config) {
					c.Dictionaries.Session = d
					c.TolerateEmptyRequiredGroups = tc.tolerate
				})
				p.send("W", 0, "55", "EUR/USD", "268", "0")
				if tc.tolerate {
					rec.wait(t, func() bool { return len(rec.apps) == 1 })
				} else {
					p.expect("3", "45", "2", "371", "268", "373", "16")
				}
				p.send("W", 0, "55", "EUR/USD", "268", "2", "269", "0", "270", "1.1")
				p.expect("3", "45", "3", "371", "268", "373", "16")
			})
		})
	}
}

// Timestamps finer than the session's version allows are a configuration
// error, not clamped: FIX 4.0 and 4.1 write whole seconds, FIX 4.3
// milliseconds, FIX 4.2, 4.4 and FIXT 1.1 down to nanoseconds.
func TestConfigTimestamps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, tc := range []struct {
			begin string
			ts    session.TimestampPrecision
			ok    bool
		}{
			{session.FIX40, session.DefaultTimestamps, true},
			{session.FIX40, session.SecondTimestamps, true},
			{session.FIX40, session.MillisecondTimestamps, false},
			{session.FIX41, session.MicrosecondTimestamps, false},
			{session.FIX42, session.NanosecondTimestamps, true},
			{session.FIX43, session.MillisecondTimestamps, true},
			{session.FIX43, session.MicrosecondTimestamps, false},
			{session.FIX44, session.MicrosecondTimestamps, true},
			{session.FIXT11, session.NanosecondTimestamps, true},
		} {
			cfg := session.Config{
				ID:         session.SessionID{BeginString: tc.begin, SenderCompID: "INI", TargetCompID: "ACC"},
				Handler:    &session.BaseHandler{},
				Timestamps: tc.ts,
			}
			if tc.begin == session.FIXT11 {
				cfg.Dictionaries.DefaultApplVerID = "9"
			}
			err := cfg.Validate()
			if tc.ok && err != nil || !tc.ok && (err == nil || !strings.Contains(err.Error(), "Timestamps:")) {
				t.Errorf("%s, Timestamps %d: Validate = %v, want valid %v", tc.begin, tc.ts, err, tc.ok)
			}
		}
		// A FIX 4.4 session writes the microseconds asked for.
		l := mem.Listen("acceptor")
		startInitiator(t, l, func(c *session.Config) { c.Timestamps = session.MicrosecondTimestamps })
		p := acceptPeer(t, l)
		logon := p.expect("A")
		if v := logon.Get(52); len(v) != len("20060102-15:04:05.000000") {
			t.Errorf("SendingTime %q, want microseconds", v)
		}
	})
}

func TestTolerateTagLeadingZeros(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
	}{{"strict", false}, {"tolerated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
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
		})
	}
}
