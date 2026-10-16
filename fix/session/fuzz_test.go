package session_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/dictionary/xmldict"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/schedule"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// The fuzz targets run a session against a raw counterparty whose messages
// the fuzzer writes. A script is a sequence of lines, one message each, with
// '|' standing for SOH and $T for the current time. A line is framed by the
// harness (BeginString, BodyLength and CheckSum around its body) unless it
// starts with '!': the rest of it is then written as it is. The header and
// the body, MsgType and MsgSeqNum included, are the fuzzer's.

// Fuzz configuration bits.
const (
	fuzzFIXT  uint32 = 1 << iota // FIXT.1.1 and FIX 5.0 SP2, else FIX 4.4
	fuzzDicts                    // validate with dictionaries
	fuzzResetOnLogon
	fuzzNextExpected
	fuzzLastProcessed
	fuzzChunks
	fuzzStrict
	fuzzActive  // the handler acts in its callbacks
	fuzzRefuse  // Authenticate refuses some Logons
	fuzzObserve // a SlogObserver sees the messages
	fuzzVerbose // the session logs at debug level
	fuzzResets  // ResetOnLogout and ResetOnDisconnect
	fuzzNoLatency
	fuzzEager    // AckProbeEager
	fuzzTight    // small queues and stashes
	fuzzExtra    // acceptor: Resolve and Route; initiator: ToAdmin cancels the Logon
	fuzzSchedule // a schedule whose window is open
	fuzzClosed   // a schedule whose window is not
	fuzzAsync    // the observer is an AsyncObserver
	fuzzSign     // initiator: the Logon is signed
)

const (
	fuzzTimeout = 5 * time.Second
	fuzzMaxLine = 48
)

var fuzzLoaded = struct {
	once                sync.Once
	fix44, fixt, fix50s *dictionary.Dict
}{}

func fuzzDictionaries(cfg uint32) session.Dictionaries {
	d := &fuzzLoaded
	d.once.Do(func() {
		load := func(name string) *dictionary.Dict {
			dict, err := xmldict.LoadFile("../cmd/fixgen/testdata/" + name)
			if err != nil {
				panic(err)
			}
			return dict
		}
		d.fix44, d.fixt, d.fix50s = load("FIX44.xml"), load("FIXT11.xml"), load("FIX50SP2.xml")
	})
	if cfg&fuzzDicts == 0 {
		if cfg&fuzzFIXT != 0 {
			return session.Dictionaries{DefaultApplVerID: "9"}
		}
		return session.Dictionaries{}
	}
	if cfg&fuzzFIXT != 0 {
		return session.Dictionaries{Session: d.fixt, Apps: map[string]*dictionary.Dict{"9": d.fix50s}, DefaultApplVerID: "9"}
	}
	return session.Dictionaries{Session: d.fix44}
}

// fuzzScript turns a script into the bytes to write, one slice per line.
// $N in a line is the next MsgSeqNum, counting from 1, $P the last one
// again and $J a number five ahead of the next one, a gap.
func fuzzScript(begin, script string) [][]byte {
	now := time.Now().UTC().Format("20060102-15:04:05.000")
	lines := strings.Split(script, "\n")
	lines = lines[:min(len(lines), fuzzMaxLine)]
	seq := 0
	var out [][]byte
	for _, line := range lines {
		line = strings.ReplaceAll(line, "$T", now)
		for {
			i := strings.Index(line, "$")
			if i < 0 || i+1 >= len(line) {
				break
			}
			switch line[i+1] {
			case 'N':
				seq++
			case 'J':
				seq += 6
			case 'P':
			default:
				i = -1
			}
			if i < 0 {
				break
			}
			line = line[:i] + strconv.Itoa(seq) + line[i+2:]
		}
		if rest, ok := strings.CutPrefix(line, "!"); ok {
			out = append(out, []byte(strings.ReplaceAll(rest, "|", "\x01")))
			continue
		}
		body := strings.ReplaceAll(line, "|", "\x01")
		if !strings.HasSuffix(body, "\x01") {
			body += "\x01"
		}
		msg := "8=" + begin + "\x019=" + strconv.Itoa(len(body)) + "\x01" + body
		sum := 0
		for i := 0; i < len(msg); i++ {
			sum += int(msg[i])
		}
		out = append(out, []byte(msg+fmt.Sprintf("10=%03d\x01", sum%256)))
	}
	return out
}

