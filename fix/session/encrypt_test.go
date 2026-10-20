package session_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store"
	"github.com/sylr/fix2/fix/session/store/memstore"
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
	} {
		cfg := session.Config{
			ID:        session.SessionID{BeginString: c.begin, SenderCompID: "INI", TargetCompID: "ACC"},
			Handler:   newRecorder(),
			Encrypter: newGCM(t, c.method, "k"),
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

// SecureDataLen (90) and SecureData (91) are the session's to write: neither
// Send nor ToAdmin sets them.
func TestEncrypter_SecureDataReserved(t *testing.T) {
	t.Run("Send", func(t *testing.T) {
		s, _, _, _ := established(t, session.FIX44, func(c *session.Config) { c.LogoutTimeout = 200 * time.Millisecond })
		for _, b := range []*fix.Builder{
			fix.NewBuilder(session.FIX44, "D").SetString(11, "O2").SetData(90, 91, []byte("x")),
			fix.NewBuilder(session.FIX44, "D").SetString(11, "O2").SetString(91, "x"),
		} {
			if _, err := s.Send(context.Background(), b); err == nil {
				t.Error("Send with SecureData succeeded")
			}
		}
	})
	t.Run("ToAdmin", func(t *testing.T) {
		l := mem.Listen("acceptor")
		startInitiator(t, l, func(c *session.Config) {
			c.Handler = adminSetter{newRecorder(), func(b *fix.Builder) { b.SetData(90, 91, []byte("x")) }}
		})
		p := acceptPeer(t, l)
		p.expectSilentEnd()
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
	}
	t.Run("acceptor accepts its own", func(t *testing.T) {
		rec, l := acceptor(t)
		p := dialPeer(t, l)
		p.write(logon(), "A", "98", "7", "108", "30")
		p.expect("A", "98", "7")
		rec.wait(t, func() bool { return rec.logons == 1 })
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
	}
	t.Run("initiator accepts its own", func(t *testing.T) {
		l := mem.Listen("acceptor")
		_, rec := startInitiator(t, l, func(cfg *session.Config) { cfg.Encrypter = newGCM(t, "7", "k") })
		p := acceptPeer(t, l)
		p.expect("A", "98", "7")
		p.write(from(session.FIX44, 1), "A", "98", "7", "108", "30")
		rec.wait(t, func() bool { return rec.logons == 1 })
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
}
