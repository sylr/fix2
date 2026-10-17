package session_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
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
	"github.com/sylr/fix2/fix/tests/dicts/fix44"
	"github.com/sylr/fix2/fix/tests/dicts/fix50sp2"
	"github.com/sylr/fix2/fix/tests/dicts/fixlatest"
	"github.com/sylr/fix2/fix/tests/dicts/fixt11"
	"github.com/sylr/fix2/transport/mem"
)

// hdr is the header of a message a test writes, every field of it chosen.
type hdr struct {
	begin, sender, target string
	seq                   int
	sent                  time.Time
}

// from returns the header of the peer's message seq to initiator INI.
func from(begin string, seq int) hdr {
	return hdr{begin: begin, sender: "ACC", target: "INI", seq: seq, sent: time.Now()}
}

// build returns a message of msgType with header h and fields after it.
func (h hdr) build(msgType string, fields ...string) *fix.Builder {
	b := fix.NewBuilder(h.begin, msgType).SetString(49, h.sender).SetString(56, h.target).SetInt(34, h.seq).
		SetString(52, string(fix.AppendUTCTimestamp(nil, h.sent, fix.Milliseconds)))
	for i := 0; i+1 < len(fields); i += 2 {
		tag, _ := strconv.Atoi(fields[i])
		b.Append(tag, []byte(fields[i+1]))
	}
	return b
}

// write writes a message of msgType with header h and fields after it.
func (p *peer) write(h hdr, msgType string, fields ...string) {
	p.t.Helper()
	p.writeRaw(h.build(msgType, fields...).Build())
}

// writeRaw writes frame as it is.
func (p *peer) writeRaw(frame []byte) {
	p.t.Helper()
	if _, err := p.conn.Write(frame); err != nil {
		p.t.Fatal(err)
	}
}

// until reads messages up to one of msgType, and returns them all.
func (p *peer) until(msgType string) []*fix.Message {
	p.t.Helper()
	var got []*fix.Message
	for {
		select {
		case ev := <-p.frames:
			if ev.err != nil {
				p.t.Fatalf("connection ended (%v), expected MsgType %s", ev.err, msgType)
			}
			m, err := fix.Parse(ev.frame)
			if err != nil {
				p.t.Fatalf("invalid message %s: %v", visible(ev.frame), err)
			}
			got = append(got, &m)
			if string(m.MsgType()) == msgType {
				return got
			}
		case <-time.After(5 * time.Second):
			p.t.Fatalf("timed out waiting for MsgType %s", msgType)
		}
	}
}

// logonFields are the body of a valid Logon of begin, with ResetSeqNumFlag if
// reset.
func logonFields(begin string, reset bool) []string {
	f := []string{"98", "0", "108", "30"}
	if reset {
		f = append(f, "141", "Y")
	}
	if begin == session.FIXT11 {
		f = append(f, "1137", "9")
	}
	return f
}

// established logs an initiator of begin on to a scripted peer, then moves
// both sides on: TestRequests 2 to 4, answered by Heartbeats 2 to 4, and an
// order, 5, the peer reads. It returns the order's receipt.
func established(t *testing.T, begin string, change func(*session.Config)) (*session.Session, *recorder, *peer, session.Receipt) {
	t.Helper()
	l := mem.Listen("acceptor")
	s, rec := startInitiator(t, l, func(c *session.Config) {
		c.ID.BeginString = begin
		c.LogoutTimeout = 5 * time.Second
		if begin == session.FIXT11 {
			c.Dictionaries.DefaultApplVerID = "9"
		}
		if change != nil {
			change(c)
		}
	})
	p := acceptPeer(t, l)
	p.expect("A")
	p.write(from(begin, 1), "A", logonFields(begin, false)...)
	rec.wait(t, func() bool { return rec.logons == 1 })
	for seq := 2; seq <= 4; seq++ {
		id := "T" + strconv.Itoa(seq)
		p.write(from(begin, seq), "1", "112", id)
		p.expect("0", "34", strconv.Itoa(seq), "112", id)
	}
	r, err := s.Send(context.Background(), fix.NewBuilder(begin, "D").SetString(11, "O1"))
	if err != nil {
		t.Fatal(err)
	}
	p.expect("D", "34", "5", "11", "O1")
	return s, rec, p, r
}

// strictApps gives a FIXT session one application dictionary: by default it
// refuses a DefaultApplVerID it has no dictionary for.
func strictApps(c *session.Config) {
	c.Dictionaries = session.Dictionaries{Session: fixt11.Dictionary(), DefaultApplVerID: "9",
		Apps: map[string]*dictionary.Dict{"9": fix50sp2.Dictionary()}}
}