// fuzzObserver counts the messages seen, and passes them on.
type fuzzObserver struct {
	in, out atomic.Int64
	next    session.MessageObserver
}

func (o *fuzzObserver) OnInbound(s *session.Session, f session.Frame) {
	o.in.Add(1)
	if o.next != nil {
		o.next.OnInbound(s, f)
	}
}

func (o *fuzzObserver) OnOutbound(s *session.Session, f session.Frame) {
	o.out.Add(1)
	if o.next != nil {
		o.next.OnOutbound(s, f)
	}
}

// fuzzHandler is a Handler that acts in its callbacks.
type fuzzHandler struct {
	session.BaseHandler
	cfg   uint32
	begin string
	n     atomic.Int64
}

func (h *fuzzHandler) Authenticate(_ context.Context, req *session.LogonRequest) error {
	if h.cfg&fuzzRefuse != 0 && (string(req.Logon.Get(553)) == "bad" || h.n.Add(1)%3 == 0) {
		return errors.New("refused")
	}
	return nil
}

func (h *fuzzHandler) ToAdmin(_ context.Context, _ *session.Session, msgType string, b *fix.Builder) error {
	if h.cfg&fuzzActive == 0 {
		return nil
	}
	if msgType == "A" && h.cfg&fuzzExtra != 0 && h.cfg&fuzzRefuse != 0 {
		return errors.New("cancelled")
	}
	b.SetString(58, "fuzz")
	return nil
}

func (h *fuzzHandler) SignLogon(_ context.Context, _ *session.Session, lh session.LogonHeader, b *fix.Builder) error {
	if h.cfg&fuzzSign == 0 {
		return nil
	}
	if h.cfg&fuzzRefuse != 0 && lh.MsgSeqNum%2 == 0 {
		return errors.New("cannot sign")
	}
	b.SetData(95, 96, []byte(lh.SendingTime+lh.SenderCompID))
	return nil
}

func (h *fuzzHandler) FromApp(ctx context.Context, s *session.Session, m *fix.Message, in session.Inbound) error {
	if h.cfg&fuzzActive == 0 {
		return nil
	}
	n := h.n.Add(1)
	if id := m.Get(11); len(id) > 0 {
		n += int64(id[0])
	}
	switch n % 8 {
	case 1:
		s.Send(ctx, fix.NewBuilder(h.begin, "8").SetString(11, "x").SetString(55, "X"))
	case 2:
		return &session.RejectError{Reason: dictionary.ReasonValueIsIncorrect, Tag: 55, Text: "no"}
	case 3:
		return &session.BusinessRejectError{Reason: session.BusinessRejectOther, RefID: "r", Text: "no"}
	case 4:
		return fix.ErrUnsupportedMessageType
	case 5:
		return errors.New("other")
	case 6:
		s.SendRaw(ctx, "D", []byte("11=y\x0155=Z\x01"))
	case 7:
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		s.Logout(cctx, "bye")
	}
	return nil
}

func (h *fuzzHandler) OnResend(_ context.Context, _ *session.Session, seq uint64, _ *fix.Message) bool {
	return h.cfg&fuzzActive == 0 || seq%2 == 0
}

