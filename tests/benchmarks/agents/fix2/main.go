// Command fix2 is the benchmark agent of fix2: the acceptor or the
// initiator of the scenarios (see package scenario), with fix2's defaults
// but for what the scenarios set. It builds and reads every message with
// the code fixgen generates from the dictionary (gen/fix2/fix44), and
// dispatches those it receives with its generated Dispatch.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary/xmldict"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/store/memstore"
	"github.com/sylr/fix2/session/transport/tcp"
	"github.com/sylr/fix2/tests/benchmarks/gen/fix2/fix44"
	"github.com/sylr/fix2/tests/benchmarks/internal/scenario"
)

func main() {
	p, err := scenario.ParseArgs(os.Args[1:])
	if err == nil {
		switch p.Role {
		case "version":
			fmt.Println(scenario.Version("github.com/sylr/fix2"))
			return
		case "acceptor":
			err = acceptor(p)
		case "initiator":
			err = initiator(p)
		}
	}
	scenario.Exit(err)
}

// config returns the session's configuration: an in-memory store, no
// logging, the dictionary of p.
func config(p scenario.Params, sender, target string, h session.Handler) (session.Config, error) {
	d, err := xmldict.LoadFile(p.Dict)
	if err != nil {
		return session.Config{}, err
	}
	return session.Config{
		ID:           session.SessionID{BeginString: scenario.BeginString, SenderCompID: sender, TargetCompID: target},
		Dictionaries: session.Dictionaries{Session: d},
		Store:        memstore.New(),
		Handler:      h,
		HeartBtInt:   scenario.HeartBtInt * time.Second,
		ResetOnLogon: true,
	}, nil
}

// watcher reports the Rejects and unexpected messages a session receives.
type watcher struct {
	session.BaseHandler
	stopping atomic.Bool // the agent logs out: a logout is expected
}

func (w *watcher) FromAdmin(_ context.Context, s *session.Session, m *fix2.Message, _ session.Inbound) {
	switch string(m.MsgType()) {
	case "3":
		scenario.Anomaly("%s received a Reject: RefSeqNum %s, %s", s.ID(), m.Get(45), m.Get(58))
	case "2", "4":
		// On the loopback, recovering a gap means a message was lost.
		scenario.Anomaly("%s received MsgType %s: sequence recovery", s.ID(), m.MsgType())
	}
}

func (w *watcher) OnRecoveryLoss(_ context.Context, s *session.Session, begin, end uint64, cause error) {
	scenario.Anomaly("%s lost messages %d to %d: %v", s.ID(), begin, end, cause)
}

func (w *watcher) OnLogout(_ context.Context, s *session.Session, reason error) {
	if !w.stopping.Load() {
		scenario.Anomaly("%s logged out: %v", s.ID(), reason)
	}
}

// dispatch dispatches m to h, reporting a message of a type h does not
// handle.
func dispatch(s *session.Session, m *fix2.Message, h fix44.Handler) error {
	err := fix44.Dispatch(m, h)
	if errors.Is(err, fix2.ErrUnsupportedMessageType) {
		scenario.Anomaly("%s received MsgType %s: %s", s.ID(), m.MsgType(), m.Get(58))
	}
	return err
}

// checkRejects reports the Rejects s sent.
func checkRejects(s *session.Session) {
	if n := s.Stats().RejectsSent.Load(); n > 0 {
		scenario.Anomaly("%s sent %d Rejects", s.ID(), n)
	}
}

// checked reports err, a getter's, as an anomaly.
func checked[T any](v T, _ bool, err error) T {
	if err != nil {
		scenario.Anomaly("%v", err)
	}
	return v
}

// exchange is the acceptor's session handler: it dispatches the
// application messages to its exchangeApp.
type exchange struct {
	watcher
	app *exchangeApp
}

func (x *exchange) OnLogout(context.Context, *session.Session, error) {}

func (x *exchange) FromApp(ctx context.Context, s *session.Session, m *fix2.Message, _ session.Inbound) error {
	x.app.ctx, x.app.s = ctx, s
	return dispatch(s, m, x.app)
}

// exchangeApp answers each NewOrderSingle with an ExecutionReport, each
// MarketDataRequest with a MarketDataSnapshotFullRefresh. Its handlers run
// on the session's one delivery goroutine: the builders, and the group
// entries they reuse, are its own.
type exchangeApp struct {
	fix44.UnimplementedHandler
	ctx context.Context
	s   *session.Session

	n       int
	er      *fix44.ExecutionReportBuilder
	parties []*fix44.NoPartyIDsEntryBuilder
	w       *fix44.MarketDataSnapshotFullRefreshBuilder
	entries []*fix44.NoMDEntriesEntryBuilder
}

