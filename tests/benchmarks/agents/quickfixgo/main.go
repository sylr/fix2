// Command quickfixgo is the benchmark agent of QuickFIX/Go
// (github.com/quickfixgo/quickfix): the acceptor or the initiator of the
// scenarios (see package scenario), with QuickFIX/Go's defaults but for
// what the scenarios set. It builds and reads every message with the code
// QuickFIX/Go's generate-fix generates from the dictionary
// (gen/quickfixgo), and routes those it receives with a MessageRouter.
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
	"github.com/shopspring/decimal"
	"github.com/sylr/fix2/tests/benchmarks/gen/quickfixgo/enum"
	"github.com/sylr/fix2/tests/benchmarks/gen/quickfixgo/field"
	"github.com/sylr/fix2/tests/benchmarks/gen/quickfixgo/fix44/executionreport"
	"github.com/sylr/fix2/tests/benchmarks/gen/quickfixgo/fix44/marketdatarequest"
	"github.com/sylr/fix2/tests/benchmarks/gen/quickfixgo/fix44/marketdatasnapshotfullrefresh"
	"github.com/sylr/fix2/tests/benchmarks/gen/quickfixgo/fix44/newordersingle"
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
// dictionary of p; the store (memory) and the log (none) are given to
// NewAcceptor and NewInitiator.
func settings(p scenario.Params, sender, target string, extra ...string) (*quickfix.Settings, error) {
	lines := []string{
		"[DEFAULT]",
		"BeginString=" + scenario.BeginString,
		"HeartBtInt=" + strconv.Itoa(scenario.HeartBtInt),
		"ResetOnLogon=Y",
	}
	lines = append(lines, extra...)
	lines = append(lines, "[SESSION]", "SenderCompID="+sender, "TargetCompID="+target, "DataDictionary="+p.Dict)
	return quickfix.ParseSettings(strings.NewReader(strings.Join(lines, "\n") + "\n"))
}

// dec is a decimal and the scale it is written with: its own, so that it is
// written as it was read.
type dec struct {
	v     decimal.Decimal
	scale int32
}

func decOf(v decimal.Decimal) dec { return dec{v, max(0, -v.Exponent())} }

func decFloat(f float64) dec { return decOf(decimal.NewFromFloat(f)) }

// The scenario's values as decimals, converted once.
var (
	orderQty = decFloat(scenario.OrderQty)
	price    = decFloat(scenario.Price)
	zero     = decOf(decimal.Zero)
	book     [2][scenario.MarketDepth]struct{ px, size dec }
)

func init() {
	for k := range scenario.Book {
		for lvl, l := range scenario.Book[k] {
			book[k][lvl].px, book[k][lvl].size = decFloat(l.Px), decFloat(l.Size)
		}
	}
}

// app is the session's Application: it reports the Rejects, sequence
// recovery and unexpected messages a session sends and receives, and routes
// the application messages it receives to its routes.
type app struct {
	*quickfix.MessageRouter
	stopping atomic.Bool // the agent logs out: a logout is expected
	logons   chan struct{}
}

func newApp() *app {
	return &app{MessageRouter: quickfix.NewMessageRouter(), logons: make(chan struct{}, 1)}
}

func (a *app) OnCreate(quickfix.SessionID) {}

func (a *app) OnLogon(quickfix.SessionID) {
	select {
	case a.logons <- struct{}{}:
	default:
	}
}

func (a *app) OnLogout(id quickfix.SessionID) {
	if !a.stopping.Load() {
		scenario.Anomaly("%s logged out", id)
	}
}

func (a *app) ToAdmin(m *quickfix.Message, id quickfix.SessionID) {
	switch {
	case m.IsMsgTypeOf("3"):
		scenario.Anomaly("%s sent a Reject: %s", id, m)
	case m.IsMsgTypeOf("2"), m.IsMsgTypeOf("4"):
		// On the loopback, recovering a gap means a message was lost.
		scenario.Anomaly("%s sent a sequence recovery: %s", id, m)
	}
}

// ToApp runs for every message sent: it checks nothing, the other agents
// having nothing to match it. The counterparty reports a
// BusinessMessageReject it receives.
func (a *app) ToApp(*quickfix.Message, quickfix.SessionID) error { return nil }

func (a *app) FromAdmin(m *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	switch {
	case m.IsMsgTypeOf("3"):
		scenario.Anomaly("%s received a Reject: %s", id, m)
	case m.IsMsgTypeOf("2"), m.IsMsgTypeOf("4"):
		scenario.Anomaly("%s received a sequence recovery: %s", id, m)
	}
	return nil
}

