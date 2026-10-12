package session_test

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/dictionary/xmldict"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport"
	"github.com/sylr/fix2/session/transport/mem"
)

// logBuffer collects what a logger writes.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// rawFrame builds a FIX.4.4 frame of body ("|" for SOH) with CheckSum cs,
// the right one if empty.
func rawFrame(body, cs string) []byte {
	body = strings.ReplaceAll(body, "|", "\x01")
	msg := fmt.Sprintf("8=FIX.4.4\x019=%d\x01", len(body)) + body
	if cs == "" {
		cs = fmt.Sprintf("%03d", fix2.CheckSum([]byte(msg)))
	}
	return []byte(msg + "10=" + cs + "\x01")
}

// withBodyLength returns frame with its BodyLength changed by delta.
func withBodyLength(frame []byte, delta int) []byte {
	i := bytes.Index(frame, []byte("\x019=")) + 3
	j := i + bytes.IndexByte(frame[i:], 1)
	n, _ := strconv.Atoi(string(frame[i:j]))
	return slices.Concat(frame[:i], strconv.AppendInt(nil, int64(n+delta), 10), frame[j:])
}

func sendingTime() string { return string(fix2.AppendUTCTimestamp(nil, time.Now(), fix2.Milliseconds)) }

// logged runs an acceptor of session S->C, logging at Debug with a
// SlogObserver, to which a client logs on with logon, sends msgs, and waits
// for done. It returns what was logged, once the acceptor stopped.
func logged(t *testing.T, change func(*session.Config), logon []byte, done func(*session.Session, string) bool, msgs ...[]byte) string {
	t.Helper()
	buf := &logBuffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a := session.NewAcceptor(session.AcceptorOptions{Logger: log})
	cfg := session.Config{ID: session.SessionID{BeginString: session.FIX44, SenderCompID: "S", TargetCompID: "C"},
		Handler: session.BaseHandler{}, Logger: log, Observer: session.SlogObserver{Logger: log}, LogoutTimeout: time.Millisecond}
	if change != nil {
		change(&cfg)
	}
	s, err := a.Add(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l := mem.Listen("acceptor")
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); a.Run(ctx) }()
	c, err := l.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go discard(c)
	for _, m := range append([][]byte{logon}, msgs...) {
		if _, err := c.Write(m); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return done(s, buf.String()) })
	// What the session logs as it stops is checked too.
	c.Close()
	cancel()
	<-stopped
	return buf.String()
}

func discard(c transport.Conn) {
	b := make([]byte, 4096)
	for {
		if _, err := c.Read(b); err != nil {
			return
		}
	}
}

// garbled reports whether the session ignored n garbled messages.
func garbled(n uint64) func(*session.Session, string) bool {
	return func(s *session.Session, _ string) bool { return s.Stats().Garbled.Load() >= n }
}

