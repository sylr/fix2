package session_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store"
	"github.com/sylr/fix2/fix/session/store/memstore"
	"github.com/sylr/fix2/fix/tests/dicts/fix44"
	"github.com/sylr/fix2/transport/mem"
)

// gcm is an Encrypter sealing with AES-GCM, its nonce before the ciphertext.
type gcm struct {
	method string
	aead   cipher.AEAD
}

// newGCM returns a gcm declaring EncryptMethod method, its key made of key.
func newGCM(t testing.TB, method, key string) *gcm {
	t.Helper()
	block, err := aes.NewCipher([]byte(strings.Repeat(key, 32)[:32]))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return &gcm{method: method, aead: aead}
}

func (g *gcm) Method() string { return g.method }

func (g *gcm) Seal(dst, plain []byte) ([]byte, error) {
	nonce := make([]byte, g.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return dst, err
	}
	dst = append(dst, nonce...)
	return g.aead.Seal(dst, nonce, plain, nil), nil
}

func (g *gcm) Open(dst, sealed []byte) ([]byte, error) {
	n := g.aead.NonceSize()
	if len(sealed) < n {
		return dst, errors.New("sealed data shorter than a nonce")
	}
	return g.aead.Open(dst, sealed[:n], sealed[n:], nil)
}

// An Encrypter is for FIX 4.x, and its Method an EncryptMethod other than 0.
func TestEncrypter_Validate(t *testing.T) {
	for _, c := range []struct {
		begin, method string
		ok            bool
	}{
		{session.FIX44, "7", true},
		{session.FIX40, "1", true},
		{session.FIXT11, "7", false},
		{session.FIX44, "0", false},
		{session.FIX44, "", false},
		{session.FIX44, "x", false},
		{session.FIX44, "-1", false},
		{session.FIX44, "07", false},
		{session.FIX44, "+7", false},
		{"dictionary", "7", false}, // FIX44 with a dictionary listing 0 only
	} {
		cfg := session.Config{
			ID:        session.SessionID{BeginString: c.begin, SenderCompID: "INI", TargetCompID: "ACC"},
			Handler:   newRecorder(),
			Encrypter: newGCM(t, c.method, "k"),
		}
		if c.begin == "dictionary" {
			cfg.ID.BeginString, cfg.Dictionaries.Session = session.FIX44, fix44.Dictionary()
		}
		if c.begin == session.FIXT11 {
			cfg.Dictionaries.DefaultApplVerID = "9"
		}
		err := cfg.Validate()
		if (err == nil) != c.ok {
			t.Errorf("%s, Method %q: Validate() = %v, want ok %v", c.begin, c.method, err, c.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "Encrypter") {
			t.Errorf("%s, Method %q: %v does not name Encrypter", c.begin, c.method, err)
		}
	}
}

// adminSetter sets fields on the Logons it is asked for.
type adminSetter struct {
	*recorder
	set func(*fix.Builder)
}

func (a adminSetter) ToAdmin(_ context.Context, _ *session.Session, msgType string, b *fix.Builder) error {
	if msgType == "A" {
		a.set(b)
	}
	return nil
}

// With an Encrypter, SecureDataLen (90) and SecureData (91) are the
// session's to write: neither Send, SendRaw nor ToAdmin sets them, and
// SendRaw refuses a body it could not seal. Without, Send sets them as
// before.
func TestEncrypter_SecureDataReserved(t *testing.T) {
	builders := func() []*fix.Builder {
		return []*fix.Builder{
			fix.NewBuilder(session.FIX44, "D").SetString(11, "O2").SetData(90, 91, []byte("x")),
			fix.NewBuilder(session.FIX44, "D").SetString(11, "O2").SetString(91, "x"),
		}
	}
	t.Run("Send", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := newGCM(t, "7", "k")
			s, _, p, _, _ := encryptedInitiator(t, g, nil)
			for _, b := range builders() {
				if _, err := s.Send(context.Background(), b); err == nil {
					t.Error("Send with SecureData succeeded")
				}
			}
			// A SOH in a value that is not DATA: the body does not parse.
			if _, err := s.Send(context.Background(), fix.NewBuilder(session.FIX44, "D").SetString(11, "O2").SetString(58, "a\x01b")); err == nil {
				t.Error("Send of a body that does not parse succeeded")
			}
			for _, body := range []string{"90=1\x0191=x\x0111=O2\x01", "11=O2\x0191=x\x01", "11=O2"} {
				if _, err := s.SendRaw(context.Background(), "D", []byte(body)); err == nil {
					t.Errorf("SendRaw of %q succeeded", body)
				}
			}
			if _, err := s.SendRaw(context.Background(), "D", []byte("11=O3\x01")); err != nil {
				t.Fatal(err)
			}
			if m, _ := p.expectSealed(g, "D", "11", "O3"); string(m.Get(34)) != "2" {
				t.Errorf("sent at %s, want 2: a message refused took a MsgSeqNum", m.Get(34))
			}
		})
	})
	t.Run("Send, no Encrypter", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _, p, _ := established(t, session.FIX44, func(c *session.Config) { c.LogoutTimeout = 200 * time.Millisecond })
			if _, err := s.Send(context.Background(), builders()[0]); err != nil {
				t.Fatal(err)
			}
			p.expect("D", "11", "O2", "91", "x")
		})
	})
	t.Run("ToAdmin", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := mem.Listen("acceptor")
			startInitiator(t, l, func(c *session.Config) {
				c.Encrypter = newGCM(t, "7", "k")
				c.Handler = adminSetter{newRecorder(), func(b *fix.Builder) { b.SetData(90, 91, []byte("x")) }}
			})
			p := acceptPeer(t, l)
			p.expectSilentEnd()
		})
	})
}

