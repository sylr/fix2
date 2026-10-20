package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
)

// plainEncrypter seals nothing: SecureData holds the fields as they are,
// and one starting with '!' does not open.
type plainEncrypter struct{}

func (plainEncrypter) Method() string { return "7" }

func (plainEncrypter) Seal(dst, plain []byte) ([]byte, error) { return append(dst, plain...), nil }

func (plainEncrypter) Open(dst, sealed []byte) ([]byte, error) {
	if len(sealed) > 0 && sealed[0] == '!' {
		return dst, errors.New("does not open")
	}
	return append(dst, sealed...), nil
}

// lines is a slog.Handler keeping the messages it is given.
type lines struct {
	mu   sync.Mutex
	msgs []string
}

func (l *lines) Enabled(context.Context, slog.Level) bool { return true }
func (l *lines) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *lines) WithGroup(string) slog.Handler            { return l }

func (l *lines) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, r.Level.String()+" "+r.Message)
	return nil
}

// undecryptable acts only on the current connection, logged on: before
// logon it leaves the message to be handled as any other; on a connection
// closed, or no longer the session's, it drops it; after the counterparty's
// Logout it ignores it, with an Info log. None rejects it.
func TestUndecryptableGuards(t *testing.T) {
	frame := wrapFrame(nil, []byte(FIX44), []byte("35=D\x0134=2\x0149=A\x0152=20260101-00:00:00.000\x0156=B\x0190=1\x0191=!\x01"))
	for _, c := range []struct {
		name    string
		phase   phase
		current bool // the session's connection
		closed  bool
		peerOut bool // after the counterparty's Logout
		handled bool
		logged  string
	}{
		{name: "awaiting logon", phase: phaseAwaitingLogon, current: true},
		{name: "logon sent", phase: phaseLogonSent, current: true},
		{name: "not the session's", phase: phaseEstablished, handled: true},
		{name: "closed", phase: phaseEstablished, current: true, closed: true, handled: true},
		{name: "after the counterparty's Logout", phase: phaseLogoutSent, current: true, peerOut: true, handled: true,
			logged: "INFO undecryptable message after the counterparty's Logout ignored"},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := &lines{}
			s, err := New(Config{ID: SessionID{BeginString: FIX44, SenderCompID: "B", TargetCompID: "A"}, Handler: BaseHandler{},
				Encrypter: plainEncrypter{}, Logger: slog.New(log)})
			if err != nil {
				t.Fatal(err)
			}
			cn := &conn{s: s, encrypted: true, closed: c.closed, peerLoggedOut: c.peerOut, timer: time.NewTimer(time.Hour)}
			defer cn.timer.Stop()
			s.phase = c.phase
			if c.current {
				s.c = cn
			}
			var m fix.Message
			if err := fix.ParseInto(&m, frame, s.dicts.parse); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if got := cn.undecryptable(&m, tagSecureData, "does not open", now, now); got != c.handled {
				t.Errorf("undecryptable = %v, want %v", got, c.handled)
			}
			if n := s.stats.RejectsSent.Load(); n != 0 {
				t.Errorf("%d Rejects sent", n)
			}
			log.mu.Lock()
			defer log.mu.Unlock()
			if got := strings.Join(log.msgs, "; "); got != c.logged {
				t.Errorf("logged %q, want %q", got, c.logged)
			}
		})
	}
}

// FuzzEncrypterOpen feeds open a message received on a connection that
// agreed on encryption: header fields, clear after the session's own, then
// SecureData sealed. Whatever it holds, open does not panic; what it cannot
// make out is left as it was; what it opens has no SecureData, and, sealed
// again and opened, is the same frame.
func FuzzEncrypterOpen(f *testing.F) {
	for _, seed := range [][2]string{
		{"", "11=A\x01"},
		{"128=X\x01", "34=2\x01115=Y\x0111=A\x0195=3\x0196=a\x01b\x01"},
		{"", "34=3\x0111=A\x01"},
		{"11=C\x01", "11=A\x01"},
		{"", "!"},
		{"", "11=A"},
		{"", ""},
		{"", "212=3\x01213=<\x01>\x0111=A\x01"},
		{"93=1\x0189=s\x01", "11=A\x01"},
		{"", "90=1\x0191=x\x0111=A\x01"},
		{"", "11=A\x0110=000\x01"},
		{"", "52=20260101-00:00:00.000\x0111=A\x0134=2\x01"},
	} {
		f.Add(seed[0], seed[1])
	}
	s, err := New(Config{ID: SessionID{BeginString: FIX44, SenderCompID: "B", TargetCompID: "A"}, Handler: BaseHandler{}, Encrypter: plainEncrypter{}})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, clear, sealed string) {
		body := "35=D\x0134=2\x0149=A\x0152=20260101-00:00:00.000\x0156=B\x01" + clear + "90=" + itoa(uint64(len(sealed))) + "\x0191=" + sealed + "\x01"
		frame := wrapFrame(nil, []byte(FIX44), []byte(body))
		var m fix.Message
		if s.cfg.parseOptions().ParseInto(&m, frame, s.dicts.parse) != nil || string(m.MsgType()) == "A" {
			return
		}
		c := &conn{s: s, encrypted: true}
		om, opened, _, why := c.open(&m, frame)
		if why != "" {
			if om != &m || !bytes.Equal(opened, frame) {
				t.Fatalf("%q, not made out, changed", frame)
			}
			return
		}
		if om.Get(tagSecureData) != nil || om.Get(tagSecureDataLen) != nil {
			t.Fatalf("%q opened as %q, SecureData left", frame, opened)
		}
		opened = bytes.Clone(opened)
		resealed, err := c.sealFrame(nil, opened)
		if err != nil {
			t.Fatalf("%q opened as %q, which does not seal: %v", frame, opened, err)
		}
		var again fix.Message
		if err := s.cfg.parseOptions().ParseInto(&again, resealed, s.dicts.parse); err != nil {
			t.Fatalf("%q sealed as %q, which does not parse: %v", opened, resealed, err)
		}
		if _, reopened, _, why := c.open(&again, resealed); why != "" || !bytes.Equal(reopened, opened) {
			t.Fatalf("%q sealed as %q, opened again as %q (%s)", opened, resealed, reopened, why)
		}
	})
}
