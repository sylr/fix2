// Command quickfixgo is the benchmark agent of QuickFIX/Go
// (github.com/quickfixgo/quickfix): the acceptor or the initiator of the
// scenario (see package scenario), with QuickFIX/Go's defaults but for what
// the scenario sets.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"
	"github.com/sylr/fix2/tests/benchmarks/internal/scenario"
)

func main() {
	p, err := scenario.ParseArgs(os.Args[1:])
	if err == nil {
		switch p.Role {
		case "version":
			fmt.Println(scenario.Version("github.com/quickfixgo/quickfix"))
			return
		case "acceptor":
			err = acceptor(p)
		case "initiator":
			err = initiator(p)
		}
	}
	scenario.Exit(err)
}

// settings returns the session's settings: those of the scenario, the
// dictionary of p if any; the store (memory) and the log (none) are given
// to NewAcceptor and NewInitiator.
func settings(p scenario.Params, sender, target string, extra ...string) (*quickfix.Settings, error) {
	lines := []string{
		"[DEFAULT]",
		"BeginString=" + scenario.BeginString,
		"HeartBtInt=" + strconv.Itoa(scenario.HeartBtInt),
		"ResetOnLogon=Y",
	}
	lines = append(lines, extra...)
	lines = append(lines, "[SESSION]", "SenderCompID="+sender, "TargetCompID="+target)
	if p.Dict != "" {
		lines = append(lines, "DataDictionary="+p.Dict)
	} else {
		// Without a dictionary QuickFIX/Go still checks for empty values
		// and fields out of their section; fix2 checks nothing.
		lines = append(lines, "ValidateFieldsHaveValues=N", "ValidateFieldsOutOfOrder=N")
	}
	return quickfix.ParseSettings(strings.NewReader(strings.Join(lines, "\n") + "\n"))
}

// watcher reports the Rejects and unexpected messages a session receives
// and sends.
type watcher struct {
	stopping atomic.Bool // the agent logs out: a logout is expected
	logons   chan struct{}
}

func (w *watcher) OnCreate(quickfix.SessionID) {}

func (w *watcher) OnLogon(quickfix.SessionID) {
	select {
	case w.logons <- struct{}{}:
	default:
	}
}

func (w *watcher) OnLogout(id quickfix.SessionID) {
	if !w.stopping.Load() {
		scenario.Anomaly("%s logged out", id)
	}
}

func (w *watcher) ToAdmin(m *quickfix.Message, id quickfix.SessionID) {
	switch {
	case m.IsMsgTypeOf("3"):
		scenario.Anomaly("%s sent a Reject: %s", id, m)
	case m.IsMsgTypeOf("2"), m.IsMsgTypeOf("4"):
		// On the loopback, recovering a gap means a message was lost.
		scenario.Anomaly("%s sent a sequence recovery: %s", id, m)
	}
}

// ToApp runs for every message sent: it checks nothing, the fix2 agent
// having nothing to match it. The counterparty reports a
// BusinessMessageReject it receives.
func (w *watcher) ToApp(*quickfix.Message, quickfix.SessionID) error { return nil }

func (w *watcher) FromAdmin(m *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	switch {
	case m.IsMsgTypeOf("3"):
		scenario.Anomaly("%s received a Reject: %s", id, m)
	case m.IsMsgTypeOf("2"), m.IsMsgTypeOf("4"):
		scenario.Anomaly("%s received a sequence recovery: %s", id, m)
	}
	return nil
}

func (w *watcher) unexpected(m *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	scenario.Anomaly("%s received %s", id, m)
	return quickfix.UnsupportedMessageType()
}

// exchange acknowledges each NewOrderSingle with an ExecutionReport.
type exchange struct {
	watcher
	n int // FromApp runs on the session's goroutine
}

func (x *exchange) OnLogout(quickfix.SessionID) {}