// Each side's Logon declares the Encrypter's Method, which the other's must
// declare too: an acceptor refuses another, or none, without a Logout; an
// initiator disconnects on such an answer.
func TestEncrypter_Negotiation(t *testing.T) {
	acceptor := func(t *testing.T) (*recorder, *mem.Listener) {
		l := mem.Listen("acceptor")
		a := session.NewAcceptor(session.AcceptorOptions{})
		rec := newRecorder()
		if _, err := a.Add(session.Config{ID: session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"},
			Handler: rec, Encrypter: newGCM(t, "7", "k"), LogoutTimeout: 200 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		a.Listen(l)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); a.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
		return rec, l
	}
	logon := func() hdr {
		return hdr{begin: session.FIX44, sender: "INI", target: "ACC", seq: 1, sent: time.Now()}
	}
	for _, c := range []struct {
		name   string
		fields []string
	}{
		{"none", []string{"98", "0", "108", "30"}},
		{"another", []string{"98", "6", "108", "30"}},
		{"missing", []string{"108", "30"}},
	} {
		t.Run("acceptor refuses "+c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec, l := acceptor(t)
				p := dialPeer(t, l)
				p.write(logon(), "A", c.fields...)
				p.expectSilentEnd()
				rec.mu.Lock()
				defer rec.mu.Unlock()
				if rec.logons != 0 {
					t.Error("logged on")
				}
			})
		})
	}
	t.Run("acceptor accepts its own", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rec, l := acceptor(t)
			p := dialPeer(t, l)
			p.write(logon(), "A", "98", "7", "108", "30")
			p.expect("A", "98", "7")
			rec.wait(t, func() bool { return rec.logons == 1 })
		})
	})
	for _, c := range []struct {
		name     string
		fields   []string
		tolerate bool
	}{
		{"none", []string{"98", "0", "108", "30"}, false},
		{"another", []string{"98", "6", "108", "30"}, false},
		{"missing, TolerateLogonWithoutEncryptMethod", []string{"108", "30"}, true},
	} {
		t.Run("initiator refuses "+c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := mem.Listen("acceptor")
				_, rec := startInitiator(t, l, func(cfg *session.Config) {
					cfg.Encrypter = newGCM(t, "7", "k")
					cfg.TolerateLogonWithoutEncryptMethod = c.tolerate
				})
				p := acceptPeer(t, l)
				p.expect("A", "98", "7")
				p.write(from(session.FIX44, 1), "A", c.fields...)
				p.expectSilentEnd()
				rec.mu.Lock()
				defer rec.mu.Unlock()
				if rec.logons != 0 {
					t.Error("logged on")
				}
			})
		})
	}
	t.Run("initiator accepts its own", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := mem.Listen("acceptor")
			_, rec := startInitiator(t, l, func(cfg *session.Config) { cfg.Encrypter = newGCM(t, "7", "k") })
			p := acceptPeer(t, l)
			p.expect("A", "98", "7")
			p.write(from(session.FIX44, 1), "A", "98", "7", "108", "30")
			rec.wait(t, func() bool { return rec.logons == 1 })
		})
	})
}

// appendLog is a store recording what the session appends to it.
type appendLog struct {
	store.Store
	mu   sync.Mutex
	raws [][]byte
}

func (a *appendLog) Append(ctx context.Context, gen, seq uint64, raw []byte) error {
	a.mu.Lock()
	a.raws = append(a.raws, bytes.Clone(raw))
	a.mu.Unlock()
	return a.Store.Append(ctx, gen, seq, raw)
}

// last returns the last message appended.
func (a *appendLog) last() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.raws[len(a.raws)-1]
}

// unseal returns frame, as g sealed it, parsed, and the fields its SecureData
// (91) holds; it fails the test if it has none or it does not open.
func unseal(t *testing.T, g *gcm, frame []byte) (*fix.Message, string) {
	t.Helper()
	m, err := fix.Parse(frame)
	if err != nil {
		t.Fatalf("invalid message %s: %v", visible(frame), err)
	}
	sealed := m.Get(91)
	if sealed == nil {
		t.Fatalf("not sealed: %s", visible(frame))
	}
	plain, err := g.Open(nil, sealed)
	if err != nil {
		t.Fatalf("SecureData of %s does not open: %v", visible(frame), err)
	}
	return &m, string(plain)
}

// encryptedInitiator logs an initiator with g on to a scripted peer, the
// Logons in clear, and returns them, what it stores and what its Observer
// sees.
func encryptedInitiator(t *testing.T, g *gcm, change func(*session.Config)) (*session.Session, *recorder, *peer, *appendLog, *observed) {
	t.Helper()
	l := mem.Listen("acceptor")
	st := &appendLog{Store: memstore.New()}
	seen := &observed{}
	s, rec := startInitiator(t, l, func(c *session.Config) {
		c.Encrypter, c.Store, c.Observer = g, st, seen
		if change != nil {
			change(c)
		}
	})
	p := acceptPeer(t, l)
	if logon := p.expect("A", "98", g.method); logon.Get(91) != nil {
		t.Error("Logon sealed")
	}
	p.write(from(session.FIX44, 1), "A", "98", g.method, "108", "30")
	rec.wait(t, func() bool { return rec.logons == 1 })
	return s, rec, p, st, seen
}

