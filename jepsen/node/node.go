package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sylr/fix2/contribs/store/sqlite3"
	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store"
	"github.com/sylr/fix2/fix/session/store/memstore"
	"github.com/sylr/fix2/transport/tcp"
)

// Options configures a node.
type Options struct {
	Role        string // "initiator" or "acceptor"
	Listen      string // acceptor: the FIX address
	Connect     string // initiator: the acceptor's FIX address
	Store       string // "memstore", or "sqlite:<path>"
	Events      string // directory of the event logs
	Incarnation string // names this process's event log
	Mutants     []string

	HeartBtInt   time.Duration
	MaxLatency   time.Duration
	StoreTimeout time.Duration
	WriteTimeout time.Duration
	AckProbe     session.AckProbe
	MaxWorkers   int // sends running at once, beyond which a send fails
	// FromAppDelay makes the application slow: the delivery queue fills,
	// and the session records how far it processed 64 messages at a time
	// rather than at each, so a kill has messages to deliver again.
	FromAppDelay time.Duration

	Logger *slog.Logger

	// store, if set, is used instead of the one Store names: for tests.
	store store.Store
}

// Mutants break the node on purpose, to show the checkers catch it.
const (
	mutantNoSync      = "no-sync"      // Sync reports success without syncing
	mutantDropDeliver = "drop-deliver" // every 10th delivery is not logged
	mutantRedeliver   = "redeliver"    // every 10th delivery is logged twice
)

var mutants = []string{mutantNoSync, mutantDropDeliver, mutantRedeliver}

// markerPrefix starts the ClOrdIDs of barrier markers, which the workload's
// sends may not use.
const markerPrefix = "BARRIER-"

// replyGrace is how long after its deadline a send is answered as
// unresolved: a call that returns at its deadline is answered with what it
// returned.
const replyGrace = 250 * time.Millisecond

// staleAfter is the age past which a status snapshot says the session is
// unreachable: the watcher is stuck reading its state.
const staleAfter = time.Second

// Node runs one fix2 session, initiator or acceptor, for the Jepsen suite.
type Node struct {
	opts Options
	ev   *Events
	log  *slog.Logger

	offset  atomic.Int64 // Config.Now's offset from time.Now
	clockMu sync.Mutex   // serializes SetClock

	sess     *session.Session
	run      func(context.Context) error
	listener *tcp.Listener

	workers  chan struct{} // a slot per send running
	inflight sync.WaitGroup

	mu         sync.Mutex
	draining   bool          // no workload send admitted any more
	drained    chan struct{} // closed once the sends admitted before draining returned
	delivered  uint64
	lastSeq    uint64          // MsgSeqNum of the last delivered
	markers    map[string]bool // markers delivered
	unresolved int             // sends answered before their call returned, still running
	markerN    int
	halted     bool

	status atomic.Pointer[Status]
}

// Status is a snapshot of the node, refreshed in the background: reading
// the session's state may wait for a store write that hangs.
type Status struct {
	At         time.Time `json:"at"`
	AgeMS      int64     `json:"age_ms"`
	Stale      bool      `json:"stale"` // older than staleAfter: the session is unreachable
	State      string    `json:"state"`
	Err        string    `json:"err,omitempty"`
	LoggedOn   bool      `json:"logged_on"`
	Delivered  uint64    `json:"delivered"`
	LastSeq    uint64    `json:"last_seq"`
	Markers    []string  `json:"markers"`
	Inflight   int       `json:"inflight"`
	Unresolved int       `json:"unresolved"`
	Draining   bool      `json:"draining"`
}

