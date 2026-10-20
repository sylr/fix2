package session_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
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