// Once the Logons agreed, a message's body goes on the wire sealed into
// SecureData, its header in clear; the store and the Observer keep it in
// clear.
func TestEncrypter_SealsOnWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGCM(t, "7", "k")
		s, _, p, st, seen := encryptedInitiator(t, g, nil)
		if _, err := s.Send(context.Background(), fix.NewBuilder(session.FIX44, "D").SetString(11, "SECRET-1").SetData(95, 96, []byte("a\x01b"))); err != nil {
			t.Fatal(err)
		}
		wire := p.expectRaw()
		if bytes.Contains(wire, []byte("SECRET-1")) {
			t.Fatalf("plaintext on the wire: %s", visible(wire))
		}
		m, plain := unseal(t, g, wire)
		if got := string(m.MsgType()) + "," + string(m.Get(34)) + "," + string(m.Get(49)) + "," + string(m.Get(56)); got != "D,2,INI,ACC" {
			t.Errorf("clear header %s", visible(wire))
		}
		if m.Get(11) != nil || m.Get(52) == nil {
			t.Errorf("header or body misplaced: %s", visible(wire))
		}
		if plain != "11=SECRET-1\x0195=3\x0196=a\x01b\x01" {
			t.Errorf("SecureData holds %q", plain)
		}
		if stored := st.last(); !bytes.Contains(stored, []byte("\x0111=SECRET-1\x01")) || bytes.Contains(stored, []byte("\x0191=")) {
			t.Errorf("stored %s", visible(stored))
		}
		seen.find(t, false, "\x0111=SECRET-1\x01")
	})
}

// sealedMsg returns the peer's message seq of msgType, the fields after its
// header sealed by g into SecureData (91).
func sealedMsg(t *testing.T, g *gcm, seq int, msgType string, fields ...string) []byte {
	t.Helper()
	var body []byte
	for i := 0; i+1 < len(fields); i += 2 {
		tag, _ := strconv.Atoi(fields[i])
		body = fix.AppendTagValue(body, tag, fields[i+1])
	}
	sealed, err := g.Seal(nil, body)
	if err != nil {
		t.Fatal(err)
	}
	return from(session.FIX44, seq).build(msgType).SetData(90, 91, sealed).Build()
}

// expectSealed reads the next message, sealed by g, checks its MsgType and
// that what it seals holds each of fields, tag=value<SOH>, and returns it.
func (p *peer) expectSealed(g *gcm, msgType string, fields ...string) (*fix.Message, string) {
	p.t.Helper()
	m, plain := unseal(p.t, g, p.expectRaw())
	if string(m.MsgType()) != msgType {
		p.t.Fatalf("got MsgType %s, sealing %q, want %s", m.MsgType(), plain, msgType)
	}
	for i := 0; i+1 < len(fields); i += 2 {
		if f := fields[i] + "=" + fields[i+1] + "\x01"; !strings.HasPrefix(plain, f) && !strings.Contains(plain, "\x01"+f) {
			p.t.Fatalf("%s seals %q, without %q", msgType, plain, f)
		}
	}
	return m, plain
}

// Once the Logons agreed, the SecureData of a message received is opened:
// the header fields in it merged with those in clear, the others its body.
// The handler and the Observer see it in clear.
func TestEncrypter_OpensOnRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGCM(t, "7", "k")
		s, rec, p, _, seen := encryptedInitiator(t, g, nil)
		// A Heartbeat without body needs no SecureData.
		p.write(from(session.FIX44, 2), "0")
		p.writeRaw(sealedMsg(t, g, 3, "D", "34", "3", "128", "DESK", "11", "IN-1", "95", "3", "96", "a\x01b"))
		rec.wait(t, func() bool { return len(rec.apps) == 1 })
		if rec.apps[0] != "IN-1" {
			t.Errorf("delivered %q", rec.apps)
		}
		in := seen.find(t, true, "\x0111=IN-1\x01")
		m, err := fix.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if m.Get(91) != nil || m.Get(90) != nil || string(m.Get(128)) != "DESK" || string(m.Get(96)) != "a\x01b" {
			t.Errorf("observed %s", visible(in))
		}
		if i := bytes.Index(in, []byte("\x01128=")); i < 0 || i > bytes.Index(in, []byte("\x0111=")) {
			t.Errorf("DeliverToCompID not in the header: %s", visible(in))
		}
		// A TestRequest sealed is answered, sealed.
		p.writeRaw(sealedMsg(t, g, 4, "1", "112", "PING"))
		p.expectSealed(g, "0", "112", "PING")
		// A repeated header group, NoHops, in clear and repeated whole inside:
		// each entry matches its own.
		sealed, err := g.Seal(nil, []byte("627=2\x01628=H1\x01628=H2\x0111=IN-2\x01"))
		if err != nil {
			t.Fatal(err)
		}
		p.writeRaw(from(session.FIX44, 5).build("D", "627", "2", "628", "H1", "628", "H2").SetData(90, 91, sealed).Build())
		rec.wait(t, func() bool { return len(rec.apps) == 2 })
		if rec.apps[1] != "IN-2" || s.Stats().RejectsSent.Load() != 0 {
			t.Errorf("delivered %q, %d Rejects", rec.apps, s.Stats().RejectsSent.Load())
		}
	})
}