func (a *app) FromApp(m *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	err := a.Route(m, id)
	if err != nil {
		scenario.Anomaly("%s received %s: %v", id, m, err)
	}
	return err
}

// checked reports err, a getter's, as an anomaly.
func checked[T any](v T, err quickfix.MessageRejectError) T {
	if err != nil {
		scenario.Anomaly("%v", err)
	}
	return v
}

// exchange answers each NewOrderSingle with an ExecutionReport, each
// MarketDataRequest with a MarketDataSnapshotFullRefresh. Its routes run on
// the session's goroutine.
type exchange struct {
	*app
	n int
}

func (x *exchange) onNewOrderSingle(m newordersingle.NewOrderSingle, id quickfix.SessionID) quickfix.MessageRejectError {
	clOrdID := checked(m.GetClOrdID())
	symbol := checked(m.GetSymbol())
	side := checked(m.GetSide())
	qty := decOf(checked(m.GetOrderQty()))
	px := decOf(checked(m.GetPrice()))
	parties := checked(m.GetNoPartyIDs())
	x.n++
	orderID := strconv.Itoa(x.n)
	er := executionreport.New(
		field.NewOrderID(orderID), field.NewExecID(orderID),
		field.NewExecType(enum.ExecType_NEW), field.NewOrdStatus(enum.OrdStatus_NEW),
		field.NewSide(side), field.NewLeavesQty(qty.v, qty.scale),
		field.NewCumQty(zero.v, zero.scale), field.NewAvgPx(zero.v, zero.scale))
	er.SetClOrdID(clOrdID)
	er.SetOrderQty(qty.v, qty.scale)
	er.SetPrice(px.v, px.scale)
	er.SetSymbol(symbol)
	er.SetTransactTime(time.Now())
	g := executionreport.NewNoPartyIDsRepeatingGroup()
	for i := range parties.Len() {
		p := parties.Get(i)
		e := g.Add()
		e.SetPartyID(checked(p.GetPartyID()))
		e.SetPartyIDSource(checked(p.GetPartyIDSource()))
		e.SetPartyRole(checked(p.GetPartyRole()))
	}
	er.SetNoPartyIDs(g)
	if err := quickfix.SendToTarget(er, id); err != nil {
		scenario.Anomaly("%s: sending ExecutionReport %s: %v", id, orderID, err)
	}
	return nil
}

func (x *exchange) onMarketDataRequest(m marketdatarequest.MarketDataRequest, id quickfix.SessionID) quickfix.MessageRejectError {
	reqID := checked(m.GetMDReqID())
	sub := checked(m.GetSubscriptionRequestType())
	depth := checked(m.GetMarketDepth())
	types := checked(m.GetNoMDEntryTypes())
	syms := checked(m.GetNoRelatedSym())
	if string(sub) != scenario.SubscriptionSnapshot || depth != scenario.MarketDepth || types.Len() != len(scenario.EntryTypes) || syms.Len() != 1 {
		scenario.Anomaly("MarketDataRequest %s: %s of %d levels of %d types of %d symbols", reqID, sub, depth, types.Len(), syms.Len())
		return nil
	}
	w := marketdatasnapshotfullrefresh.New()
	w.SetMDReqID(reqID)
	w.SetSymbol(checked(syms.Get(0).GetSymbol()))
	g := marketdatasnapshotfullrefresh.NewNoMDEntriesRepeatingGroup()
	for k := range types.Len() {
		typ := checked(types.Get(k).GetMDEntryType())
		if string(typ) != scenario.EntryTypes[k] {
			scenario.Anomaly("MarketDataRequest %s: MDEntryType %s, want %s", reqID, typ, scenario.EntryTypes[k])
		}
		for lvl := range depth {
			l := book[k][lvl]
			e := g.Add()
			e.SetMDEntryType(typ)
			e.SetMDEntryPx(l.px.v, l.px.scale)
			e.SetMDEntrySize(l.size.v, l.size.scale)
		}
	}
	w.SetNoMDEntries(g)
	if err := quickfix.SendToTarget(w, id); err != nil {
		scenario.Anomaly("%s: sending MarketDataSnapshotFullRefresh %s: %v", id, reqID, err)
	}
	return nil
}

