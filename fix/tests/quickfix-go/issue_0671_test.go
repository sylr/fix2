package quickfixgo

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/671
//
// An application reset the sequence numbers on its first Logon only by
// setting ResetSeqNumFlag (141=Y) in ToAdmin; QuickFIX/Go 0.9.3 called
// ToAdmin after deciding whether to reset, and the flag went out without the
// engine resetting. In fix2 the session alone writes 141 (ResetOnLogon): a
// ToAdmin setting it must fail loudly, cancelling the Logon, rather than send
// a flag the session does not act on.

// i671Handler sets ResetSeqNumFlag on its Logons.
type i671Handler struct {
	*recorder
}

func (i671Handler) ToAdmin(_ context.Context, _ *session.Session, msgType string, b *fix.Builder) error {
	if msgType == "A" {
		b.SetString(141, "Y")
	}
	return nil
}

// i671Log is a log written concurrently.
type i671Log struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *i671Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *i671Log) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestIssue671_ToAdminCannotSetResetSeqNumFlag(t *testing.T) {
	log := &i671Log{}
	_, _, l := startInitiator(t, func(c *session.Config) {
		c.Handler = i671Handler{newRecorder()}
		c.Logger = slog.New(slog.NewTextHandler(log, nil))
	})
	p := acceptPeer(t, l)
	select {
	case ev := <-p.frames:
		if ev.err == nil {
			t.Fatalf("got %s, want the Logon cancelled", visible(ev.frame))
		}
	case <-time.After(timeout):
		t.Fatal("connection still open")
	}
	waitFor(t, "the error logged", func() bool { return strings.Contains(log.String(), "tag 141 is set by the session") })
}