// A message that cannot be made out is rejected, SessionRejectReason 7,
// counted, and not delivered: SecureData that does not open, a header field
// in it differing from the clear one, a body in clear. The next one is
// handled.
func TestEncrypter_DecryptionProblem(t *testing.T) {
	other := newGCM(t, "7", "another key")
	for _, c := range []struct {
		name  string
		ref   string // RefTagID
		frame func(g *gcm) []byte
	}{
		{"does not open", "91", func(*gcm) []byte { return sealedMsg(t, other, 2, "D", "11", "BAD") }},
		{"header differs", "91", func(g *gcm) []byte { return sealedMsg(t, g, 2, "D", "34", "9", "11", "BAD") }},
		{"CompID differs", "91", func(g *gcm) []byte { return sealedMsg(t, g, 2, "D", "49", "EVE", "11", "BAD") }},
		{"second hop differs", "91", func(g *gcm) []byte {
			sealed, _ := g.Seal(nil, []byte("627=2\x01628=H1\x01628=EVE\x0111=BAD\x01"))
			return from(session.FIX44, 2).build("D", "627", "2", "628", "H1", "628", "H2").SetData(90, 91, sealed).Build()
		}},
		{"body in clear", "11", func(*gcm) []byte { return from(session.FIX44, 2).build("D", "11", "BAD").Build() }},
		{"body in clear beside SecureData", "1", func(g *gcm) []byte {
			sealed, _ := g.Seal(nil, []byte("11=BAD\x01"))
			return from(session.FIX44, 2).build("D", "1", "ACCT").SetData(90, 91, sealed).Build()
		}},
		{"not fields", "91", func(g *gcm) []byte {
			sealed, _ := g.Seal(nil, []byte("11=BAD"))
			return from(session.FIX44, 2).build("D").SetData(90, 91, sealed).Build()
		}},
		{"SecureData twice", "91", func(g *gcm) []byte {
			sealed, _ := g.Seal(nil, []byte("11=BAD\x01"))
			return from(session.FIX44, 2).build("D", "90", "1", "91", "x", "90", strconv.Itoa(len(sealed)), "91", string(sealed)).Build()
		}},
		{"signature sealed", "91", func(g *gcm) []byte {
			return sealedMsg(t, g, 2, "D", "11", "BAD", "93", "1", "89", "s")
		}},
		{"SecureData sealed", "91", func(g *gcm) []byte {
			return sealedMsg(t, g, 2, "D", "11", "BAD", "90", "1", "91", "x")
		}},
		{"CheckSum sealed", "91", func(g *gcm) []byte {
			return sealedMsg(t, g, 2, "D", "11", "BAD", "10", "000")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				g := newGCM(t, "7", "k")
				s, rec, p, _, _ := encryptedInitiator(t, g, nil)
				p.writeRaw(c.frame(g))
				_, plain := p.expectSealed(g, "3", "45", "2", "371", c.ref, "373", "7")
				if strings.Contains(plain, "BAD") || strings.Contains(plain, "EVE") {
					t.Errorf("Reject shows the message: %q", plain)
				}
				p.writeRaw(sealedMsg(t, g, 3, "D", "11", "GOOD"))
				rec.wait(t, func() bool { return len(rec.apps) == 1 })
				if rec.apps[0] != "GOOD" {
					t.Errorf("delivered %q", rec.apps)
				}
				if got := s.NextTargetSeqNum(); got != 4 {
					t.Errorf("NextTargetSeqNum = %d, want 4", got)
				}
			})
		})
	}
	t.Run("MsgSeqNum hidden", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := newGCM(t, "7", "k")
			_, _, p, _, _ := encryptedInitiator(t, g, nil)
			sealed, _ := other.Seal(nil, []byte("34=2\x0111=BAD\x01"))
			p.writeRaw(fix.NewBuilder(session.FIX44, "D").SetString(49, "ACC").SetString(56, "INI").
				SetString(52, string(fix.AppendUTCTimestamp(nil, time.Now(), fix.Milliseconds))).SetData(90, 91, sealed).Build())
			p.expectSealed(g, "5")
		})
	})
}

// SecureData where no encryption was agreed is a decryption problem: on a
// Logon, or on a session without Encrypter.
func TestEncrypter_SecureDataNotAgreed(t *testing.T) {
	t.Run("message", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, rec, p, _ := established(t, session.FIX44, func(c *session.Config) { c.LogoutTimeout = 200 * time.Millisecond })
			p.write(from(session.FIX44, 5), "D", "90", "1", "91", "x", "11", "BAD")
			p.expect("3", "45", "5", "371", "91", "373", "7")
			p.write(from(session.FIX44, 6), "D", "11", "GOOD")
			rec.wait(t, func() bool { return len(rec.apps) == 1 })
			if rec.apps[0] != "GOOD" || s.NextTargetSeqNum() != 7 {
				t.Errorf("delivered %q, NextTargetSeqNum %d", rec.apps, s.NextTargetSeqNum())
			}
		})
	})
	t.Run("Logon", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := mem.Listen("acceptor")
			_, rec := startInitiator(t, l, func(c *session.Config) { c.Encrypter = newGCM(t, "7", "k") })
			p := acceptPeer(t, l)
			p.expect("A")
			p.write(from(session.FIX44, 1), "A", "98", "7", "108", "30", "90", "1", "91", "x")
			p.expectSilentEnd()
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if rec.logons != 0 {
				t.Error("logged on")
			}
		})
	})
}

// encryptedPair runs an initiator and an acceptor of FIX 4.4, with ini and
// acc as their Encrypters, and the acceptor's configuration changed by
// change if not nil.
func encryptedPair(t *testing.T, ini, acc session.Encrypter, change func(*session.Config)) (is, as *session.Session, ir, ar *recorder) {
	t.Helper()
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	ar = newRecorder()
	cfg := session.Config{ID: session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"},
		Handler: ar, Encrypter: acc, LogoutTimeout: 200 * time.Millisecond}
	if change != nil {
		change(&cfg)
	}
	as, err := a.Add(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 50 * time.Millisecond})
	ir = newRecorder()
	is, err = i.Add(session.Config{ID: session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
		Handler: ir, Encrypter: ini, LogoutTimeout: 200 * time.Millisecond}, l)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.Run(ctx) }()
	go func() { defer wg.Done(); i.Run(ctx) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
	return is, as, ir, ar
}