// A Logon with ResetSeqNumFlag on an established session is checked as a
// first Logon is, its MsgSeqNum 1 included: an invalid one is rejected and
// logs out, and resets nothing.
func TestActiveResetRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		begin  string
		change func(*session.Config)
		header func(*hdr)
		fields []string // the Logon's body, a valid one with ResetSeqNumFlag if nil
		reason string   // the Reject's SessionRejectReason, "" for no Reject
		refTag string   // its RefTagID, "" for none
	}{
		{name: "wrong SenderCompID", header: func(h *hdr) { h.sender = "WRONG" }, reason: "9"},
		{name: "wrong TargetCompID", header: func(h *hdr) { h.target = "WRONG" }, reason: "9"},
		{name: "SendingTime an hour off", header: func(h *hdr) { h.sent = h.sent.Add(-time.Hour) }, reason: "10"},
		{name: "MsgSeqNum 50", header: func(h *hdr) { h.seq = 50 }, reason: "5", refTag: "34"},
		{name: "MsgSeqNum 3, too low", header: func(h *hdr) { h.seq = 3 }, reason: "5", refTag: "34"},
		{name: "MsgSeqNum 5, the one expected", header: func(h *hdr) { h.seq = 5 }, reason: "5", refTag: "34"},
		{name: "HeartBtInt missing", fields: []string{"98", "0", "141", "Y"}, reason: "1", refTag: "108"},
		{name: "HeartBtInt not a number", fields: []string{"98", "0", "108", "abc", "141", "Y"}, reason: "6", refTag: "108"},
		{name: "HeartBtInt negative", fields: []string{"98", "0", "108", "-30", "141", "Y"}, reason: "5", refTag: "108"},
		{name: "EncryptMethod 1", fields: []string{"98", "1", "108", "30", "141", "Y"}, reason: "5", refTag: "98"},
		{name: "EncryptMethod missing", fields: []string{"108", "30", "141", "Y"}, reason: "1", refTag: "98"},
		{name: "EncryptMethod missing, session dictionary", change: func(c *session.Config) { c.Dictionaries.Session = fix44.Dictionary() },
			fields: []string{"108", "30", "141", "Y"}, reason: "1", refTag: "98"},
		{name: "wrong BeginString", header: func(h *hdr) { h.begin = session.FIX42 }},
		{name: "FIXT DefaultApplVerID missing", begin: session.FIXT11, fields: []string{"98", "0", "108", "30", "141", "Y"}, reason: "1", refTag: "1137"},
		{name: "FIXT DefaultApplVerID unsupported", begin: session.FIXT11, change: strictApps,
			fields: []string{"98", "0", "108", "30", "141", "Y", "1137", "7"}, reason: "5", refTag: "1137"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			begin := cmp.Or(tc.begin, session.FIX44)
			s, _, p, r1 := established(t, begin, tc.change)
			// An order sent as the Logon arrives is not dropped, whether
			// queued, pending or written by then.
			r2, err := s.Send(context.Background(), fix.NewBuilder(begin, "D").SetString(11, "O2"))
			if err != nil {
				t.Fatal(err)
			}
			h := from(begin, 1)
			if tc.header != nil {
				tc.header(&h)
			}
			fields := tc.fields
			if fields == nil {
				fields = logonFields(begin, true)
			}
			p.write(h, "A", fields...)

			var reject *fix.Message
			sent := false
			for _, m := range p.until("5") {
				switch string(m.MsgType()) {
				case "3":
					reject = m
				case "D":
					sent = string(m.Get(11)) == "O2"
				case "5":
				default:
					t.Errorf("unexpected MsgType %s", m.MsgType())
				}
			}
			if !sent {
				t.Error("order O2 not sent before the Logout")
			}
			wantSender := uint64(9) // O2 6, the Reject 7, the Logout 8
			switch {
			case tc.reason == "":
				wantSender = 8
				if reject != nil {
					t.Errorf("Reject %s, want none", reject.Get(58))
				}
			case reject == nil:
				t.Error("no Reject")
			default:
				for _, f := range []struct {
					tag  int
					want string
				}{{45, strconv.Itoa(h.seq)}, {373, tc.reason}, {371, tc.refTag}} {
					if got := string(reject.Get(f.tag)); got != f.want {
						t.Errorf("Reject tag %d = %q, want %q (Text %q)", f.tag, got, f.want, reject.Get(58))
					}
				}
			}
			// Counted as received only if it is the one expected, as a
			// message with a wrong header is.
			wantTarget := uint64(5)
			if h.seq == 5 && tc.reason != "" {
				wantTarget = 6
			}
			if got := s.NextSenderSeqNum(); got != wantSender {
				t.Errorf("NextSenderSeqNum = %d, want %d", got, wantSender)
			}
			if got := s.NextTargetSeqNum(); got != wantTarget {
				t.Errorf("NextTargetSeqNum = %d, want %d", got, wantTarget)
			}
			for _, r := range []session.Receipt{r1, r2} {
				if err := r.Err(); err != nil {
					t.Errorf("receipt %d: %v", r.Seq(), err)
				}
			}
			// The store was not reset: the orders are resent on request.
			p.write(from(begin, int(wantTarget)), "2", "7", "5", "16", strconv.FormatUint(r2.Seq(), 10))
			p.expect("D", "34", "5", "11", "O1", "43", "Y")
			p.expect("D", "34", strconv.FormatUint(r2.Seq(), 10), "11", "O2", "43", "Y")
			p.write(from(begin, int(wantTarget)+1), "5")
			p.expectEnd(5 * time.Second)
		})
	}
}