// fuzzConfig returns the configuration of a session from cfg.
func fuzzConfig(cfg uint32, id session.SessionID) (session.Config, *fuzzObserver, func()) {
	c := session.Config{
		ID:            id,
		Dictionaries:  fuzzDictionaries(cfg),
		HeartBtInt:    time.Second,
		LogonTimeout:  200 * time.Millisecond,
		LogoutTimeout: 50 * time.Millisecond,
		ResetOnLogon:  cfg&fuzzResetOnLogon != 0,
		ResetOnLogout: cfg&fuzzResets != 0, ResetOnDisconnect: cfg&fuzzResets != 0,
		NextExpectedMsgSeqNum:  cfg&fuzzNextExpected != 0,
		LastMsgSeqNumProcessed: cfg&fuzzLastProcessed != 0,
		Handler:                &fuzzHandler{cfg: cfg, begin: id.BeginString},
	}
	if cfg&fuzzChunks != 0 {
		c.ResendChunkSize = 2
	}
	if cfg&fuzzStrict != 0 {
		c.Compatibility = session.CompatStrict2020
		c.Leniency.SequenceResetWithoutOrigSendingTime = true
	}
	if cfg&fuzzNoLatency != 0 {
		c.MaxLatency = -1
	}
	if cfg&fuzzEager != 0 {
		c.AckProbe = session.AckProbeEager
	}
	if cfg&fuzzTight != 0 {
		c.MaxStash, c.DeliveryQueue, c.MaxHeldMessages, c.MaxPendingMessages = 3, 2, 2, 4
		c.FailWhenFull = true
	}
	obs := &fuzzObserver{}
	c.Observer = obs
	if cfg&(fuzzObserve|fuzzAsync) != 0 {
		obs.next = session.SlogObserver{
			Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})),
			Dictionary: c.Dictionaries.Session,
		}
	}
	cleanup := func() {}
	if cfg&fuzzAsync != 0 {
		async := session.NewAsyncObserver(obs.next, 4)
		obs.next, cleanup = async, async.Close
	}
	if cfg&fuzzVerbose != 0 {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if cfg&(fuzzSchedule|fuzzClosed) != 0 {
		start := time.Now().UTC().Hour()
		if cfg&fuzzClosed != 0 {
			start = (start + 2) % 24
		}
		c.Schedule = schedule.Daily{Start: schedule.Clock(start, 0, 0), End: schedule.Clock((start+1)%24, 0, 0), Location: time.UTC}
		if cfg&fuzzClosed == 0 {
			c.Schedule = schedule.Daily{Start: schedule.Clock((start+23)%24, 0, 0), End: schedule.Clock((start+1)%24, 0, 0), Location: time.UTC}
		}
	}
	return c, obs, cleanup
}

// fuzzPeer is the raw counterparty: it writes the script and drains what the
// session writes.
type fuzzPeer struct {
	conn  transport.Conn
	drain chan struct{} // closed once the session closed the connection
	got   atomic.Int64  // bytes read
}

