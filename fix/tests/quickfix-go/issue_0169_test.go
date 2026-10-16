package quickfixgo

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store/memstore"
)

// https://github.com/quickfixgo/quickfix/issues/169
//
// When both sides asked for resends at the same time, at logon, the session
// deadlocked: neither ResendRequest was served, no heartbeat, no disconnect,
// no progress. Two sessions each missing the other's messages must both
// recover them.
func TestIssue169_BothSidesResendAtLogon(t *testing.T) {
	ini, acc, ir, ar := pair(t,
		func(c *session.Config) { c.Store = i169Store(t, "INI", "ACC") },
		func(c *session.Config) { c.Store = i169Store(t, "ACC", "INI") })
	ir.wait(t, "the acceptor's messages", func() bool { return len(ir.apps) == 2 })
	ar.wait(t, "the initiator's messages", func() bool { return len(ar.apps) == 2 })
	if got, want := ir.appValues(11), []string{"ACC1", "ACC2"}; !slices.Equal(got, want) {
		t.Errorf("initiator got %q, want %q", got, want)
	}
	if got, want := ar.appValues(11), []string{"INI1", "INI2"}; !slices.Equal(got, want) {
		t.Errorf("acceptor got %q, want %q", got, want)
	}
	waitFor(t, "sequence numbers in step", func() bool {
		return ini.NextTargetSeqNum() == acc.NextSenderSeqNum() && acc.NextTargetSeqNum() == ini.NextSenderSeqNum()
	})
}

// i169Store returns a store of sender's session that sent two orders to
// target, which target never received.
func i169Store(t *testing.T, sender, target string) *memstore.Store {
	t.Helper()
	ctx := context.Background()
	st := memstore.New()
	gen, err := st.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sent := string(fix.AppendUTCTimestamp(nil, time.Now().Add(-time.Minute), fix.Milliseconds))
	for seq := range uint64(2) {
		seq++
		m := raw(fmt.Sprintf("8=FIX.4.4|35=D|34=%d|49=%s|52=%s|56=%s|11=%s%d", seq, sender, sent, target, sender, seq))
		if err := st.Append(ctx, gen.Generation, seq, m); err != nil {
			t.Fatal(err)
		}
	}
	return st
}