// FIX requires EncryptMethod (98) of every Logon: an acceptor refuses a
// first Logon without it, the connection closed without a word; an
// initiator accepts a Logon answer without it, as venues whose dictionary
// makes it optional send.
func TestLogonWithoutEncryptMethod(t *testing.T) {
	t.Run("acceptor", func(t *testing.T) {
		l := mem.Listen("acceptor")
		a := session.NewAcceptor(session.AcceptorOptions{})
		rec := newRecorder()
		if _, err := a.Add(session.Config{ID: session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"}, Handler: rec}); err != nil {
			t.Fatal(err)
		}
		a.Listen(l)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); a.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
		p := dialPeer(t, l)
		p.write(hdr{begin: session.FIX44, sender: "INI", target: "ACC", seq: 1, sent: time.Now()}, "A", "108", "30")
		p.expectSilentEnd()
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if rec.logons != 0 {
			t.Error("logged on")
		}
	})
	t.Run("initiator, TolerateLogonWithoutEncryptMethod", func(t *testing.T) {
		l := mem.Listen("acceptor")
		_, rec := startInitiator(t, l, func(c *session.Config) { c.TolerateLogonWithoutEncryptMethod = true })
		p := acceptPeer(t, l)
		p.expect("A")
		p.write(from(session.FIX44, 1), "A", "108", "30")
		rec.wait(t, func() bool { return rec.logons == 1 })
		p.write(from(session.FIX44, 2), "1", "112", "PING")
		p.expect("0", "112", "PING")
	})
	// The 2020 Session Layer requires it of the answer too.
	t.Run("initiator", func(t *testing.T) {
		l := mem.Listen("acceptor")
		_, rec := startInitiator(t, l, nil)
		p := acceptPeer(t, l)
		p.expect("A")
		p.write(from(session.FIX44, 1), "A", "108", "30")
		p.expectSilentEnd()
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if rec.logons != 0 {
			t.Error("logged on")
		}
	})
}

// A valid Logon with ResetSeqNumFlag on an established session resets both
// sides to 1.
func TestActiveResetAccepted(t *testing.T) {
	for _, tc := range []struct {
		name, begin string
		change      func(*session.Config)
	}{
		{"FIX.4.2", session.FIX42, nil},
		{"FIX.4.4", session.FIX44, nil},
		{"FIXT.1.1", session.FIXT11, nil},
		{"FIXT.1.1 strict", session.FIXT11, strictApps},
	} {
		begin := tc.begin
		t.Run(tc.name, func(t *testing.T) {
			s, _, p, r := established(t, begin, tc.change)
			p.write(from(begin, 1), "A", logonFields(begin, true)...)
			p.expect("A", "34", "1", "141", "Y")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := r.Wait(ctx, session.Acknowledged); !errors.Is(err, session.ErrUnknownDelivery) {
				t.Errorf("Wait = %v, want ErrUnknownDelivery", err)
			}
			if got := s.NextSenderSeqNum(); got != 2 {
				t.Errorf("NextSenderSeqNum = %d, want 2", got)
			}
			if got := s.NextTargetSeqNum(); got != 2 {
				t.Errorf("NextTargetSeqNum = %d, want 2", got)
			}
			p.write(from(begin, 2), "1", "112", "AFTER")
			p.expect("0", "34", "2", "112", "AFTER")
		})
	}
}