func (x *exchange) FromApp(m *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	if !m.IsMsgTypeOf("D") {
		return x.unexpected(m, id)
	}
	x.n++
	orderID := strconv.Itoa(x.n)
	get := func(tag quickfix.Tag) string {
		v, _ := m.Body.GetString(tag)
		return v
	}
	qty := get(38)
	er := quickfix.NewMessage()
	er.Header.SetString(35, "8")
	er.Body.SetString(6, scenario.Zero).
		SetString(11, get(11)).
		SetString(14, scenario.Zero).
		SetString(17, orderID).
		SetString(37, orderID).
		SetString(38, qty).
		SetString(39, scenario.OrdStatusNew).
		SetString(44, get(44)).
		SetString(54, get(54)).
		SetString(55, get(55)).
		SetField(60, quickfix.FIXUTCTimestamp{Time: time.Now(), Precision: quickfix.Millis}).
		SetString(150, scenario.ExecTypeNew).
		SetString(151, qty)
	if err := quickfix.SendToTarget(er, id); err != nil {
		scenario.Anomaly("%s: sending ExecutionReport %s: %v", id, orderID, err)
	}
	return nil
}

func acceptor(p scenario.Params) error {
	x := &exchange{}
	st, err := settings(p, scenario.AcceptorCompID, scenario.InitiatorCompID,
		"SocketAcceptHost="+scenario.Host, "SocketAcceptPort="+strconv.Itoa(p.Port))
	if err != nil {
		return err
	}
	a, err := quickfix.NewAcceptor(x, quickfix.NewMemoryStoreFactory(), st, quickfix.NewNullLogFactory())
	if err != nil {
		return err
	}
	if err := a.Start(); err != nil {
		return err
	}
	err = scenario.ServeAcceptor(scenario.NewControl())
	a.Stop()
	return err
}

// client passes the ExecutionReports it receives to the tracker.
type client struct {
	watcher
	tr *scenario.Tracker
}

func (c *client) FromApp(m *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	if !m.IsMsgTypeOf("8") {
		return c.unexpected(m, id)
	}
	clOrdID, _ := m.Body.GetBytes(11)
	c.tr.Ack(clOrdID)
	return nil
}

func initiator(p scenario.Params) error {
	c := &client{tr: scenario.NewTracker(p), watcher: watcher{logons: make(chan struct{}, 1)}}
	st, err := settings(p, scenario.InitiatorCompID, scenario.AcceptorCompID,
		"SocketConnectHost="+scenario.Host, "SocketConnectPort="+strconv.Itoa(p.Port))
	if err != nil {
		return err
	}
	i, err := quickfix.NewInitiator(c, quickfix.NewMemoryStoreFactory(), st, quickfix.NewNullLogFactory())
	if err != nil {
		return err
	}
	if err := i.Start(); err != nil {
		return err
	}
	defer func() {
		c.stopping.Store(true)
		i.Stop()
	}()
	select {
	case <-c.logons:
	case <-time.After(10 * time.Second):
		return errors.New("timed out waiting for the logon")
	}

	ctl := scenario.NewControl()
	id := quickfix.SessionID{BeginString: scenario.BeginString, SenderCompID: scenario.InitiatorCompID, TargetCompID: scenario.AcceptorCompID}
	send := func(clOrdID []byte) error {
		nos := quickfix.NewMessage()
		nos.Header.SetString(35, "D")
		nos.Body.SetString(11, string(clOrdID)).
			SetString(21, scenario.HandlInst).
			SetString(38, scenario.OrderQty).
			SetString(40, scenario.OrdType).
			SetString(44, scenario.Price).
			SetString(54, scenario.Side).
			SetString(55, scenario.Symbol).
			SetString(59, scenario.TimeInForce).
			SetField(60, quickfix.FIXUTCTimestamp{Time: time.Now(), Precision: quickfix.Millis})
		return quickfix.SendToTarget(nos, id)
	}
	if err := c.tr.Run(ctl, send); err != nil {
		return err
	}
	// The driver closes stdin once it has the result: then log out.
	for {
		if _, ok := ctl.Next(); !ok {
			return nil
		}
	}
}