// Two sessions with the same Encrypter log on and talk, both ways; with
// different Methods, they do not log on.
func TestEncrypter_Pair(t *testing.T) {
	t.Run("agreed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ini, acc, ir, ar := encryptedPair(t, newGCM(t, "7", "k"), newGCM(t, "7", "k"), nil)
			ir.wait(t, func() bool { return ir.logons == 1 })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, id := range []string{"A", "B"} {
				if _, err := ini.Send(ctx, fix.NewBuilder(session.FIX44, "D").SetString(11, id)); err != nil {
					t.Fatal(err)
				}
			}
			ar.wait(t, func() bool { return len(ar.apps) == 2 })
			r, err := acc.Send(ctx, fix.NewBuilder(session.FIX44, "8").SetString(11, "A"))
			if err != nil {
				t.Fatal(err)
			}
			// Acknowledged by a TestRequest and its Heartbeat, both sealed.
			if err := r.Wait(ctx, session.Acknowledged); err != nil {
				t.Fatalf("Wait(Acknowledged): %v", err)
			}
			ir.wait(t, func() bool { return len(ir.apps) == 1 })
			if got := ar.apps; got[0] != "A" || got[1] != "B" {
				t.Errorf("acceptor received %v", got)
			}
			for _, s := range []*session.Session{ini, acc} {
				if n := s.Stats().RejectsSent.Load(); n != 0 {
					t.Errorf("%s sent %d Rejects", s.ID(), n)
				}
			}
			if err := ini.Logout(ctx, "bye"); err != nil {
				t.Fatal(err)
			}
			ar.wait(t, func() bool { return len(ar.logouts) == 1 })
		})
	})
	t.Run("Methods differ", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ini, _, ir, ar := encryptedPair(t, newGCM(t, "7", "k"), newGCM(t, "6", "k"), nil)
			waitFor(t, func() bool { return ini.Stats().Connections.Load() >= 2 })
			ir.mu.Lock()
			ar.mu.Lock()
			defer ir.mu.Unlock()
			defer ar.mu.Unlock()
			if ir.logons != 0 || ar.logons != 0 {
				t.Errorf("logged on: initiator %d, acceptor %d times", ir.logons, ar.logons)
			}
		})
	})
}

// A resend after a reconnection is sealed anew from the message stored in
// clear, its PossDupFlag and OrigSendingTime in the clear header; the
// GapFill over the Logon is sealed too.
func TestEncrypter_ResendAfterReconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGCM(t, "7", "k")
		l := mem.Listen("acceptor")
		s, rec := startInitiator(t, l, func(c *session.Config) { c.Encrypter = g })
		p := acceptPeer(t, l)
		p.expect("A", "34", "1", "98", "7")
		p.write(from(session.FIX44, 1), "A", "98", "7", "108", "30")
		rec.wait(t, func() bool { return rec.logons == 1 })
		if _, err := s.Send(context.Background(), fix.NewBuilder(session.FIX44, "D").SetString(11, "O1")); err != nil {
			t.Fatal(err)
		}
		first, _ := p.expectSealed(g, "D", "11", "O1")
		p.conn.Close()
		rec.wait(t, func() bool { return len(rec.logouts) == 1 })

		p = acceptPeer(t, l)
		if logon := p.expect("A", "34", "3", "98", "7"); logon.Get(91) != nil {
			t.Error("Logon sealed")
		}
		p.write(from(session.FIX44, 2), "A", "98", "7", "108", "30")
		rec.wait(t, func() bool { return rec.logons == 2 })
		p.writeRaw(sealedMsg(t, g, 3, "2", "7", "2", "16", "0"))
		m, plain := p.expectSealed(g, "D", "11", "O1")
		if string(m.Get(34)) != "2" || string(m.Get(43)) != "Y" || !bytes.Equal(m.Get(122), first.Get(52)) {
			t.Errorf("resent header: 34=%s 43=%s 122=%s, first sent at %s", m.Get(34), m.Get(43), m.Get(122), first.Get(52))
		}
		if strings.Contains(plain, "43=") || strings.Contains(plain, "122=") || bytes.Equal(m.Get(91), first.Get(91)) {
			t.Errorf("resent SecureData %q not sealed anew", plain)
		}
		gf, _ := p.expectSealed(g, "4", "36", "4", "123", "Y")
		if string(gf.Get(34)) != "3" || string(gf.Get(43)) != "Y" || gf.Get(122) == nil {
			t.Errorf("GapFill header: 34=%s 43=%s 122=%s", gf.Get(34), gf.Get(43), gf.Get(122))
		}
	})
}

// A Logon resetting the sequence numbers on an established connection, and
// its answer, are in clear; what follows them stays sealed.
func TestEncrypter_ResetLogon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGCM(t, "7", "k")
		s, rec, p, _, _ := encryptedInitiator(t, g, nil)
		p.write(from(session.FIX44, 1), "A", "98", "7", "108", "30", "141", "Y")
		if answer := p.expect("A", "34", "1", "98", "7", "141", "Y"); answer.Get(91) != nil {
			t.Error("Logon answer sealed")
		}
		waitFor(t, func() bool { return s.NextTargetSeqNum() == 2 && s.State() == session.StateLoggedOn })
		if _, err := s.Send(context.Background(), fix.NewBuilder(session.FIX44, "D").SetString(11, "AFTER")); err != nil {
			t.Fatal(err)
		}
		if m, _ := p.expectSealed(g, "D", "11", "AFTER"); string(m.Get(34)) != "2" {
			t.Errorf("sent at %s, want 2", m.Get(34))
		}
		p.writeRaw(sealedMsg(t, g, 2, "D", "11", "IN"))
		rec.wait(t, func() bool { return len(rec.apps) == 1 })
	})
}

// failingSeal is an Encrypter whose Seal fails.
type failingSeal struct{ *gcm }

func (failingSeal) Seal([]byte, []byte) ([]byte, error) { return nil, errors.New("no key") }