// A session logging out sends nothing but what a ResendRequest asks for:
// a Logon with ResetSeqNumFlag is neither answered nor resets anything.
func TestActiveResetWhileLoggingOut(t *testing.T) {
	s, _, p, r := established(t, session.FIX44, nil)
	done := make(chan error, 1)
	go func() { done <- s.Logout(context.Background(), "bye") }()
	p.expect("5", "34", "6")
	p.write(from(session.FIX44, 1), "A", logonFields(session.FIX44, true)...)
	p.write(from(session.FIX44, 5), "5")
	p.expectSilentEnd()
	if err := <-done; err != nil {
		t.Errorf("Logout = %v", err)
	}
	if got := s.NextSenderSeqNum(); got != 7 {
		t.Errorf("NextSenderSeqNum = %d, want 7", got)
	}
	if got := s.NextTargetSeqNum(); got != 6 {
		t.Errorf("NextTargetSeqNum = %d, want 6", got)
	}
	if err := r.Err(); err != nil {
		t.Errorf("receipt: %v", err)
	}
}

// A refused reset leaves the recovery of a gap as it was: the message kept
// ahead of it is processed once the gap is filled, while logging out.
func TestActiveResetRefusedKeepsRecovery(t *testing.T) {
	s, rec, p, _ := established(t, session.FIX44, nil)
	p.write(from(session.FIX44, 6), "D", "11", "S6")
	p.expect("2", "7", "5", "16", "0")
	h := from(session.FIX44, 1)
	h.sender = "WRONG"
	p.write(h, "A", logonFields(session.FIX44, true)...)
	p.expect("3", "45", "1", "373", "9")
	p.expect("5")
	p.write(from(session.FIX44, 5), "D", "11", "S5")
	rec.wait(t, func() bool { return len(rec.apps) == 2 })
	rec.mu.Lock()
	if rec.apps[0] != "S5" || rec.apps[1] != "S6" {
		t.Errorf("received %q, want S5 then S6", rec.apps)
	}
	rec.mu.Unlock()
	if got := s.NextTargetSeqNum(); got != 7 {
		t.Errorf("NextTargetSeqNum = %d, want 7", got)
	}
	p.write(from(session.FIX44, 7), "5")
	p.expectEnd(5 * time.Second)
}

// authCounter counts the Logons it authenticates.
type authCounter struct {
	*recorder
	n atomic.Int32
}

func (a *authCounter) Authenticate(context.Context, *session.LogonRequest) error {
	a.n.Add(1)
	return nil
}

// The handler authenticates a Logon with ResetSeqNumFlag the session
// accepts, not one it refuses anyway.
func TestActiveResetAuthenticatedOnlyIfValid(t *testing.T) {
	var auth *authCounter
	s, _, p, _ := established(t, session.FIX44, func(c *session.Config) {
		auth = &authCounter{recorder: c.Handler.(*recorder)}
		c.Handler = auth
	})
	before := auth.n.Load()
	p.write(from(session.FIX44, 1), "A", logonFields(session.FIX44, true)...)
	p.expect("A", "34", "1", "141", "Y")
	if got := auth.n.Load(); got != before+1 {
		t.Errorf("valid reset: %d authentications, want %d", got, before+1)
	}
	h := from(session.FIX44, 1)
	h.sender = "WRONG"
	p.write(h, "A", logonFields(session.FIX44, true)...)
	p.until("5")
	if got := auth.n.Load(); got != before+1 {
		t.Errorf("invalid reset: %d authentications, want %d", got, before+1)
	}
	p.write(from(session.FIX44, int(s.NextTargetSeqNum())), "5")
	p.expectEnd(5 * time.Second)
}

// An acceptor checks the HeartBtInt of a Logon with ResetSeqNumFlag against
// its bounds, as at logon.
func TestActiveResetHeartBtIntBounds(t *testing.T) {
	for _, hb := range []string{"5", "600"} {
		t.Run(hb, func(t *testing.T) {
			l := mem.Listen("acceptor")
			a := session.NewAcceptor(session.AcceptorOptions{})
			rec := newRecorder()
			s, err := a.Add(session.Config{
				ID:            session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"},
				Handler:       rec,
				MinHeartBtInt: 10 * time.Second,
				MaxHeartBtInt: time.Minute,
				LogoutTimeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			a.Listen(l)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); a.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })

			p := dialPeer(t, l)
			toAcceptor := func(seq int) hdr {
				return hdr{begin: session.FIX44, sender: "INI", target: "ACC", seq: seq, sent: time.Now()}
			}
			p.write(toAcceptor(1), "A", "98", "0", "108", "30")
			p.expect("A")
			rec.wait(t, func() bool { return rec.logons == 1 })
			p.write(toAcceptor(1), "A", "98", "0", "108", hb, "141", "Y")
			p.expect("3", "45", "1", "373", "5", "371", "108")
			p.expect("5")
			if got := s.NextSenderSeqNum(); got != 4 {
				t.Errorf("NextSenderSeqNum = %d, want 4", got)
			}
			if got := s.NextTargetSeqNum(); got != 2 {
				t.Errorf("NextTargetSeqNum = %d, want 2", got)
			}
			p.write(toAcceptor(2), "5")
			p.expectEnd(5 * time.Second)
		})
	}
}