// NewNode opens the node's event log and store and creates its session.
func NewNode(opts Options) (*Node, error) {
	for _, m := range opts.Mutants {
		if !slices.Contains(mutants, m) {
			return nil, fmt.Errorf("mutant %q: want one of %s", m, strings.Join(mutants, ", "))
		}
	}
	if opts.MaxWorkers <= 0 {
		opts.MaxWorkers = 256
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	ev, err := OpenEvents(opts.Events, opts.Incarnation)
	if err != nil {
		return nil, err
	}
	n := &Node{opts: opts, ev: ev, log: opts.Logger, workers: make(chan struct{}, opts.MaxWorkers), markers: map[string]bool{}}
	n.status.Store(&Status{At: time.Now(), State: "starting"})
	ev.Log(Event{Kind: "start", Role: opts.Role, Store: opts.Store, Mutant: opts.Mutants})

	st := opts.store
	switch {
	case st != nil:
	case opts.Store == "memstore":
		st = memstore.New()
	case strings.HasPrefix(opts.Store, "sqlite:"):
		if st, err = sqlite3.OpenFile(strings.TrimPrefix(opts.Store, "sqlite:"), "jepsen"); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("store %q: want memstore or sqlite:<path>", opts.Store)
	}
	st = wrapStore(st, ev, n.mutant(mutantNoSync))

	id := session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"}
	if opts.Role == "acceptor" {
		id.SenderCompID, id.TargetCompID = id.TargetCompID, id.SenderCompID
	}
	cfg := session.Config{
		ID:           id,
		Store:        st,
		Logger:       opts.Logger,
		Observer:     observer{n},
		Handler:      &handler{n: n},
		HeartBtInt:   opts.HeartBtInt,
		MaxLatency:   opts.MaxLatency,
		StoreTimeout: opts.StoreTimeout,
		WriteTimeout: opts.WriteTimeout,
		AckProbe:     opts.AckProbe,
		Now:          n.now,
	}
	switch opts.Role {
	case "initiator":
		i := session.NewInitiator(session.InitiatorOptions{Logger: opts.Logger, ReconnectMin: 100 * time.Millisecond, ReconnectMax: time.Second})
		n.sess, err = i.Add(cfg, &tcp.Dialer{Addresses: []string{opts.Connect}})
		n.run = i.Run
	case "acceptor":
		a := session.NewAcceptor(session.AcceptorOptions{Logger: opts.Logger})
		if n.sess, err = a.Add(cfg); err != nil {
			break
		}
		if n.listener, err = tcp.Listen(opts.Listen, nil); err != nil {
			break
		}
		a.Listen(n.listener)
		n.run = a.Run
	default:
		err = fmt.Errorf("role %q: want initiator or acceptor", opts.Role)
	}
	if err != nil {
		st.Close()
		return nil, err
	}
	return n, nil
}

// Addr is the acceptor's FIX address.
func (n *Node) Addr() net.Addr { return n.listener.Addr() }

// Run runs the session until ctx is done. The event log stays open: a send
// or a store write may still be running, and logs when it returns.
func (n *Node) Run(ctx context.Context) error {
	go n.watch(ctx)
	return n.run(ctx)
}

func (n *Node) mutant(name string) bool { return slices.Contains(n.opts.Mutants, name) }

// now is the session's clock: time.Now, set apart by SetClock.
func (n *Node) now() time.Time {
	return time.Now().Add(time.Duration(n.offset.Load()))
}

// watch refreshes the status snapshot, and logs the halt once seen.
func (n *Node) watch(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		state, err := n.sess.State(), n.sess.Err()
		n.mu.Lock()
		if err != nil && !n.halted {
			n.halted = true
			// The session's error for a store write past StoreTimeout,
			// unexported: matched by its text.
			n.ev.Log(Event{Kind: "halt", Err: err.Error(), Watchdog: strings.Contains(err.Error(), "longer than StoreTimeout")})
		}
		st := &Status{
			At: time.Now(), State: string(state), LoggedOn: state == session.StateLoggedOn,
			Delivered: n.delivered, LastSeq: n.lastSeq, Inflight: len(n.workers),
			Unresolved: n.unresolved, Draining: n.draining,
		}
		for id := range n.markers {
			st.Markers = append(st.Markers, id)
		}
		n.mu.Unlock()
		if err != nil {
			st.Err = err.Error()
		}
		n.status.Store(st)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Snapshot returns the last status, with its age.
func (n *Node) Snapshot() Status {
	st := *n.status.Load()
	age := time.Since(st.At)
	st.AgeMS, st.Stale = age.Milliseconds(), age > staleAfter
	return st
}

// SendResult is the outcome of a send, as Jepsen records it: "ok" once
// acknowledged, "fail" if the message was certainly not stored, "info"
// otherwise.
type SendResult struct {
	Type  string `json:"type"`
	Gen   uint64 `json:"gen,omitempty"`
	Seq   uint64 `json:"seq,omitempty"`
	Level string `json:"level,omitempty"`
	Err   string `json:"error,omitempty"`
}

var (
	errDraining   = errors.New("draining: no more sends")
	errBusy       = errors.New("too many sends running")
	errReserved   = errors.New("ClOrdID reserved for barrier markers")
	errUnresolved = errors.New("unresolved: the send had not returned at its deadline")
)

// Send sends an order with ClOrdID id and waits for its acknowledgement for
// up to timeout.
func (n *Node) Send(id string, timeout time.Duration) SendResult {
	if strings.HasPrefix(id, markerPrefix) {
		return SendResult{Type: "fail", Err: errReserved.Error()}
	}
	return n.dispatch(id, timeout, false)
}

// Barrier sends a marker, which the counterparty delivers once it has
// delivered everything sent before it, and returns its ClOrdID. It is
// admitted while draining.
func (n *Node) Barrier(timeout time.Duration) (string, SendResult) {
	n.mu.Lock()
	n.markerN++
	id := fmt.Sprintf("%s%s-%d", markerPrefix, n.opts.Incarnation, n.markerN)
	n.mu.Unlock()
	return id, n.dispatch(id, timeout, true)
}

// dispatch runs a send in a worker and answers by its deadline. A store
// write that hangs holds Send and Receipt.Wait past their context: the call
// goes on in its worker, counted unresolved until it returns, and is
// answered "info". Workload sends are not admitted once draining; markers
// are.
func (n *Node) dispatch(id string, timeout time.Duration, marker bool) SendResult {
	n.mu.Lock()
	if n.draining && !marker {
		n.mu.Unlock()
		return SendResult{Type: "fail", Err: errDraining.Error()}
	}
	select {
	case n.workers <- struct{}{}:
	default:
		n.mu.Unlock()
		return SendResult{Type: "fail", Err: errBusy.Error()}
	}
	n.inflight.Add(1)
	n.mu.Unlock()

	deadline := time.Now().Add(timeout)
	const running, returned, abandoned = 0, 1, 2
	var state atomic.Int32
	done := make(chan SendResult, 1)
	go func() {
		defer n.inflight.Done()
		defer func() { <-n.workers }()
		r := n.send(id, deadline)
		n.ev.Log(Event{Kind: "send", Phase: "end", ClOrdID: id, Gen: r.Gen, Seq: r.Seq, Level: r.Level, Type: r.Type, Err: r.Err})
		if !state.CompareAndSwap(running, returned) {
			n.mu.Lock()
			n.unresolved--
			n.mu.Unlock()
		}
		done <- r
	}()
	timer := time.NewTimer(time.Until(deadline) + replyGrace)
	defer timer.Stop()
	select {
	case r := <-done:
		return r
	case <-timer.C:
		if !state.CompareAndSwap(running, abandoned) {
			return <-done
		}
		n.mu.Lock()
		n.unresolved++
		n.mu.Unlock()
		return SendResult{Type: "info", Err: errUnresolved.Error()}
	}
}

func (n *Node) send(id string, deadline time.Time) SendResult {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	n.ev.Log(Event{Kind: "send", Phase: "begin", ClOrdID: id})
	r, err := n.sess.Send(ctx, order(id, n.now()))
	if err != nil {
		res := SendResult{Type: "fail", Err: err.Error()}
		if errors.Is(err, store.ErrUnknownOutcome) || strings.Contains(err.Error(), "longer than StoreTimeout") {
			// Halted on the store: perhaps stored.
			res.Type = "info"
		}
		return res
	}
	res := SendResult{Type: "info", Gen: r.Generation(), Seq: r.Seq(), Level: "accepted"}
	if err := r.Wait(ctx, session.Written); err != nil {
		res.Err = err.Error()
		return res
	}
	res.Level = "written"
	if err := r.Wait(ctx, session.Acknowledged); err != nil {
		res.Err = err.Error()
		return res
	}
	res.Type, res.Level = "ok", "acknowledged"
	return res
}

// Drain refuses workload sends from now on, and waits up to timeout for
// those admitted before to return.
func (n *Node) Drain(timeout time.Duration) bool {
	n.mu.Lock()
	n.draining = true
	if n.drained == nil {
		n.drained = make(chan struct{})
		go func() { n.inflight.Wait(); close(n.drained) }()
	}
	drained := n.drained
	n.mu.Unlock()
	select {
	case <-drained:
		return true
	case <-time.After(timeout):
		return false
	}
}

// SetClock sets Config.Now's offset, logged first: a frame the session dated
// with the old offset and the observer with the new one is logged after.
func (n *Node) SetClock(offset time.Duration) {
	n.clockMu.Lock()
	defer n.clockMu.Unlock()
	n.ev.Log(Event{Kind: "clock", WasMS: time.Duration(n.offset.Load()).Milliseconds(), OffsetMS: offset.Milliseconds()})
	n.offset.Store(int64(offset))
}

// handler logs what the session hands the application.
type handler struct {
	session.BaseHandler
	n      *Node
	nApply atomic.Uint64
}

func (h *handler) FromApp(_ context.Context, _ *session.Session, m *fix.Message, in session.Inbound) error {
	n := h.n
	if d := n.opts.FromAppDelay; d > 0 {
		time.Sleep(d)
	}
	id := string(m.Get(tagClOrdID))
	ev := Event{Kind: "deliver", Gen: in.Generation, Seq: in.Seq, ClOrdID: id, PossDup: in.PossDup, Digest: digest(m)}
	times := 1
	if k := h.nApply.Add(1); k%10 == 0 {
		switch {
		case n.mutant(mutantDropDeliver):
			times = 0
		case n.mutant(mutantRedeliver):
			times = 2
		}
	}
	for range times {
		n.ev.Log(ev)
	}
	n.mu.Lock()
	n.delivered++
	n.lastSeq = in.Seq
	if strings.HasPrefix(id, markerPrefix) {
		n.markers[id] = true
	}
	n.mu.Unlock()
	return nil
}

func (h *handler) OnLogon(context.Context, *session.Session) {
	h.n.ev.Log(Event{Kind: "logon"})
}

func (h *handler) OnLogout(_ context.Context, _ *session.Session, reason error) {
	ev := Event{Kind: "logout"}
	if reason != nil {
		ev.Err = reason.Error()
	}
	h.n.ev.Log(ev)
}

func (h *handler) OnRecoveryLoss(_ context.Context, _ *session.Session, begin, end uint64, cause error) {
	ev := Event{Kind: "recovery-loss", Begin: begin, End: end}
	if cause != nil {
		ev.Err = cause.Error()
	}
	h.n.ev.Log(ev)
}

// observer logs every frame the session reads or writes, synchronously:
// AsyncObserver drops frames when it falls behind.
type observer struct{ n *Node }

func (o observer) OnInbound(_ *session.Session, f session.Frame) {
	o.n.ev.Log(frameEvent("in", f, o.n.now()))
}

func (o observer) OnOutbound(_ *session.Session, f session.Frame) {
	o.n.ev.Log(frameEvent("out", f, o.n.now()))
}

// OnRejected logs a message the session rejected, and whether it counted it
// as received, never to be delivered.
func (o observer) OnRejected(_ *session.Session, r session.Rejected) {
	o.n.ev.Log(Event{Kind: "rejected", Gen: r.Generation, Seq: r.Seq, RejectReason: strconv.Itoa(int(r.Reason)), Counted: r.Counted})
}