// A Seal error is a write error: the connection closes, the message
// unwritten.
func TestEncrypter_SealFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := mem.Listen("acceptor")
		s, rec := startInitiator(t, l, func(c *session.Config) { c.Encrypter = failingSeal{newGCM(t, "7", "k")} })
		p := acceptPeer(t, l)
		p.expect("A", "98", "7")
		p.write(from(session.FIX44, 1), "A", "98", "7", "108", "30")
		rec.wait(t, func() bool { return rec.logons == 1 })
		if _, err := s.Send(context.Background(), fix.NewBuilder(session.FIX44, "D").SetString(11, "O1")); err != nil {
			t.Fatal(err)
		}
		p.expectSilentEnd()
		rec.wait(t, func() bool { return len(rec.logouts) == 1 })
		if n := s.Stats().MessagesSent.Load(); n != 1 {
			t.Errorf("MessagesSent = %d, want 1, the Logon", n)
		}
	})
}

// A signature, SignatureLength (93) and Signature (89), ends the message in
// clear, after SecureData, never within it, and arrives intact.
func TestEncrypter_SignatureInClear(t *testing.T) {
	sig := []byte("s\x01g")
	t.Run("wire", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := newGCM(t, "7", "k")
			s, _, p, _, _ := encryptedInitiator(t, g, nil)
			if _, err := s.Send(context.Background(), fix.NewBuilder(session.FIX44, "D").SetString(11, "SIGNED").SetData(93, 89, sig)); err != nil {
				t.Fatal(err)
			}
			wire := p.expectRaw()
			m, plain := unseal(t, g, wire)
			if plain != "11=SIGNED\x01" || !bytes.Equal(m.Get(89), sig) {
				t.Errorf("SecureData holds %q, Signature %q", plain, m.Get(89))
			}
			if i, j := bytes.Index(wire, []byte("\x0191=")), bytes.Index(wire, []byte("\x0193=3\x0189=s\x01g\x0110=")); i < 0 || j < i {
				t.Errorf("signature not after SecureData, before CheckSum: %s", visible(wire))
			}
		})
	})
	t.Run("received", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			seen := &observed{}
			ini, _, ir, ar := encryptedPair(t, newGCM(t, "7", "k"), newGCM(t, "7", "k"), func(c *session.Config) { c.Observer = seen })
			ir.wait(t, func() bool { return ir.logons == 1 })
			if _, err := ini.Send(context.Background(), fix.NewBuilder(session.FIX44, "D").SetString(11, "SIGNED").SetData(93, 89, sig)); err != nil {
				t.Fatal(err)
			}
			ar.wait(t, func() bool { return len(ar.apps) == 1 })
			if in := seen.find(t, true, "\x0111=SIGNED\x01"); !bytes.HasSuffix(in[:len(in)-7], []byte("\x0111=SIGNED\x0193=3\x0189=s\x01g\x01")) {
				t.Errorf("received %s", visible(in))
			}
		})
	})
}

// A message that does not open, received ahead of a gap, is not kept: the
// gap and it are asked for, and it is handled when it comes again.
func TestEncrypter_UndecryptableTooHigh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGCM(t, "7", "k")
		s, rec, p, _, _ := encryptedInitiator(t, g, nil)
		p.writeRaw(sealedMsg(t, newGCM(t, "7", "another key"), 4, "D", "11", "BAD"))
		p.expectSealed(g, "2", "7", "2", "16", "0")
		orig := string(fix.AppendUTCTimestamp(nil, time.Now().Add(-time.Second), fix.Milliseconds))
		resent := func(seq int, msgType string, fields ...string) []byte {
			var body []byte
			for i := 0; i+1 < len(fields); i += 2 {
				tag, _ := strconv.Atoi(fields[i])
				body = fix.AppendTagValue(body, tag, fields[i+1])
			}
			sealed, err := g.Seal(nil, body)
			if err != nil {
				t.Fatal(err)
			}
			return from(session.FIX44, seq).build(msgType, "43", "Y", "122", orig).SetData(90, 91, sealed).Build()
		}
		p.writeRaw(resent(2, "4", "123", "Y", "36", "4"))
		p.writeRaw(resent(4, "D", "11", "GOOD"))
		rec.wait(t, func() bool { return len(rec.apps) == 1 })
		if rec.apps[0] != "GOOD" || s.NextTargetSeqNum() != 5 || s.Stats().RejectsSent.Load() != 0 {
			t.Errorf("delivered %q, NextTargetSeqNum %d, %d Rejects", rec.apps, s.NextTargetSeqNum(), s.Stats().RejectsSent.Load())
		}
	})
}

// A header field only within SecureData, SendingTime here, is the message's:
// checked and delivered.
func TestEncrypter_SendingTimeSealed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGCM(t, "7", "k")
		s, rec, p, _, seen := encryptedInitiator(t, g, nil)
		sent := string(fix.AppendUTCTimestamp(nil, time.Now(), fix.Milliseconds))
		sealed, err := g.Seal(nil, []byte("52="+sent+"\x0111=IN\x01"))
		if err != nil {
			t.Fatal(err)
		}
		p.writeRaw(fix.NewBuilder(session.FIX44, "D").SetString(49, "ACC").SetString(56, "INI").SetInt(34, 2).SetData(90, 91, sealed).Build())
		rec.wait(t, func() bool { return len(rec.apps) == 1 })
		m, err := fix.Parse(seen.find(t, true, "\x0111=IN\x01"))
		if err != nil {
			t.Fatal(err)
		}
		if string(m.Get(52)) != sent || s.Stats().RejectsSent.Load() != 0 {
			t.Errorf("SendingTime %q, want %q; %d Rejects", m.Get(52), sent, s.Stats().RejectsSent.Load())
		}
	})
}

// Our Logout, once the Logons agreed, is sealed.
func TestEncrypter_LogoutSealed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGCM(t, "7", "k")
		s, _, p, _, _ := encryptedInitiator(t, g, nil)
		done := make(chan error, 1)
		go func() { done <- s.Logout(context.Background(), "bye") }()
		p.expectSealed(g, "5", "58", "bye")
		p.writeRaw(from(session.FIX44, 2).build("5").Build())
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
}