// fixtApps are the dictionaries of a FIXT initiator: FIX 5.0 SP2 (9) by
// default, and FIX Latest (10), which alone defines EncryptedPasswordLen and
// EncryptedPassword (1401/1402), a LENGTH/DATA pair. Validation is off: the
// tests are about parsing.
func fixtApps(c *session.Config) {
	c.ID.BeginString = session.FIXT11
	c.Dictionaries = session.Dictionaries{Session: fixt11.Dictionary(), DefaultApplVerID: "9",
		Apps: map[string]*dictionary.Dict{"9": fix50sp2.Dictionary(), "10": fixlatest.Dictionary()}}
	c.TolerateValidationFailures = dictionary.CheckAll
}

// dataRecorder records the EncryptedPassword (1402) and ApplVerID of the
// application messages it receives.
type dataRecorder struct {
	*recorder
	data, versions []string
}

func (r *dataRecorder) FromApp(ctx context.Context, s *session.Session, m *fix.Message, in session.Inbound) error {
	r.mu.Lock()
	r.data = append(r.data, string(m.Get(1402)))
	r.versions = append(r.versions, in.ApplVerID)
	r.mu.Unlock()
	return r.recorder.FromApp(ctx, s, m, in)
}

// fixtSession logs a FIXT initiator with fixtApps on to a scripted peer
// whose DefaultApplVerID is peerDefault.
func fixtSession(t *testing.T, peerDefault string, change func(*session.Config)) (*session.Session, *dataRecorder, *peer) {
	t.Helper()
	l := mem.Listen("acceptor")
	var rec *dataRecorder
	s, _ := startInitiator(t, l, func(c *session.Config) {
		fixtApps(c)
		rec = &dataRecorder{recorder: c.Handler.(*recorder)}
		c.Handler = rec
		if change != nil {
			change(c)
		}
	})
	p := acceptPeer(t, l)
	p.expect("A")
	p.write(from(session.FIXT11, 1), "A", "98", "0", "108", "30", "1137", peerDefault)
	rec.wait(t, func() bool { return rec.logons == 1 })
	return s, rec, p
}

// userRequest returns UserRequest (BE) seq with EncryptedPassword payload,
// ApplVerID applVerID unless empty, and SecureData secure in its header
// unless empty.
func userRequest(seq int, applVerID, secure, payload string) []byte {
	b := from(session.FIXT11, seq).build("BE")
	if secure != "" {
		b.SetData(90, 91, []byte(secure))
	}
	if applVerID != "" {
		b.SetString(1128, applVerID)
	}
	return b.SetString(923, "R1").SetString(924, "1").SetString(553, "user").SetData(1401, 1402, []byte(payload)).Build()
}

// received returns what rec received, once it has n application messages.
func (r *dataRecorder) received(t *testing.T, n int) (data, versions []string) {
	t.Helper()
	r.wait(t, func() bool { return len(r.data) == n })
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.data, r.versions
}

