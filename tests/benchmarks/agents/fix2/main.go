// Command fix2 is the benchmark agent of fix2: the acceptor or the
// initiator of the scenario (see package scenario), with fix2's defaults but
// for what the scenario sets.
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
// logging, the dictionary of p if any.
func config(p scenario.Params, sender, target string, h session.Handler) (session.Config, error) {
	cfg := session.Config{
		ID:           session.SessionID{BeginString: scenario.BeginString, SenderCompID: sender, TargetCompID: target},
		Store:        memstore.New(),
		Handler:      h,
		HeartBtInt:   scenario.HeartBtInt * time.Second,
		ResetOnLogon: true,
	}
	if p.Dict != "" {
		d, err := xmldict.LoadFile(p.Dict)
		if err != nil {
			return cfg, err
		}
		cfg.Dictionaries.Session = d
	}
	return cfg, nil
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

func (w *watcher) unexpected(s *session.Session, m *fix2.Message) error {
	scenario.Anomaly("%s received MsgType %s: %s", s.ID(), m.MsgType(), m.Get(58))
	return fix2.ErrUnsupportedMessageType
}

func (w *watcher) OnLogout(_ context.Context, s *session.Session, reason error) {
	if !w.stopping.Load() {
		scenario.Anomaly("%s logged out: %v", s.ID(), reason)
	}
}

// checkRejects reports the Rejects s sent.
func checkRejects(s *session.Session) {
	if n := s.Stats().RejectsSent.Load(); n > 0 {
		scenario.Anomaly("%s sent %d Rejects", s.ID(), n)
	}
}

// exchange acknowledges each NewOrderSingle with an ExecutionReport.
type exchange struct {
	watcher
	// FromApp runs on one goroutine: its buffers are reused.
	n  int
	er *fix2.Builder
	id []byte
	ts []byte
}

func (x *exchange) OnLogout(context.Context, *session.Session, error) {}

func (x *exchange) FromApp(ctx context.Context, s *session.Session, m *fix2.Message, _ session.Inbound) error {
	if string(m.MsgType()) != "D" {
		return x.unexpected(s, m)
	}
	x.n++
	x.id = strconv.AppendInt(x.id[:0], int64(x.n), 10)
	x.ts = fix2.AppendUTCTimestamp(x.ts[:0], time.Now(), fix2.Milliseconds)
	qty := m.Get(38)
	er := x.er
	er.Reset()
	// In ascending tag order, as QuickFIX engines write them.
	er.SetString(6, scenario.Zero).
		Set(11, m.Get(11)).
		SetString(14, scenario.Zero).
		Set(17, x.id).
		Set(37, x.id).
		Set(38, qty).
		SetString(39, scenario.OrdStatusNew).
		Set(44, m.Get(44)).
		Set(54, m.Get(54)).
		Set(55, m.Get(55)).
		Set(60, x.ts).
		SetString(150, scenario.ExecTypeNew).
		Set(151, qty)
	// A Send from a callback never waits: the window bounds what is queued.
	_, err := s.Send(ctx, er)
	return err
}

func acceptor(p scenario.Params) error {
	x := &exchange{er: fix2.NewBuilder(scenario.BeginString, "8")}
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

// client passes the ExecutionReports it receives to the tracker.
type client struct {
	watcher
	tr     *scenario.Tracker
	logons chan struct{}
}

func (c *client) OnLogon(context.Context, *session.Session) {
	select {
	case c.logons <- struct{}{}:
	default:
	}
}

func (c *client) FromApp(_ context.Context, s *session.Session, m *fix2.Message, _ session.Inbound) error {
	if string(m.MsgType()) != "8" {
		return c.unexpected(s, m)
	}
	c.tr.Ack(m.Get(11))
	return nil
}

func initiator(p scenario.Params) error {
	c := &client{tr: scenario.NewTracker(p), logons: make(chan struct{}, 1)}
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

	ctl := scenario.NewControl()
	nos := fix2.NewBuilder(scenario.BeginString, "D")
	var ts []byte
	send := func(clOrdID []byte) error {
		ts = fix2.AppendUTCTimestamp(ts[:0], time.Now(), fix2.Milliseconds)
		nos.Reset()
		nos.Set(11, clOrdID).
			SetString(21, scenario.HandlInst).
			SetString(38, scenario.OrderQty).
			SetString(40, scenario.OrdType).
			SetString(44, scenario.Price).
			SetString(54, scenario.Side).
			SetString(55, scenario.Symbol).
			SetString(59, scenario.TimeInForce).
			Set(60, ts)
		_, err := s.Send(ctx, nos)
		return err
	}
	if err := c.tr.Run(ctl, send); err != nil {
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
