package quickfixgo

import (
	"context"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/356
//
// Sessions were looked up by BeginString and CompIDs only: SubIDs and
// LocationIDs were ignored, and sessions differing by them could not be told
// apart. They are part of a session's identity: the acceptor must route by
// them, and every message must carry them.
func TestIssue356_SubAndLocationIDsIdentifySessions(t *testing.T) {
	deskID := func(desk string) session.SessionID {
		return session.SessionID{
			BeginString:  session.FIX44,
			SenderCompID: "ACC", SenderSubID: "ACCSUB", SenderLocationID: "ACCLOC",
			TargetCompID: "INI", TargetSubID: desk, TargetLocationID: "INILOC",
		}
	}
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{Logger: testLogger()})
	rec1, rec2 := newRecorder(), newRecorder()
	if _, err := a.Add(accConfig(rec1, func(c *session.Config) { c.ID = deskID("DESK1") })); err != nil {
		t.Fatal(err)
	}
	acc, err := a.Add(accConfig(rec2, func(c *session.Config) { c.ID = deskID("DESK2") }))
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	run(t, a.Run)

	irec := newRecorder()
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 20 * time.Millisecond, ReconnectMax: 50 * time.Millisecond})
	ini, err := i.Add(iniConfig(irec, func(c *session.Config) {
		c.ID = session.SessionID{
			BeginString:  session.FIX44,
			SenderCompID: "INI", SenderSubID: "DESK2", SenderLocationID: "INILOC",
			TargetCompID: "ACC", TargetSubID: "ACCSUB", TargetLocationID: "ACCLOC",
		}
	}), l)
	if err != nil {
		t.Fatal(err)
	}
	run(t, i.Run)
	irec.wait(t, "initiator logon", func() bool { return irec.logons == 1 })
	rec2.wait(t, "DESK2 logon", func() bool { return rec2.logons == 1 })
	rec1.mu.Lock()
	if rec1.logons != 0 {
		t.Errorf("DESK1 logged on too")
	}
	rec1.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := ini.Send(ctx, fix.NewBuilder(session.FIX44, "D").SetString(11, "O1")); err != nil {
		t.Fatal(err)
	}
	rec2.wait(t, "order", func() bool { return len(rec2.apps) == 1 })
	if _, err := acc.Send(ctx, fix.NewBuilder(session.FIX44, "8").SetString(11, "O1")); err != nil {
		t.Fatal(err)
	}
	irec.wait(t, "report", func() bool { return len(irec.apps) == 1 })
	rec2.mu.Lock()
	order := rec2.apps[0]
	rec2.mu.Unlock()
	irec.mu.Lock()
	report := irec.apps[0]
	irec.mu.Unlock()
	for _, c := range []struct {
		m                    *fix.Message
		sub, loc, tsub, tloc string
	}{
		{order, "DESK2", "INILOC", "ACCSUB", "ACCLOC"},
		{report, "ACCSUB", "ACCLOC", "DESK2", "INILOC"},
	} {
		if got := [4]string{string(c.m.Get(50)), string(c.m.Get(142)), string(c.m.Get(57)), string(c.m.Get(143))}; got != [4]string{c.sub, c.loc, c.tsub, c.tloc} {
			t.Errorf("MsgType %s: 50, 142, 57, 143 = %q, want %q", c.m.MsgType(), got, [4]string{c.sub, c.loc, c.tsub, c.tloc})
		}
	}
}