// A FIXT message is parsed with the profile of its application version,
// chosen from its header before its body is parsed: a DATA field only that
// version defines arrives whole, whatever SOH and fields it holds.
func TestAppProfileChosenBeforeParsing(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		peerDefault          string
		applVerID, secure    string
		payload, wantVersion string
	}{
		{name: "x<SOH>y", peerDefault: "9", applVerID: "10", payload: "x\x01y"},
		{name: "x<SOH><SOH>y", peerDefault: "9", applVerID: "10", payload: "x\x01\x01y"},
		{name: "x<SOH>=y", peerDefault: "9", applVerID: "10", payload: "x\x01=y"},
		{name: "x<SOH>58=z", peerDefault: "9", applVerID: "10", payload: "x\x0158=z"},
		{name: "ApplVerID in the payload", peerDefault: "9", applVerID: "10", payload: "x\x011128=9"},
		{name: "ApplVerID in the header's SecureData", peerDefault: "9", applVerID: "10", secure: "s\x011128=9\x01", payload: "x\x01y"},
		{name: "peer's default, x<SOH>y", peerDefault: "10", payload: "x\x01y"},
		{name: "peer's default, ApplVerID in the payload", peerDefault: "10", payload: "x\x011128=9"},
		{name: "peer's default, ApplVerID in the payload, then a field", peerDefault: "10", payload: "x\x011128=9\x01y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rec, p := fixtSession(t, tc.peerDefault, nil)
			p.writeRaw(userRequest(2, tc.applVerID, tc.secure, tc.payload))
			data, versions := rec.received(t, 1)
			if data[0] != tc.payload {
				t.Errorf("EncryptedPassword %q, want %q", data[0], tc.payload)
			}
			if versions[0] != "10" {
				t.Errorf("ApplVerID %q, want 10", versions[0])
			}
			if got := s.NextTargetSeqNum(); got != 3 {
				t.Errorf("NextTargetSeqNum = %d, want 3", got)
			}
		})
	}
}

// A message kept ahead of a gap is parsed with the profile of its
// application version when its turn comes.
func TestAppProfileOfStashedMessage(t *testing.T) {
	_, rec, p := fixtSession(t, "10", nil)
	const payload = "x\x011128=9"
	p.writeRaw(userRequest(3, "", "", payload))
	p.expect("2", "7", "2")
	p.write(from(session.FIXT11, 2), "0")
	data, versions := rec.received(t, 1)
	if data[0] != payload || versions[0] != "10" {
		t.Errorf("EncryptedPassword %q of ApplVerID %q, want %q of 10", data[0], versions[0], payload)
	}
}

// observed records the frames an observer sees.
type observed struct {
	mu      sync.Mutex
	in, out [][]byte
}

func (o *observed) OnInbound(_ *session.Session, f session.Frame) {
	o.mu.Lock()
	o.in = append(o.in, bytes.Clone(f.Raw))
	o.mu.Unlock()
}

func (o *observed) OnOutbound(_ *session.Session, f session.Frame) {
	o.mu.Lock()
	o.out = append(o.out, bytes.Clone(f.Raw))
	o.mu.Unlock()
}

// find waits for a frame the observer saw, inbound or not, holding part.
func (o *observed) find(t *testing.T, inbound bool, part string) []byte {
	t.Helper()
	var found []byte
	waitFor(t, func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		frames := o.out
		if inbound {
			frames = o.in
		}
		for _, f := range frames {
			if bytes.Contains(f, []byte(part)) {
				found = f
				return true
			}
		}
		return false
	})
	return found
}

// Redaction parses a frame as the session does: an ApplVerID within a DATA
// field does not choose its profile.
func TestRedactAppProfile(t *testing.T) {
	seen := &observed{}
	obs := session.NewAsyncObserver(seen, 16)
	t.Cleanup(obs.Close)
	_, rec, p := fixtSession(t, "10", func(c *session.Config) { c.Observer = obs })
	const payload = "s3cr\x011128=9"
	p.writeRaw(userRequest(2, "", "", payload))
	rec.received(t, 1)
	redacted := seen.find(t, true, "\x0135=BE\x01")
	if !bytes.Contains(redacted, []byte("\x011401=3\x011402=***\x01")) || bytes.Contains(redacted, []byte("s3cr")) || bytes.Contains(redacted, []byte("1128=9")) {
		t.Errorf("redacted %s", visible(redacted))
	}
}

// expectRaw reads the next message, unparsed: the default profile may not
// read it.
func (p *peer) expectRaw() []byte {
	p.t.Helper()
	select {
	case ev := <-p.frames:
		if ev.err != nil {
			p.t.Fatalf("connection ended: %v", ev.err)
		}
		return ev.frame
	case <-time.After(5 * time.Second):
		p.t.Fatal("timed out waiting for a message")
	}
	return nil
}