// Neither the session's log nor its Rejects show the value of a sensitive
// field, whatever is wrong with the message holding it.
func TestLogRedaction(t *testing.T) {
	const secret = "S3CRET-VALUE"
	logon := func(fields string) []byte {
		return rawFrame("35=A|49=C|56=S|34=1|52="+sendingTime()+"|98=0|108=30|553=USER|554="+secret+"-LOGON|"+fields, "")
	}
	order := func(fields, cs string) []byte {
		return rawFrame("35=D|49=C|56=S|34=2|52="+sendingTime()+"|11=O1|"+fields, cs)
	}
	type tc struct {
		name    string
		logon   []byte // the first message, a valid Logon if nil
		msg     []byte
		done    func(*session.Session, string) bool
		garbled bool // a garbled log shows bytes, error and offset, no MsgType nor MsgSeqNum
		// misplaced tells the secret is in a field that is not sensitive
		// too: the observer, which masks fields by tag, shows it there.
		misplaced bool
		secret    string // what must not be logged, secret if empty
	}
	cases := []tc{
		{name: "CheckSum out of range", msg: order("554="+secret+"|", "999"), done: garbled(1), garbled: true},
		{name: "CheckSum mismatch", msg: order("554="+secret+"|", "000"), done: garbled(1), garbled: true},
		{name: "reset Logon, bad CheckSum", msg: rawFrame("35=A|49=C|56=S|34=1|52="+sendingTime()+"|98=0|108=30|141=Y|553=U|554="+secret+"|", "001"),
			done: garbled(1), garbled: true},
		{name: "UserRequest NewPassword, bad CheckSum", msg: rawFrame("35=BE|49=C|56=S|34=2|52="+sendingTime()+"|923=1|924=3|553=U|554=old|925="+secret+"|", "002"),
			done: garbled(1), garbled: true},
		{name: "malformed tag, bad CheckSum", msg: order("abc=1|554="+secret+"|", "004"), done: garbled(1), garbled: true},
		{name: "malformed tag", msg: order("abc=1|554="+secret+"|", ""),
			done: func(s *session.Session, _ string) bool { return s.Stats().RejectsSent.Load() >= 1 }},
		{name: "field without equal sign", msg: order("554"+secret+"|", ""),
			done: func(s *session.Session, _ string) bool {
				return s.Stats().RejectsSent.Load()+s.Stats().Garbled.Load() >= 1
			}},
		{name: "BodyLength too small", msg: withBodyLength(order("554="+secret+"|", ""), -5), done: garbled(1)},
		// Followed by a valid message, for the framer to see this one end.
		{name: "BodyLength too large", msg: slices.Concat(withBodyLength(order("554="+secret+"|", ""), 20), rawFrame("35=0|49=C|56=S|34=2|52="+sendingTime()+"|", "")), done: garbled(1)},
		{name: "secret after 256 bytes", msg: order("58="+strings.Repeat("x", 260)+"|554="+secret+"|", "999"), done: garbled(1), garbled: true},
		{name: "valid", msg: order("554="+secret+"|", ""),
			done: func(s *session.Session, _ string) bool { return s.Stats().MessagesReceived.Load() >= 2 }},
		{name: "first Logon, bad CheckSum", logon: rawFrame("35=A|49=C|56=S|34=1|52="+sendingTime()+"|98=0|108=30|553=U|554="+secret+"|", "999"),
			done: func(_ *session.Session, log string) bool { return strings.Contains(log, "connection refused") }},
		// A LENGTH field's value, which is no number, is not quoted.
		// A number, sensitive or not, is not quoted either.
		{name: "LENGTH holding a number", msg: order("554=42424242|95=42424242|96=x|", ""), done: garbled(1), garbled: true, secret: "4242"},
		// Neither are the MsgType and MsgSeqNum of a garbled message: the
		// fields it holds cannot be told apart.
		{name: "MsgType holding a credential, bad CheckSum", msg: rawFrame("35=ABCD|49=C|56=S|34=2|52="+sendingTime()+"|554=ABCD|", "999"),
			done: garbled(1), garbled: true, secret: "ABCD"},
		{name: "invalid LENGTH", msg: order("95="+secret+"|96="+secret+"|", ""), done: garbled(1), garbled: true},
		// Numbers of a parsed message are withheld if they may show a
		// credential.
		{name: "MsgSeqNum too high holding a credential", misplaced: true, secret: "42424242",
			msg:  rawFrame("35=D|49=C|56=S|34=42424242|52="+sendingTime()+"|11=O1|554=42424242|", ""),
			done: func(_ *session.Session, log string) bool { return strings.Contains(log, "MsgSeqNum too high") }},
		{name: "NewSeqNo holding a credential", misplaced: true, secret: "42424242",
			msg:  rawFrame("35=4|49=C|56=S|34=2|52="+sendingTime()+"|36=42424242|554=42424242|", ""),
			done: func(_ *session.Session, log string) bool { return strings.Contains(log, "msg=SequenceReset") }},
		{name: "BeginSeqNo holding a credential", misplaced: true, secret: "42424242",
			msg: rawFrame("35=2|49=C|56=S|34=2|52="+sendingTime()+"|7=42424242|16=42424243|554=42424242|", ""),
			done: func(_ *session.Session, log string) bool {
				return strings.Contains(log, "ResendRequest for nothing sent yet")
			}},
		{name: "invalid LENGTH, first Logon", logon: rawFrame("35=A|49=C|56=S|34=1|52="+sendingTime()+"|98=0|108=30|95="+secret+"|96="+secret+"|", ""),
			done: func(_ *session.Session, log string) bool { return strings.Contains(log, "connection refused") }},
		// A field holding a credential by mistake: what is said of it is
		// withheld.
		{name: "first Logon, HeartBtInt invalid", misplaced: true, logon: rawFrame("35=A|49=C|56=S|34=1|52="+sendingTime()+"|98=0|108="+secret+"|554="+secret+"|", ""),
			done: func(_ *session.Session, log string) bool { return strings.Contains(log, "disconnected") }},
		{name: "reset Logon, HeartBtInt invalid", misplaced: true, msg: rawFrame("35=A|49=C|56=S|34=1|52="+sendingTime()+"|98=0|108="+secret+"|141=Y|554="+secret+"|", ""),
			done: func(s *session.Session, _ string) bool { return s.Stats().RejectsSent.Load() >= 1 }},
		{name: "MsgSeqNum invalid", misplaced: true, msg: rawFrame("35=D|49=C|56=S|34="+secret+"|52="+sendingTime()+"|11=O1|554="+secret+"|", ""),
			done: func(_ *session.Session, log string) bool { return strings.Contains(log, "logging out") }},
	}
	// Each sensitive field, in a message valid but for its CheckSum, and in
	// a valid one.
	for _, f := range []struct {
		tag    int
		fields string
	}{
		{554, "554=" + secret + "|"},
		{925, "925=" + secret + "|"},
		{1402, fmt.Sprintf("1401=%d|1402=%s|", len(secret)+2, "x\x01"+secret)},
		{1404, fmt.Sprintf("1403=%d|1404=%s|", len(secret), secret)},
		{96, fmt.Sprintf("95=%d|96=%s|", len(secret), secret)},
		{91, fmt.Sprintf("90=%d|91=%s|", len(secret), secret)},
	} {
		cases = append(cases,
			tc{name: fmt.Sprintf("tag %d, bad CheckSum", f.tag), msg: order(f.fields, "005"), done: garbled(1), garbled: true},
			tc{name: fmt.Sprintf("tag %d, valid", f.tag), msg: order(f.fields, ""),
				done: func(s *session.Session, _ string) bool { return s.Stats().MessagesReceived.Load() >= 2 }})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := c.logon
			if first == nil {
				first = logon("")
			}
			var msgs [][]byte
			if c.msg != nil {
				msgs = append(msgs, c.msg)
			}
			out := logged(t, nil, first, c.done, msgs...)
			secret := cmp.Or(c.secret, secret)
			for l := range strings.SplitSeq(out, "\n") {
				if strings.Contains(l, secret) && !(c.misplaced && strings.Contains(l, `msg="FIX in"`)) {
					t.Errorf("secret logged:\n%s", out)
					break
				}
			}
			if !c.garbled {
				return
			}
			var line string
			for l := range strings.SplitSeq(out, "\n") {
				if strings.Contains(l, "garbled message ignored") {
					line = l
				}
			}
			for _, want := range []string{"bytes=", "error=", "offset="} {
				if !strings.Contains(line, want) {
					t.Errorf("garbled log %q lacks %q", line, want)
				}
			}
			for _, unwanted := range []string{"msg_type=", " seq="} {
				if strings.Contains(line, unwanted) {
					t.Errorf("garbled log %q shows %q", line, unwanted)
				}
			}
		})
	}
}