func acceptor(p scenario.Params) error {
	x := &exchange{app: newApp()}
	x.stopping.Store(true) // the acceptor expects the initiator's logout
	x.AddRoute(newordersingle.Route(x.onNewOrderSingle))
	x.AddRoute(marketdatarequest.Route(x.onMarketDataRequest))
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

// client reads the answers it receives and passes their identifiers to the
// tracker.
type client struct {
	*app
	check scenario.Checker
	tr    *scenario.Tracker
}

func (c *client) onExecutionReport(m executionreport.ExecutionReport, _ quickfix.SessionID) quickfix.MessageRejectError {
	clOrdID := checked(m.GetClOrdID())
	parties := checked(m.GetNoPartyIDs())
	c.check.Parties(parties.Len())
	for i := range parties.Len() {
		p := parties.Get(i)
		c.check.Party(i, checked(p.GetPartyID()), string(checked(p.GetPartyIDSource())), string(checked(p.GetPartyRole())))
	}
	c.tr.Ack(clOrdID)
	return nil
}

func (c *client) onMarketDataSnapshotFullRefresh(m marketdatasnapshotfullrefresh.MarketDataSnapshotFullRefresh, _ quickfix.SessionID) quickfix.MessageRejectError {
	reqID := checked(m.GetMDReqID())
	entries := checked(m.GetNoMDEntries())
	c.check.Entries(entries.Len())
	for i := range min(entries.Len(), 2*scenario.MarketDepth) {
		e := entries.Get(i)
		typ := checked(e.GetMDEntryType())
		px := checked(e.GetMDEntryPx())
		size := checked(e.GetMDEntrySize())
		// Compared as decimals, the values QuickFIX/Go reads.
		k, lvl := i/scenario.MarketDepth, i%scenario.MarketDepth
		if l := book[k][lvl]; string(typ) != scenario.EntryTypes[k] || !px.Equal(l.px.v) || !size.Equal(l.size.v) {
			scenario.Anomaly("NoMDEntries entry %d: %s %v@%v, want %s %v@%v", i, typ, size, px, scenario.EntryTypes[k], l.size.v, l.px.v)
		}
	}
	c.tr.Ack(reqID)
	return nil
}

func initiator(p scenario.Params) error {
	c := &client{app: newApp(), check: scenario.Checker{Scenario: p.Scenario}, tr: scenario.NewTracker(p)}
	// Only the scenario's answers are routed: FromApp reports any other.
	switch p.Scenario {
	case scenario.Orders:
		c.AddRoute(executionreport.Route(c.onExecutionReport))
	case scenario.MarketData:
		c.AddRoute(marketdatasnapshotfullrefresh.Route(c.onMarketDataSnapshotFullRefresh))
	}
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

	sid := quickfix.SessionID{BeginString: scenario.BeginString, SenderCompID: scenario.InitiatorCompID, TargetCompID: scenario.AcceptorCompID}
	var send func(id string) error
	switch p.Scenario {
	case scenario.Orders:
		send = func(id string) error {
			nos := newordersingle.New(field.NewClOrdID(id), field.NewSide(enum.Side_BUY),
				field.NewTransactTime(time.Now()), field.NewOrdType(enum.OrdType_LIMIT))
			nos.SetHandlInst(enum.HandlInst_AUTOEXECPRIV)
			nos.SetOrderQty(orderQty.v, orderQty.scale)
			nos.SetPrice(price.v, price.scale)
			nos.SetSymbol(scenario.Symbol)
			nos.SetTimeInForce(enum.TimeInForce_DAY)
			g := newordersingle.NewNoPartyIDsRepeatingGroup()
			for _, party := range scenario.Parties {
				e := g.Add()
				e.SetPartyID(party.ID)
				e.SetPartyIDSource(enum.PartyIDSource(party.Source))
				e.SetPartyRole(enum.PartyRole(party.Role))
			}
			nos.SetNoPartyIDs(g)
			return quickfix.SendToTarget(nos, sid)
		}
	case scenario.MarketData:
		send = func(id string) error {
			req := marketdatarequest.New(field.NewMDReqID(id),
				field.NewSubscriptionRequestType(enum.SubscriptionRequestType_SNAPSHOT),
				field.NewMarketDepth(scenario.MarketDepth))
			types := marketdatarequest.NewNoMDEntryTypesRepeatingGroup()
			for _, typ := range scenario.EntryTypes {
				types.Add().SetMDEntryType(enum.MDEntryType(typ))
			}
			req.SetNoMDEntryTypes(types)
			syms := marketdatarequest.NewNoRelatedSymRepeatingGroup()
			syms.Add().SetSymbol(scenario.Symbol)
			req.SetNoRelatedSym(syms)
			return quickfix.SendToTarget(req, sid)
		}
	}
	ctl := scenario.NewControl()
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