// Our own messages are resent and redacted with the profile of their
// application version: the ApplVerID of their header or, without one, our
// default version, whatever the counterparty's.
func TestOwnAppProfile(t *testing.T) {
	seen := &observed{}
	obs := session.NewAsyncObserver(seen, 16)
	t.Cleanup(obs.Close)
	s, _, p := fixtSession(t, "10", func(c *session.Config) { c.Observer = obs })
	ctx := context.Background()
	// FIX Latest, with SOH in EncryptedPassword.
	if _, err := s.SendRaw(ctx, "BE", []byte("1128=10\x01923=R1\x01924=1\x01553=user\x011401=3\x011402=x\x01y\x01")); err != nil {
		t.Fatal(err)
	}
	// FIX 5.0 SP2, where 1401 announces no DATA field, unlike in FIX Latest.
	if _, err := s.SendRaw(ctx, "BE", []byte("923=R2\x01924=1\x01553=user\x011401=3\x0158=abc\x01")); err != nil {
		t.Fatal(err)
	}
	p.expect("BE", "34", "2", "1402", "x\x01y")
	if raw := p.expectRaw(); !bytes.Contains(raw, []byte("\x0134=3\x01")) {
		t.Fatalf("got %s, want message 3", visible(raw))
	}
	p.write(from(session.FIXT11, 2), "2", "7", "2", "16", "3")
	p.expect("BE", "34", "2", "43", "Y", "1402", "x\x01y")
	if raw := p.expectRaw(); !bytes.Contains(raw, []byte("\x0135=BE\x0134=3\x01")) || !bytes.Contains(raw, []byte("\x0143=Y\x01")) {
		t.Errorf("got %s, want message 3 resent", visible(raw))
	}
	if r := seen.find(t, false, "\x01923=R1\x01"); !bytes.Contains(r, []byte("\x011401=3\x011402=***\x01")) {
		t.Errorf("redacted %s", visible(r))
	}
	if r := seen.find(t, false, "\x0135=BE\x0134=3\x01"); !bytes.Contains(r, []byte("\x01923=R2\x01")) {
		t.Errorf("redacted %s", visible(r))
	}
}

// The application version a message is validated and delivered with is the
// one it was parsed with: an ApplVerID after the header does not choose it.
func TestAppVersionChosenOnce(t *testing.T) {
	// A version 9 with a UserRequest whose 1401 is no LENGTH field.
	app, err := xmldict.Load(strings.NewReader(`<fix type="FIX" major="5" minor="0" servicepack="2">
  <header/><trailer/>
  <messages><message name="UserRequest" msgtype="BE" msgcat="app"><field name="UserRequestID" required="Y"/><field name="UserRequestType" required="Y"/><field name="Username" required="Y"/>
    <field name="EncryptedPasswordMethod" required="N"/><field name="Password" required="N"/></message></messages>
  <components/>
  <fields>
    <field number="923" name="UserRequestID" type="STRING"/><field number="1128" name="ApplVerID" type="STRING"/>
    <field number="1401" name="EncryptedPasswordMethod" type="INT"/><field number="554" name="Password" type="STRING"/>
    <field number="924" name="UserRequestType" type="INT"/><field number="553" name="Username" type="STRING"/>
  </fields>
</fix>`))
	if err != nil {
		t.Fatal(err)
	}
	s, rec, p := fixtSession(t, "9", func(c *session.Config) { c.Dictionaries.Apps["9"] = app })
	// As FIX Latest, 1401 would announce 1402, not 554: this parses only as
	// the counterparty's default, 9.
	p.writeRaw(from(session.FIXT11, 2).build("BE", "923", "R1", "1128", "10", "924", "1", "553", "user", "1401", "2", "554", "pw").Build())
	_, versions := rec.received(t, 1)
	if versions[0] != "9" {
		t.Errorf("ApplVerID %q, want 9", versions[0])
	}
	if got := s.NextTargetSeqNum(); got != 3 {
		t.Errorf("NextTargetSeqNum = %d, want 3", got)
	}
}

// A message whose header does not parse is garbled: ignored, its MsgSeqNum
// not consumed.
func TestMalformedHeaderGarbled(t *testing.T) {
	s, _, p := fixtSession(t, "9", nil)
	// SecureData shorter than SecureDataLen says.
	p.writeRaw(from(session.FIXT11, 2).build("BE", "90", "50", "91", "abc", "1128", "10", "923", "R1").Build())
	p.write(from(session.FIXT11, 2), "1", "112", "PING")
	p.expect("0", "112", "PING")
	if got := s.Stats().Garbled.Load(); got != 1 {
		t.Errorf("Garbled = %d, want 1", got)
	}
	if got := s.NextTargetSeqNum(); got != 3 {
		t.Errorf("NextTargetSeqNum = %d, want 3", got)
	}
}