// bigOpen opens SecureData to more than a message may hold.
type bigOpen struct{ *gcm }

func (bigOpen) Open(dst, _ []byte) ([]byte, error) {
	return append(dst, "11="+strings.Repeat("x", fix.MaxBodyLength)+"\x01"...), nil
}

// What Open returns is bounded: beyond fix.MaxBodyLength, the message is
// rejected.
func TestEncrypter_OpenTooLarge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGCM(t, "7", "k")
		s, rec, p, _, _ := encryptedInitiator(t, g, func(c *session.Config) { c.Encrypter = bigOpen{g} })
		p.writeRaw(sealedMsg(t, g, 2, "D", "11", "BIG"))
		p.expectSealed(g, "3", "45", "2", "371", "91", "373", "7")
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if s.NextTargetSeqNum() != 3 || len(rec.apps) != 0 {
			t.Errorf("NextTargetSeqNum %d, delivered %q", s.NextTargetSeqNum(), rec.apps)
		}
	})
}

// concealed returns the peer's message seq of msgType whose header has, in
// clear, the CompIDs, MsgSeqNum and clear's fields, and whose SecureData,
// sealed by g, holds fields.
func concealed(t *testing.T, g *gcm, seq int, msgType string, clear, fields []string) []byte {
	t.Helper()
	b := fix.NewBuilder(session.FIX44, msgType).SetString(49, "ACC").SetString(56, "INI").SetInt(34, seq)
	for i := 0; i+1 < len(clear); i += 2 {
		tag, _ := strconv.Atoi(clear[i])
		b.Append(tag, []byte(clear[i+1]))
	}
	var body []byte
	for i := 0; i+1 < len(fields); i += 2 {
		tag, _ := strconv.Atoi(fields[i])
		body = fix.AppendTagValue(body, tag, fields[i+1])
	}
	sealed, err := g.Seal(nil, body)
	if err != nil {
		t.Fatal(err)
	}
	return b.SetData(90, 91, sealed).Build()
}

// A message that does not open may hide header fields that are not always
// in clear: SendingTime, a SubID, OrigSendingTime. Their absence is not
// judged: it is rejected for its SecureData, counted in sequence, asked for
// again ahead of a gap. Those it shows are still checked.
func TestEncrypter_UndecryptableHidesHeader(t *testing.T) {
	g, other := newGCM(t, "7", "k"), newGCM(t, "7", "another key")
	now := func() string { return string(fix.AppendUTCTimestamp(nil, time.Now(), fix.Milliseconds)) }
	t.Run("SendingTime", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, rec, p, _, _ := encryptedInitiator(t, g, nil)
			p.writeRaw(concealed(t, other, 2, "D", nil, []string{"52", now(), "11", "BAD"}))
			p.expectSealed(g, "3", "45", "2", "371", "91", "373", "7")
			p.writeRaw(sealedMsg(t, g, 3, "D", "11", "GOOD"))
			rec.wait(t, func() bool { return len(rec.apps) == 1 })
			if got := s.NextTargetSeqNum(); got != 4 {
				t.Errorf("NextTargetSeqNum = %d, want 4", got)
			}
		})
	})
	t.Run("SenderSubID", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := mem.Listen("acceptor")
			s, rec := startInitiator(t, l, func(c *session.Config) { c.Encrypter, c.ID.TargetSubID = g, "DESK" })
			p := acceptPeer(t, l)
			p.expect("A", "57", "DESK")
			p.write(from(session.FIX44, 1), "A", "50", "DESK", "98", "7", "108", "30")
			rec.wait(t, func() bool { return rec.logons == 1 })
			p.writeRaw(concealed(t, other, 2, "D", []string{"52", now()}, []string{"50", "DESK", "11", "BAD"}))
			p.expectSealed(g, "3", "45", "2", "371", "91", "373", "7")
			p.writeRaw(concealed(t, g, 3, "D", []string{"52", now()}, []string{"50", "DESK", "11", "GOOD"}))
			rec.wait(t, func() bool { return len(rec.apps) == 1 })
			if got := s.NextTargetSeqNum(); got != 4 || s.State() != session.StateLoggedOn {
				t.Errorf("NextTargetSeqNum = %d, want 4; %s", got, s.State())
			}
		})
	})
	t.Run("SendingTime, ahead of a gap", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, _, p, _, _ := encryptedInitiator(t, g, nil)
			p.writeRaw(concealed(t, other, 4, "D", nil, []string{"52", now(), "11", "BAD"}))
			p.expectSealed(g, "2", "7", "2", "16", "0")
		})
	})
	t.Run("OrigSendingTime of a PossDup", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _, p, _, _ := encryptedInitiator(t, g, nil)
			p.writeRaw(concealed(t, other, 2, "D", []string{"43", "Y", "52", now()}, []string{"122", now(), "11", "BAD"}))
			p.expectSealed(g, "3", "45", "2", "371", "91", "373", "7")
			waitFor(t, func() bool { return s.NextTargetSeqNum() == 3 })
		})
	})
	t.Run("CompIDs only within SecureData", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, rec, p, _, _ := encryptedInitiator(t, g, nil)
			sealed, err := g.Seal(nil, []byte("49=ACC\x0156=INI\x0111=BAD\x01"))
			if err != nil {
				t.Fatal(err)
			}
			p.writeRaw(fix.NewBuilder(session.FIX44, "D").SetInt(34, 2).SetString(52, now()).SetData(90, 91, sealed).Build())
			p.expectSealed(g, "3", "45", "2", "371", "49", "373", "1")
			p.writeRaw(sealedMsg(t, g, 3, "D", "11", "GOOD"))
			rec.wait(t, func() bool { return len(rec.apps) == 1 })
			if rec.apps[0] != "GOOD" {
				t.Errorf("delivered %q", rec.apps)
			}
		})
	})
}