// A Reject for an invalid value of a sensitive field does not show it, nor
// does the log, however it is quoted; one about another field is not
// withheld for a short sensitive value it happens to contain.
func TestRejectRedaction(t *testing.T) {
	// A dictionary in which Password is a number.
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
    <message name="TestRequest" msgtype="1" msgcat="admin"><field name="TestReqID" required="Y"/></message>
    <message name="Reject" msgtype="3" msgcat="admin"><field name="RefSeqNum" required="Y"/></message>
    <message name="Logon" msgtype="A" msgcat="admin">
      <field name="EncryptMethod" required="Y"/><field name="HeartBtInt" required="Y"/>
    </message>
    <message name="NewOrderSingle" msgtype="D" msgcat="app">
      <field name="ClOrdID" required="Y"/><field name="Password" required="N"/><field name="OrderQty" required="N"/>
    </message>
  </messages>
  <fields>
    <field number="8" name="BeginString" type="STRING"/><field number="9" name="BodyLength" type="LENGTH"/>
    <field number="10" name="CheckSum" type="STRING"/><field number="11" name="ClOrdID" type="STRING"/>
    <field number="38" name="OrderQty" type="INT"/><field number="34" name="MsgSeqNum" type="SEQNUM"/><field number="35" name="MsgType" type="STRING"/>
    <field number="45" name="RefSeqNum" type="SEQNUM"/><field number="49" name="SenderCompID" type="STRING"/>
    <field number="52" name="SendingTime" type="UTCTIMESTAMP"/><field number="56" name="TargetCompID" type="STRING"/>
    <field number="98" name="EncryptMethod" type="INT"/><field number="108" name="HeartBtInt" type="INT"/>
    <field number="112" name="TestReqID" type="STRING"/><field number="554" name="Password" type="INT"/>
  </fields>
</fix>`))
	if err != nil {
		t.Fatal(err)
	}
	buf := &logBuffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a := session.NewAcceptor(session.AcceptorOptions{Logger: log})
	if _, err := a.Add(session.Config{ID: session.SessionID{BeginString: session.FIX44, SenderCompID: "S", TargetCompID: "C"},
		Handler: session.BaseHandler{}, Logger: log, Observer: session.SlogObserver{Logger: log}, LogoutTimeout: time.Millisecond,
		Dictionaries: session.Dictionaries{Session: d}}); err != nil {
		t.Fatal(err)
	}
	l := mem.Listen("acceptor")
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); a.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-stopped })
	p := dialPeer(t, l)
	p.writeRaw(rawFrame("35=A|49=C|56=S|34=1|52="+sendingTime()+"|98=0|108=30|", ""))
	p.expect("A")
	badFormat := strconv.Itoa(int(dictionary.ReasonIncorrectDataFormat))
	for i, tc := range []struct {
		name, fields string
		tag          string // the Reject's RefTagID
		secret       string // what neither the Reject nor the log may show
		shown        string // what the Reject's Text must show
	}{
		{name: "secret", fields: "554=S3CRET-VALUE|", tag: "554", secret: "S3CRET-VALUE"},
		// %q escapes the tab, and byte 64 falls inside the "é".
		{name: "escaped, cut inside a rune", fields: "554=\t" + strings.Repeat("a", 62) + "\u00e9zz|", tag: "554", secret: strings.Repeat("a", 62)},
		{name: "short value, another field", fields: "554=1|38=x12|", tag: "38", shown: `"x12"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seq := strconv.Itoa(i + 2)
			p.writeRaw(rawFrame("35=D|49=C|56=S|34="+seq+"|52="+sendingTime()+"|11=O1|"+tc.fields, ""))
			r := p.expect("3", "45", seq, "371", tc.tag, "373", badFormat)
			text := string(r.Get(58))
			if tc.secret != "" && strings.Contains(text, tc.secret) {
				t.Errorf("Reject Text %q shows the secret", text)
			}
			if !strings.Contains(text, tc.shown) {
				t.Errorf("Reject Text %q lacks %q", text, tc.shown)
			}
			if out := buf.String(); tc.secret != "" && strings.Contains(out, tc.secret) {
				t.Errorf("secret logged:\n%s", out)
			}
		})
	}
}