// An application dictionary may not read a standard header field otherwise
// than the transport dictionary does: the header is read before the
// application version is known.
func TestHeaderFieldRetypedRefused(t *testing.T) {
	app, err := xmldict.Load(strings.NewReader(`<fix type="FIX" major="5" minor="0" servicepack="2">
  <header/><trailer/>
  <messages><message name="News" msgtype="B" msgcat="app"><field name="Headline" required="Y"/></message></messages>
  <components/>
  <fields>
    <field number="148" name="Headline" type="STRING"/><field number="212" name="XmlDataLen" type="INT"/>
  </fields>
</fix>`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.New(session.Config{
		ID:      session.SessionID{BeginString: session.FIXT11, SenderCompID: "INI", TargetCompID: "ACC"},
		Handler: session.BaseHandler{},
		Dictionaries: session.Dictionaries{Session: fixt11.Dictionary(), DefaultApplVerID: "9",
			Apps: map[string]*dictionary.Dict{"9": fix50sp2.Dictionary(), "X": app}},
	})
	if err == nil || !strings.Contains(err.Error(), "212") {
		t.Errorf("New = %v, want an error about header field 212", err)
	}
}

// Without dictionaries, an ApplVerID (1128) after the standard header does
// not choose the application version: the counterparty's default does. One
// in the header does.
func TestApplVerIDWithoutDictionaries(t *testing.T) {
	_, rec, p := fixtSession(t, "9", func(c *session.Config) { c.Dictionaries = session.Dictionaries{DefaultApplVerID: "9"} })
	p.write(from(session.FIXT11, 2), "D", "11", "ORDER", "1128", "10")
	p.write(from(session.FIXT11, 3), "D", "1128", "10", "11", "ORDER")
	_, versions := rec.received(t, 2)
	if versions[0] != "9" || versions[1] != "10" {
		t.Errorf("ApplVerIDs %q, want 9 for the body's, 10 for the header's", versions)
	}
}

// A message whose ApplVerID has no dictionary is rejected, though our
// default's profile, which such a version would be parsed with, does not
// parse it: it is not garbled.
func TestUnknownApplVerIDNotGarbled(t *testing.T) {
	s, rec, p := fixtSession(t, "10", nil)
	// 1401 announces 1402 in the counterparty's default, 10, not in ours, 9.
	p.writeRaw(userRequest(2, "99", "", "x\x01y"))
	p.expect("3", "45", "2", "373", "18", "371", "1128")
	if got := s.Stats().Garbled.Load(); got != 0 {
		t.Errorf("Garbled = %d, want 0", got)
	}
	if got := s.NextTargetSeqNum(); got != 3 {
		t.Errorf("NextTargetSeqNum = %d, want 3", got)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.data) != 0 {
		t.Errorf("delivered %q", rec.data)
	}
}

// A message whose ApplVerID has no dictionary is rejected as before.
func TestUnknownApplVerID(t *testing.T) {
	s, rec, p := fixtSession(t, "9", nil)
	p.writeRaw(userRequest(2, "99", "", "xy"))
	p.expect("3", "45", "2", "373", "18", "371", "1128")
	p.write(from(session.FIXT11, 3), "1", "112", "PING")
	p.expect("0", "112", "PING")
	if got := s.NextTargetSeqNum(); got != 4 {
		t.Errorf("NextTargetSeqNum = %d, want 4", got)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.data) != 0 {
		t.Errorf("delivered %q", rec.data)
	}
}

// A group entry out of its group's order is rejected, reason 15, unless
// the session skips dictionary.CheckGroupOrder: then it is delivered.
func TestGroupEntryOutOfOrder(t *testing.T) {
	// NoAllocs lists AllocAccount (79), AllocQty (80), then IndividualAllocID (467).
	order := []string{"11", "O1", "54", "1", "60", "20240101-00:00:00.000", "40", "2", "44", "10", "55", "EUR/USD",
		"78", "1", "79", "A", "467", "I1", "80", "10"}
	t.Run("checked", func(t *testing.T) {
		s, rec, p := loggedOn(t, func(c *session.Config) { c.Dictionaries = session.Dictionaries{Session: fix44.Dictionary()} })
		p.send("D", 0, order...)
		p.expect("3", "45", "2", "373", "15", "371", "80")
		if got := s.NextTargetSeqNum(); got != 3 {
			t.Errorf("NextTargetSeqNum = %d, want 3", got)
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if len(rec.apps) != 0 {
			t.Errorf("delivered %q", rec.apps)
		}
	})
	t.Run("skipped", func(t *testing.T) {
		_, rec, p := loggedOn(t, func(c *session.Config) {
			c.Dictionaries = session.Dictionaries{Session: fix44.Dictionary()}
			c.TolerateValidationFailures = dictionary.CheckGroupOrder
		})
		p.send("D", 0, order...)
		rec.wait(t, func() bool { return len(rec.apps) == 1 })
	})
}