// businessRejecter rejects every application message with a
// BusinessMessageReject whose BusinessRejectRefID is refID.
type businessRejecter struct {
	*recorder
	refID string
}

func (b businessRejecter) FromApp(context.Context, *session.Session, *fix.Message, session.Inbound) error {
	return &session.BusinessRejectError{Reason: session.BusinessRejectOther, RefID: b.refID, Text: "no"}
}

// A handler's BusinessRejectRefID holding a SOH is left out (see
// TestBusinessRejectRefIDWithSOH): the BusinessMessageReject parses, and is
// sealed as any other, the session logged on still.
func TestEncrypter_BusinessRejectRefIDWithSOH(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGCM(t, "7", "k")
		s, _, p, _, _ := encryptedInitiator(t, g, func(c *session.Config) {
			c.Handler = businessRejecter{c.Handler.(*recorder), "a\x01b"}
		})
		p.writeRaw(sealedMsg(t, g, 2, "D", "11", "IN"))
		if _, plain := p.expectSealed(g, "j", "45", "2", "380", "0"); strings.Contains(plain, "\x01379=") || strings.HasPrefix(plain, "379=") {
			t.Errorf("BusinessMessageReject seals %q, with BusinessRejectRefID", plain)
		}
		if s.State() != session.StateLoggedOn {
			t.Errorf("state %s, want logged on", s.State())
		}
	})
}

// fullOpen opens SecureData to as much as a message may hold, which the
// clear header then takes past it.
type fullOpen struct{ *gcm }

func (fullOpen) Open(dst, _ []byte) ([]byte, error) {
	return append(dst, "11="+strings.Repeat("x", fix.MaxBodyLength-4)+"\x01"...), nil
}

// The message rebuilt from what Open returns is bounded too: Open's result
// within fix.MaxBodyLength, the header beside it past it, it is rejected.
func TestEncrypter_OpenedMessageTooLarge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGCM(t, "7", "k")
		s, rec, p, _, _ := encryptedInitiator(t, g, func(c *session.Config) { c.Encrypter = fullOpen{g} })
		p.writeRaw(sealedMsg(t, g, 2, "D", "11", "BIG"))
		p.expectSealed(g, "3", "45", "2", "371", "91", "373", "7")
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if s.NextTargetSeqNum() != 3 || len(rec.apps) != 0 {
			t.Errorf("NextTargetSeqNum %d, delivered %q", s.NextTargetSeqNum(), rec.apps)
		}
	})
}

// failLater is an Encrypter whose Seal fails once fail is set.
type failLater struct {
	*gcm
	fail *atomic.Bool
}

func (f failLater) Seal(dst, plain []byte) ([]byte, error) {
	if f.fail.Load() {
		return dst, errors.New("key gone")
	}
	return f.gcm.Seal(dst, plain)
}

// A Seal error while resending is a write error too: the connection closes,
// nothing resent.
func TestEncrypter_SealFailureResending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGCM(t, "7", "k")
		var fail atomic.Bool
		s, rec, p, _, _ := encryptedInitiator(t, g, func(c *session.Config) { c.Encrypter = failLater{g, &fail} })
		if _, err := s.Send(context.Background(), fix.NewBuilder(session.FIX44, "D").SetString(11, "O1")); err != nil {
			t.Fatal(err)
		}
		p.expectSealed(g, "D", "11", "O1")
		fail.Store(true)
		p.writeRaw(sealedMsg(t, g, 2, "2", "7", "2", "16", "0"))
		p.expectSilentEnd()
		rec.wait(t, func() bool { return len(rec.logouts) == 1 })
		if n := s.Stats().Resent.Load(); n != 0 {
			t.Errorf("Resent = %d, want 0", n)
		}
	})
}

// A PossDup message whose SendingTime SecureData hides, and does not open,
// still has a readable OrigSendingTime checked on its own: malformed, or
// finer than the version allows, it is rejected for it (SessionRejectReason
// 6, RefTagID 122) and counted; only its comparison with SendingTime is
// left out.
func TestEncrypter_OrigSendingTimeWithSendingTimeHidden(t *testing.T) {
	g, other := newGCM(t, "7", "k"), newGCM(t, "7", "another key")
	for _, c := range []struct {
		name, begin, orig string
	}{
		{"malformed", session.FIX44, "BAD"},
		{"too fine", session.FIX43, string(fix.AppendUTCTimestamp(nil, time.Now(), fix.Microseconds))},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := mem.Listen("acceptor")
				s, _ := startInitiator(t, l, func(cfg *session.Config) { cfg.ID.BeginString, cfg.Encrypter = c.begin, g })
				p := acceptPeer(t, l)
				p.expect("A", "98", "7")
				p.write(from(c.begin, 1), "A", "98", "7", "108", "30")
				waitFor(t, func() bool { return s.State() == session.StateLoggedOn })
				sealed, err := other.Seal(nil, []byte("52="+string(fix.AppendUTCTimestamp(nil, time.Now(), fix.Milliseconds))+"\x0111=BAD\x01"))
				if err != nil {
					t.Fatal(err)
				}
				p.writeRaw(fix.NewBuilder(c.begin, "D").SetString(49, "ACC").SetString(56, "INI").SetInt(34, 2).
					SetString(43, "Y").SetString(122, c.orig).SetData(90, 91, sealed).Build())
				p.expectSealed(g, "3", "45", "2", "371", "122", "373", "6")
				waitFor(t, func() bool { return s.NextTargetSeqNum() == 3 })
			})
		})
	}
}