// A counterparty's DefaultApplVerID changed by a reset Logon is logged
// withheld if it may show a credential: without dictionaries, any value is
// accepted.
func TestActiveResetDefaultApplVerIDRedaction(t *testing.T) {
	const secret = "S3CRET-VALUE"
	buf := &logBuffer{}
	_, _, p := fixtSession(t, "9", func(c *session.Config) {
		c.Dictionaries = session.Dictionaries{DefaultApplVerID: "9"}
		c.Logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	p.write(from(session.FIXT11, 1), "A", "98", "0", "108", "30", "141", "Y", "1137", secret, "554", secret)
	p.expect("A", "141", "Y")
	if out := buf.String(); strings.Contains(out, secret) {
		t.Errorf("secret logged:\n%s", out)
	}
}

// A LastMsgSeqNumProcessed acknowledging messages never sent is logged
// withheld if it may show a credential.
func TestAckRedaction(t *testing.T) {
	const secret = "42424242"
	logon := rawFrame("35=A|49=C|56=S|34=1|52="+sendingTime()+"|98=0|108=30|", "")
	msg := rawFrame("35=0|49=C|56=S|34=2|52="+sendingTime()+"|369="+secret+"|554="+secret+"|", "")
	out := logged(t, func(c *session.Config) { c.Retention.Interval = time.Hour }, logon,
		func(_ *session.Session, log string) bool {
			return strings.Contains(log, "acknowledges messages we never sent")
		}, msg)
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, secret) && !strings.Contains(l, `msg="FIX in"`) {
			t.Errorf("secret logged: %s", l)
		}
	}
}

// An acceptor refusing a Logon, whatever the reason, does not log a
// credential it holds, though the reason quotes its fields: a dynamic
// session Resolve gives identities the Logon does not have.
func TestAcceptorRefusalRedaction(t *testing.T) {
	const secret = "S3CRET-VALUE"
	buf := &logBuffer{}
	a := session.NewAcceptor(session.AcceptorOptions{
		Logger: slog.New(slog.NewTextHandler(buf, nil)),
		Resolve: func(_ context.Context, req *session.LogonRequest) (*session.Config, error) {
			id := req.ID
			id.SenderCompID = "WRONG"
			return &session.Config{ID: id}, nil
		},
	})
	l := mem.Listen("acceptor")
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); a.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	c, err := l.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go discard(c)
	if _, err := c.Write(rawFrame("35=A|49="+secret+"|56=S|34=1|52="+sendingTime()+"|98=0|108=30|554="+secret+"|", "")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(buf.String(), "connection refused") })
	if out := buf.String(); strings.Contains(out, secret) {
		t.Errorf("secret logged:\n%s", out)
	}
}