func (x *exchangeApp) OnNewOrderSingle(m *fix44.NewOrderSingle) error {
	clOrdID, _ := m.ClOrdID()
	symbol, _ := m.Symbol()
	side, _ := m.Side()
	qty := checked(m.OrderQty())
	px := checked(m.Price())
	x.n++
	id := strconv.Itoa(x.n)
	er := x.er
	er.Reset()
	er.SetOrderID(id).
		SetExecID(id).
		SetExecType(fix44.ExecTypeNew).
		SetOrdStatus(fix44.OrdStatusNew).
		SetSide(side).
		SetLeavesQty(qty).
		SetCumQty(0).
		SetAvgPx(0).
		SetClOrdID(clOrdID).
		SetOrderQty(qty).
		SetPrice(px).
		SetSymbol(symbol).
		SetTransactTime(time.Now())
	for i, p := range m.NoPartyIDsEntries() {
		if i == len(x.parties) {
			x.parties = append(x.parties, fix44.NewNoPartyIDsEntry())
		}
		pid, _ := p.PartyID()
		src, _ := p.PartyIDSource()
		role, _ := p.PartyRole()
		er.AddNoPartyIDsEntry(x.parties[i].SetPartyID(pid).SetPartyIDSource(src).SetPartyRole(role))
	}
	// A Send from a callback never waits: the window bounds what is queued.
	_, err := x.s.Send(x.ctx, er)
	return err
}

func (x *exchangeApp) OnMarketDataRequest(m *fix44.MarketDataRequest) error {
	reqID, _ := m.MDReqID()
	sub, _ := m.SubscriptionRequestType()
	depth := checked(m.MarketDepth())
	types := m.NoMDEntryTypesEntries()
	syms := m.NoRelatedSymEntries()
	if sub != scenario.SubscriptionSnapshot || depth != scenario.MarketDepth || len(types) != len(scenario.EntryTypes) || len(syms) != 1 {
		scenario.Anomaly("MarketDataRequest %s: %s of %d levels of %d types of %d symbols", reqID, sub, depth, len(types), len(syms))
		return nil
	}
	symbol, _ := syms[0].Symbol()
	w := x.w
	w.Reset()
	w.SetMDReqID(reqID).SetSymbol(symbol)
	for k, t := range types {
		typ, _ := t.MDEntryType()
		if string(typ) != scenario.EntryTypes[k] {
			scenario.Anomaly("MarketDataRequest %s: MDEntryType %s, want %s", reqID, typ, scenario.EntryTypes[k])
		}
		for lvl := range depth {
			i := k*depth + lvl
			if i == len(x.entries) {
				x.entries = append(x.entries, fix44.NewNoMDEntriesEntry())
			}
			l := scenario.Book[k][lvl]
			w.AddNoMDEntriesEntry(x.entries[i].SetMDEntryType(typ).SetMDEntryPx(l.Px).SetMDEntrySize(l.Size))
		}
	}
	_, err := x.s.Send(x.ctx, w)
	return err
}

func acceptor(p scenario.Params) error {
	x := &exchange{app: &exchangeApp{
		er: fix44.BuildExecutionReport(scenario.BeginString),
		w:  fix44.BuildMarketDataSnapshotFullRefresh(scenario.BeginString),
	}}
	cfg, err := config(p, scenario.AcceptorCompID, scenario.InitiatorCompID, x)
	if err != nil {
		return err
	}
	a := session.NewAcceptor(session.AcceptorOptions{})
	s, err := a.Add(cfg)
	if err != nil {
		return err
	}
	l, err := tcp.Listen(p.Addr(), nil)
	if err != nil {
		return err
	}
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runErr error
	wg.Go(func() { runErr = a.Run(ctx) })
	err = scenario.ServeAcceptor(scenario.NewControl())
	cancel()
	wg.Wait()
	checkRejects(s)
	return errors.Join(err, runErr)
}

// client is the initiator's session handler: it dispatches the application
// messages to its clientApp.
type client struct {
	watcher
	app    *clientApp
	logons chan struct{}
}

func (c *client) OnLogon(context.Context, *session.Session) {
	select {
	case c.logons <- struct{}{}:
	default:
	}
}

func (c *client) FromApp(_ context.Context, s *session.Session, m *fix2.Message, _ session.Inbound) error {
	return dispatch(s, m, c.app)
}

// clientApp reads the answers it receives and passes their identifiers to
// the tracker.
type clientApp struct {
	fix44.UnimplementedHandler
	check scenario.Checker
	tr    *scenario.Tracker
}