func newFuzzPeer(conn transport.Conn) *fuzzPeer {
	p := &fuzzPeer{conn: conn, drain: make(chan struct{})}
	go func() {
		defer close(p.drain)
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			p.got.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	return p
}

// write writes the script, stopping if the session closed the connection.
func (p *fuzzPeer) write(frames [][]byte, after func(int)) {
	p.conn.SetWriteDeadline(time.Now().Add(fuzzTimeout))
	for i, f := range frames {
		if _, err := p.conn.Write(f); err != nil {
			return
		}
		after(i)
	}
}

// waitRead waits for the session to write something, or to close the
// connection.
func (p *fuzzPeer) waitRead(d time.Duration) {
	deadline := time.After(d)
	for p.got.Load() == 0 {
		select {
		case <-p.drain:
			return
		case <-deadline:
			return
		case <-time.After(200 * time.Microsecond):
		}
	}
}

// settle waits until the session closed the connection or went quiet: it
// neither read nor wrote for a few milliseconds.
func (p *fuzzPeer) settle(obs *fuzzObserver) {
	deadline := time.After(100 * time.Millisecond)
	var last [3]int64
	for stable := 0; stable < 4; {
		select {
		case <-p.drain:
			return
		case <-deadline:
			return
		case <-time.After(500 * time.Microsecond):
		}
		cur := [3]int64{obs.in.Load(), obs.out.Load(), p.got.Load()}
		if cur == last {
			stable++
		} else {
			stable, last = 0, cur
		}
	}
}

// fuzzStop cancels ctx and fails the test if run does not return in time.
func fuzzStop(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(fuzzTimeout):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func fuzzID(cfg uint32, sender, target string) (session.SessionID, string) {
	begin := session.FIX44
	if cfg&fuzzFIXT != 0 {
		begin = session.FIXT11
	}
	return session.SessionID{BeginString: begin, SenderCompID: sender, TargetCompID: target}, begin
}

// fuzzOps are the operations an application performs on a session, one
// after each message the peer writes: byte b is operation b%12 and has
// argument b/12.
type fuzzOps struct {
	begin   string
	ops     []byte
	session func() *session.Session
	remove  func(context.Context, session.SessionID) error
	list    func() // lists the sessions
	settle  func() // waits for the session to act on what the peer wrote
}

func (o *fuzzOps) run(i int) {
	if i >= len(o.ops) {
		return
	}
	// The operation acts on the session as the messages written so far left
	// it, whatever the scheduling: an input reproduces.
	o.settle()
	o.list()
	s := o.session()
	if s == nil {
		return
	}
	b := o.ops[i]
	arg := int(b / 12)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer cancel()
	switch b % 12 {
	case 0:
		r, err := s.Send(ctx, fix.NewBuilder(o.begin, "D").SetString(11, "op").SetString(55, "X"))
		if err == nil {
			r.Wait(ctx, []session.Level{session.Written, session.Acknowledged, session.Accepted}[arg%3])
			r.Seq()
			r.Generation()
			r.Err()
			r.Rejection()
		}
	case 1:
		s.SendRaw(ctx, "8", []byte("11=raw\x0155=X\x01"))
	case 2:
		s.Logout(ctx, "op")
	case 3:
		s.SetNextSenderSeqNum(s.NextSenderSeqNum() + uint64(arg))
	case 4:
		s.SetNextTargetSeqNum(uint64(arg))
	case 5:
		s.State()
		s.Stats()
		s.NextTargetSeqNum()
	case 6:
		o.remove(ctx, s.ID())
	case 7:
		s.Stop(ctx)
	case 8:
		s.Send(ctx, fix.NewBuilder(o.begin, []string{"0", "1", "2", "3", "4", "5", "A", "j", "", "ZZ"}[arg%10]).SetInt(36, 5))
	case 9:
		s.Send(ctx, fix.NewBuilder(o.begin, "D").SetString(11, "a").SetInt(arg*37, 1))
	}
}

const (
	fuzzL    = "35=A|49=INI|56=ACC|34=$N|52=$T|98=0|108=30"
	fuzzLT   = fuzzL + "|1137=9"
	fuzzApp  = "\n35=D|49=INI|56=ACC|34=$N|52=$T|11=A|55=X|54=1|60=$T|40=1|38=1"
	fuzzAppT = "\n35=D|49=INI|56=ACC|34=$N|52=$T|1128=9|11=A|55=X|54=1|60=$T|40=1|38=1"
	fuzzHB   = "\n35=0|49=INI|56=ACC|34=$N|52=$T"
	fuzzTest = "\n35=1|49=INI|56=ACC|34=$N|52=$T|112=T1"
	fuzzOut  = "\n35=5|49=INI|56=ACC|34=$N|52=$T|58=bye"
)

// fuzzSeeds adds the seed corpus: configurations by scripts by operations.
// swap rewrites the scripts for the counterparty's side.
func fuzzSeeds(f *testing.F, swap func(string) string) {
	m := func(mt, seq, extra string) string {
		return "\n35=" + mt + "|49=INI|56=ACC|34=" + seq + "|52=$T|" + extra
	}
	scripts := []struct {
		script string
		ops    []byte
	}{
		{fuzzL + fuzzHB + fuzzApp + fuzzTest + fuzzApp + m("2", "$N", "7=1|16=0") + fuzzOut, nil},
		// We send, then the peer asks for it all, and for ranges beyond.
		{fuzzL + fuzzApp + m("2", "$N", "7=1|16=0") + m("2", "$N", "7=2|16=2") + m("2", "$N", "7=9|16=3") + m("2", "$N", "7=0|16=0") + fuzzHB, []byte{0, 0, 12, 24, 0, 1, 0}},
		// A gap: the peer's messages ahead of it are stashed, the gap is filled.
		{fuzzL + fuzzApp + m("D", "$J", "11=G1|55=X|54=1|60=$T|40=1|38=1") + m("D", "$N", "11=G2|55=X|54=1|60=$T|40=1|38=1") + m("D", "$N", "11=G3|55=X") + m("4", "3", "43=Y|122=$T|123=Y|36=8") + fuzzHB, nil},
		// The gap is recovered with PossDup copies, each with OrigSendingTime.
		{fuzzL + fuzzApp + m("D", "$J", "11=G1|55=X|54=1|60=$T|40=1|38=1") + m("D", "3", "43=Y|122=$T|11=P3|55=X|54=1|60=$T|40=1|38=1") + m("4", "4", "43=Y|122=$T|123=Y|36=7") + m("D", "7", "43=Y|122=$T|11=P7|55=X|54=1|60=$T|40=1|38=1"), nil},
		// Resets: forward, backward, with and without the gap-fill flag.
		{fuzzL + m("4", "$N", "36=20") + m("4", "$N", "36=3") + m("4", "21", "123=Y|36=25") + m("4", "26", "123=Y|36=5") + m("4", "0", "36=30") + fuzzHB, nil},
		{fuzzL + m("4", "$N", "123=Y|36=9") + m("4", "9", "123=Y|36=9") + m("4", "9", "43=Y|36=30") + m("4", "9", "43=Y|122=$T|123=Y|36=3") + fuzzHB, nil},
		// A second Logon, with and without a reset, mid-session.
		{fuzzL + fuzzApp + "\n" + fuzzL + "|141=Y" + fuzzApp + "\n" + fuzzL + "|141=N|789=2", []byte{0, 0, 0}},
		{fuzzL + "|141=Y|789=1" + fuzzApp + fuzzHB + fuzzOut, []byte{0, 0, 0, 2}},
		// Garbage before and after the Logon.
		{"!8=FIX.4.4|9=5|35=0|10=161|\n!garbage\n!8=FIX.4.4|9=99999999999|", nil},
		{"!8=FIX.4.4|9=0|10=000|\n!\x00\x00\n" + fuzzL + "\n!8=FIX.4.4|9=3|35=0|10=0", nil},
		{"35=0|49=INI|56=ACC|34=1|52=$T\n" + fuzzL, nil},
		{fuzzL + "|553=bad|141=Y" + fuzzApp + fuzzOut, nil},
		// Heartbeats, TestRequests and Rejects.
		{fuzzL + fuzzTest + m("0", "$N", "112=T1") + m("0", "$N", "112=nope") + m("1", "$N", "") + m("3", "$N", "45=2|373=1|371=55|58=x") + m("j", "$N", "380=0|58=x") + m("3", "$N", "45=9999|373=99") + fuzzOut + fuzzOut, []byte{0, 5, 0, 0, 5}},
		// Time and sequence problems: stale, missing, zero and huge numbers.
		{fuzzL + "\n35=D|49=INI|56=ACC|34=$N|52=19990101-00:00:00|11=A\n35=D|49=INI|56=ACC|34=$N|11=B\n35=D|49=INI|56=ACC|34=0|52=$T|11=C\n35=D|49=INI|56=ACC|34=18446744073709551615|52=$T|11=D\n35=D|49=XXX|56=ACC|34=$N|52=$T|11=E\n35=D|49=INI|56=XXX|34=$N|52=$T|11=F", nil},
		{fuzzL + fuzzApp + m("D", "2", "43=Y|11=NOORIG") + m("D", "2", "43=Y|122=$T|11=DUP") + m("D", "2", "97=Y|11=RESEND") + m("D", "1", "43=Y|122=19990101-00:00:00|11=OLD"), nil},
		// A Logon with odd heartbeat intervals, Logout while resending.
		{"35=A|49=INI|56=ACC|34=1|52=$T|98=0|108=0\n" + fuzzOut, nil},
		{"35=A|49=INI|56=ACC|34=1|52=$T|98=0|108=999\n35=A|49=INI|56=ACC|34=1|52=$T|98=1|108=30", nil},
		{"35=A|49=INI|56=ACC|34=5|52=$T|98=0|108=30|789=1\n" + m("2", "$N", "7=1|16=0") + fuzzOut, nil},
		{fuzzL + fuzzApp + fuzzApp + fuzzOut + m("2", "$N", "7=1|16=0"), []byte{0, 0, 0, 0, 2}},
		// Operations of the application: all of them, early and late.
		{fuzzL + fuzzApp + fuzzApp + fuzzApp + fuzzApp + fuzzApp + fuzzApp + fuzzApp + fuzzApp + fuzzApp + fuzzApp, []byte{0, 0, 27, 40, 3 + 12*5, 4 + 12*3, 5, 8, 8 + 12, 8 + 12*2, 8 + 12*3, 9, 6}},
		{fuzzL + fuzzApp + fuzzApp + fuzzApp, []byte{0, 4 + 12, 3 + 12*7, 5}},
		{fuzzL + fuzzApp + fuzzApp + fuzzApp, []byte{7, 0}},
		{fuzzL + fuzzApp + fuzzApp + fuzzApp, []byte{0, 0, 6, 0}},
		{fuzzL + fuzzApp + fuzzApp + fuzzApp, []byte{0, 2, 0, 0}},
		{fuzzL + fuzzApp + m("2", "$N", "7=1|16=0") + fuzzApp, []byte{0, 0, 3 + 12*4, 0, 8 + 12*4}},
		// Malformed fields and headers.
		{fuzzL + m("D", "$N", "0=x|11=A") + m("D", "$N", "-5=1|11=A") + m("D", "$N", "abc=1|11=A") + m("D", "$N", "11|55=X") + m("D", "$N", "=5|11=A") + m("D", "$N", "||11=A") + m("D", "$N", "11=A|11=B|55=X") + m("D", "$N", "99999999999999999999=1"), nil},
		{fuzzL + "\n35=D|56=ACC|34=$N|52=$T|11=A\n35=D|49=INI|34=$N|52=$T|11=A\n35=D|49=INI|56=ACC|52=$T|34=$N|11=A\n35=D|49=INI|56=ACC|50=SUB|57=SUB|34=$N|52=$T|11=A\n35=D|49=INI|56=ACC|115=ONB|128=DLV|144=X|129=Y|34=$N|52=$T|11=A|369=1", nil},
		{fuzzL + "\n35=|49=INI|56=ACC|34=$N|52=$T\n35=ZZZ|49=INI|56=ACC|34=$N|52=$T|11=A\n49=INI|56=ACC|34=$N|52=$T\n", nil},
		{fuzzL + m("3", "$N", "45=0|373=1|58=\xc3\xa9\x07\x00end") + m("5", "$N", "58=\xff\xfe\x01"), nil},
		{"35=A|49=INI|50=S|142=L|56=ACC|57=T|143=M|34=1|52=$T|98=0|108=30\n35=0|49=INI|50=S|142=L|56=ACC|57=T|143=M|34=2|52=$T", nil},
		{fuzzL + "|95=3|96=abc|553=u|554=p|925=n|464=Y|384=1|372=D|385=R", nil},
		// A Logon of another BeginString, with a malformed SendingTime,
		// without MsgSeqNum, with an empty SenderCompID or TargetCompID.
		{"!8=FIX.4.2|9=57|35=A|49=INI|56=ACC|34=1|52=20231001-12:00:00|98=0|108=30|10=197|", nil},
		{"35=A|49=INI|56=ACC|34=1|52=x|98=0|108=30", nil},
		{"35=A|49=INI|56=ACC|52=$T|98=0|108=30", nil},
		{"35=A|49=|56=ACC|34=1|52=$T|98=0|108=30", nil},
		{"35=A|49=INI|56=|34=1|52=$T|98=0|108=30", nil},
		{"35=A|49=ACC|56=|34=1|52=$T|98=0|108=30", nil},
		// A Reject of the initiator's Logon, of a reason that is not a
		// number.
		{"35=3|49=INI|56=ACC|34=1|52=$T|45=1|373=x|58=no", nil},
		// A bad tag after a MsgSeqNum too high, a malformed OrigSendingTime.
		{fuzzL + "\n35=0|49=INI|56=ACC|34=9|52=$T|5x=1", nil},
		{fuzzL + m("D", "$N", "11=A") + m("D", "2", "43=Y|122=bad|11=B") + m("4", "2", "43=Y|123=Y|36=4"), nil},
		// SequenceResets without NewSeqNo, a GapFill whose NewSeqNo is not a
		// number, a Reset whose MsgSeqNum is not a number.
		{fuzzL + m("4", "$N", "") + m("4", "$N", "123=Y|36=x") + m("4", "x", "36=40"), nil},
		// ResendRequests without BeginSeqNo or EndSeqNo, with EndSeqNo not a
		// number, from another sender, and too high.
		{fuzzL + m("2", "$N", "16=0") + m("2", "$N", "7=1") + m("2", "$N", "7=1|16=x") + "\n35=2|49=XXX|56=ACC|34=$N|52=$T|7=1|16=0", nil},
		{fuzzL + fuzzApp + m("2", "$J", "7=1|16=0"), nil},
		// Logouts too high, without Text, from another sender, and from
		// another sender answering ours, which a MsgSeqNum that is not a
		// number had the session send.
		{fuzzL + m("5", "$J", "58=bye"), nil},
		{fuzzL + m("5", "$N", ""), nil},
		{fuzzL + "\n35=5|49=XXX|56=ACC|34=$N|52=$T", nil},
		{fuzzL + m("0", "x", "") + "\n35=5|49=XXX|56=ACC|34=$N|52=$T", []byte{5, 5}},
		// NextTargetSeqNum set to 0, a Send after Stop.
		{fuzzL + fuzzHB + fuzzHB, []byte{4, 7, 0}},
	}
	scriptsT := []struct {
		script string
		ops    []byte
	}{
		{fuzzLT + fuzzHB + fuzzAppT + fuzzTest + fuzzOut, nil},
		{fuzzLT + "\n35=D|49=INI|56=ACC|34=$N|52=$T|1128=0|11=A|55=X\n35=D|49=INI|56=ACC|34=$N|52=$T|1128=9|11=B|55=X" + m("2", "$N", "7=1|16=0") + m("4", "$N", "36=40"), []byte{0, 0, 0}},
		{fuzzLT + "|1137=7\n35=D|49=INI|56=ACC|34=$N|52=$T|1128=8|11=A|55=X" + fuzzAppT + fuzzAppT, []byte{0, 0}},
		{"35=A|49=INI|56=ACC|34=1|52=$T|98=0|108=30\n" + fuzzOut, nil},
		{"35=A|49=INI|56=ACC|34=1|52=$T|98=0|108=30|1137=99\n" + fuzzAppT, nil},
	}
	for _, cfg := range []uint32{
		0, fuzzDicts | fuzzObserve, fuzzDicts | fuzzActive | fuzzObserve | fuzzVerbose | fuzzNoLatency,
		fuzzResetOnLogon | fuzzActive | fuzzChunks, fuzzNextExpected | fuzzLastProcessed | fuzzDicts,
		fuzzStrict | fuzzTight | fuzzActive | fuzzEager, fuzzRefuse | fuzzActive, fuzzExtra | fuzzActive | fuzzDicts,
		fuzzResets | fuzzDicts | fuzzActive, fuzzSchedule | fuzzAsync | fuzzSign, fuzzClosed | fuzzActive,
		fuzzNextExpected | fuzzChunks | fuzzActive | fuzzNoLatency, fuzzStrict | fuzzDicts | fuzzNextExpected | fuzzSign | fuzzRefuse,
	} {
		for _, s := range scripts {
			f.Add(cfg, swap(s.script), s.ops)
		}
		for _, s := range scriptsT {
			f.Add(cfg|fuzzFIXT, swap(s.script), s.ops)
		}
	}
	// Scripts that need a configuration of their own.
	for _, s := range []struct {
		cfg    uint32
		script string
	}{
		// A malformed NextExpectedMsgSeqNum, and one beyond ours.
		{fuzzNextExpected, fuzzL + "|789=x"},
		{fuzzNextExpected, fuzzL + "|789=99"},
		// The initiator's ToAdmin cancels its Logon.
		{fuzzExtra | fuzzRefuse | fuzzActive, fuzzL + fuzzApp},
		// A Logon without TargetCompID, which no session can be resolved
		// for, its Password not cut on a rune boundary.
		{fuzzExtra | fuzzActive | fuzzDicts, "35=A|49=INI|34=1|52=$T|98=0|108=30|553=u|554=abc\xc3\xa9d"},
		// A Logon without MsgSeqNum, logged.
		{fuzzVerbose, "35=A|49=INI|56=ACC|52=$T|98=0|108=30"},
		// A MsgType the dictionaries do not support.
		{fuzzDicts, fuzzL + m("F", "$N", "58=x")},
		// A PossDup SequenceReset without OrigSendingTime, allowed.
		{fuzzStrict, fuzzL + fuzzApp + m("4", "2", "43=Y|123=Y|36=4")},
	} {
		f.Add(s.cfg, swap(s.script), []byte(nil))
	}
}

// FuzzAcceptor feeds an acceptor of one configured session, or of sessions
// it resolves, what a counterparty writes, while the application operates
// on the session.
func FuzzAcceptor(f *testing.F) {
	fuzzSeeds(f, func(s string) string { return s })
	f.Fuzz(func(t *testing.T, cfg uint32, script string, ops []byte) {
		id, begin := fuzzID(cfg, "ACC", "INI")
		sc, obs, cleanup := fuzzConfig(cfg, id)
		defer cleanup()
		opts := session.AcceptorOptions{LogonTimeout: 100 * time.Millisecond}
		if cfg&fuzzVerbose != 0 {
			opts.Logger = sc.Logger
		}
		if cfg&fuzzExtra != 0 {
			opts.Resolve = func(_ context.Context, req *session.LogonRequest) (*session.Config, error) {
				if string(req.Logon.Get(553)) == "bad" {
					return nil, errors.New("refused")
				}
				c := sc
				c.ID = req.ID
				return &c, nil
			}
			opts.Route = func(_ context.Context, _ *session.LogonRequest, candidates []session.SessionID) (session.SessionID, error) {
				return candidates[0], nil
			}
		}
		a := session.NewAcceptor(opts)
		if cfg&fuzzExtra == 0 {
			if _, err := a.Add(sc); err != nil {
				t.Fatal(err)
			}
		}
		l := mem.Listen("acceptor")
		a.Listen(l)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); a.Run(ctx) }()
		defer fuzzStop(t, cancel, done)

		dctx, dcancel := context.WithTimeout(ctx, fuzzTimeout)
		defer dcancel()
		conn, err := l.Dial(dctx)
		if err != nil {
			t.Fatal(err)
		}
		p := newFuzzPeer(conn)
		app := &fuzzOps{
			begin: begin, ops: ops, remove: a.Remove,
			session: func() *session.Session { return a.Session(id) },
			list:    func() { a.Sessions() },
			settle:  func() { p.settle(obs) },
		}
		p.write(fuzzScript(begin, script), app.run)
		p.settle(obs)
		conn.Close()
		<-p.drain
	})
}