func (c *clientApp) OnExecutionReport(m *fix44.ExecutionReport) error {
	if !c.check.Answers(scenario.Orders) {
		return fix2.ErrUnsupportedMessageType
	}
	clOrdID, _ := m.ClOrdID()
	parties := m.NoPartyIDsEntries()
	c.check.Parties(len(parties))
	for i, p := range parties {
		pid, _ := p.PartyID()
		src, _ := p.PartyIDSource()
		role, _ := p.PartyRole()
		c.check.Party(i, pid, string(src), string(role))
	}
	c.tr.Ack(clOrdID)
	return nil
}

func (c *clientApp) OnMarketDataSnapshotFullRefresh(m *fix44.MarketDataSnapshotFullRefresh) error {
	if !c.check.Answers(scenario.MarketData) {
		return fix2.ErrUnsupportedMessageType
	}
	reqID, _ := m.MDReqID()
	entries := m.NoMDEntriesEntries()
	c.check.Entries(len(entries))
	for i, e := range entries {
		typ, _ := e.MDEntryType()
		px := checked(e.MDEntryPx())
		size := checked(e.MDEntrySize())
		c.check.Entry(i, string(typ), px, size)
	}
	c.tr.Ack(reqID)
	return nil
}

func initiator(p scenario.Params) error {
	c := &client{app: &clientApp{check: scenario.Checker{Scenario: p.Scenario}, tr: scenario.NewTracker(p)}, logons: make(chan struct{}, 1)}
	cfg, err := config(p, scenario.InitiatorCompID, scenario.AcceptorCompID, c)
	if err != nil {
		return err
	}
	i := session.NewInitiator(session.InitiatorOptions{})
	s, err := i.Add(cfg, &tcp.Dialer{Addresses: []string{p.Addr()}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runErr error
	wg.Go(func() { runErr = i.Run(ctx) })
	// Stopping the initiator logs it out.
	stop := sync.OnceFunc(func() {
		c.stopping.Store(true)
		cancel()
		wg.Wait()
	})
	defer stop()
	select {
	case <-c.logons:
	case <-time.After(10 * time.Second):
		return errors.New("timed out waiting for the logon")
	}

	var send func(id string) error
	switch p.Scenario {
	case scenario.Orders:
		nos := fix44.BuildNewOrderSingle(scenario.BeginString)
		parties := make([]*fix44.NoPartyIDsEntryBuilder, len(scenario.Parties))
		for k := range parties {
			parties[k] = fix44.NewNoPartyIDsEntry()
		}
		send = func(id string) error {
			nos.Reset()
			nos.SetClOrdID(id).
				SetHandlInst(fix44.HandlInstAutoexecpriv).
				SetOrderQty(scenario.OrderQty).
				SetOrdType(fix44.OrdTypeLimit).
				SetPrice(scenario.Price).
				SetSide(fix44.SideBuy).
				SetSymbol(scenario.Symbol).
				SetTimeInForce(fix44.TimeInForceDay).
				SetTransactTime(time.Now())
			for k, party := range scenario.Parties {
				nos.AddNoPartyIDsEntry(parties[k].
					SetPartyID(party.ID).
					SetPartyIDSource(fix44.PartyIDSource(party.Source)).
					SetPartyRole(fix44.PartyRole(party.Role)))
			}
			_, err := s.Send(ctx, nos)
			return err
		}
	case scenario.MarketData:
		req := fix44.BuildMarketDataRequest(scenario.BeginString)
		types := make([]*fix44.NoMDEntryTypesEntryBuilder, len(scenario.EntryTypes))
		for k := range types {
			types[k] = fix44.NewNoMDEntryTypesEntry()
		}
		sym := fix44.NewNoRelatedSymEntry()
		send = func(id string) error {
			req.Reset()
			req.SetMDReqID(id).
				SetSubscriptionRequestType(fix44.SubscriptionRequestTypeSnapshot).
				SetMarketDepth(scenario.MarketDepth)
			for k, typ := range scenario.EntryTypes {
				req.AddNoMDEntryTypesEntry(types[k].SetMDEntryType(fix44.MDEntryType(typ)))
			}
			req.AddNoRelatedSymEntry(sym.SetSymbol(scenario.Symbol))
			_, err := s.Send(ctx, req)
			return err
		}
	}
	ctl := scenario.NewControl()
	if err := c.app.tr.Run(ctl, send); err != nil {
		return err
	}
	// The driver closes stdin once it has the result: then log out.
	for {
		if _, ok := ctl.Next(); !ok {
			break
		}
	}
	stop()
	checkRejects(s)
	return runErr
}