// FuzzInitiator answers an initiator's Logon with what a counterparty
// writes, while the application operates on the session.
func FuzzInitiator(f *testing.F) {
	fuzzSeeds(f, strings.NewReplacer("49=INI|56=ACC", "49=ACC|56=INI").Replace)
	f.Fuzz(func(t *testing.T, cfg uint32, script string, ops []byte) {
		id, begin := fuzzID(cfg, "INI", "ACC")
		sc, obs, cleanup := fuzzConfig(cfg, id)
		defer cleanup()
		l := mem.Listen("counterparty")
		opts := session.InitiatorOptions{ReconnectMin: time.Millisecond, ReconnectMax: 5 * time.Millisecond}
		if cfg&fuzzVerbose != 0 {
			opts.Logger = sc.Logger
		}
		i := session.NewInitiator(opts)
		if _, err := i.Add(sc, l); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); i.Run(ctx) }()
		defer fuzzStop(t, cancel, done)

		// A session outside its schedule does not connect.
		wait := fuzzTimeout
		if cfg&fuzzClosed != 0 {
			wait = 20 * time.Millisecond
		}
		actx, acancel := context.WithTimeout(ctx, wait)
		defer acancel()
		conn, err := l.Accept(actx)
		if err != nil {
			if cfg&fuzzClosed == 0 {
				t.Fatal(err)
			}
			return
		}
		p := newFuzzPeer(conn)
		app := &fuzzOps{
			begin: begin, ops: ops, remove: i.Remove,
			session: func() *session.Session { return i.Session(id) },
			list:    func() { i.Sessions() },
			settle:  func() { p.settle(obs) },
		}
		// The counterparty answers the initiator's Logon.
		p.waitRead(time.Second)
		p.write(fuzzScript(begin, script), app.run)
		p.settle(obs)
		conn.Close()
		<-p.drain
	})
}
