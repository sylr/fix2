package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store"
	"github.com/sylr/fix2/sofh"
)

var errLost = errors.New("fixp/session: messages lost, not skipped")

var (
	// ErrQueueFull is a Send from a callback that found no room in the
	// queue: a callback must not wait for the writer.
	ErrQueueFull = errors.New("fixp/session: send queue full")
	// ErrUnnegotiated is a Send on a client's session not negotiated yet:
	// its ID, and so the session its messages would belong to, is not
	// settled.
	ErrUnnegotiated = errors.New("fixp/session: session not negotiated")
	// ErrStopped is a call on a session its Client or Server stopped
	// running, or that halted (Session.Err tells why).
	ErrStopped = errors.New("fixp/session: session stopped")
	// ErrSchema is a Send of a message of the FIXP schema, or not an SBE
	// message: its header is too short.
	ErrSchema = errors.New("fixp/session: not an application message")
	// ErrTooLarge is a Send of a message whose frame is longer than
	// Config.MaxPendingBytes: it would never fit the queue.
	ErrTooLarge = errors.New("fixp/session: message too large")
)

// State is a session's state, as Session.State reports it.
type State string

const (
	// StateUnnegotiated: a client's session before its negotiation
	// succeeded; its ID may still change (§7.1.7.5).
	StateUnnegotiated State = "unnegotiated"
	// StateNegotiating: Negotiate sent or received, its answer awaited or
	// being decided.
	StateNegotiating State = "negotiating"
	// StateUnbound: negotiated, bound to no connection.
	StateUnbound State = "unbound"
	// StateEstablishing: Establish sent or received, its answer awaited or
	// being decided.
	StateEstablishing State = "establishing"
	// StateEstablished: bound to a connection, flows running.
	StateEstablished State = "established"
	// StateTerminating: a Terminate sent, its answer awaited.
	StateTerminating State = "terminating"
	// StateFinalizing: both flows finished and the Terminate exchanged, the
	// finalization being recorded: no establishment any more.
	StateFinalizing State = "finalizing"
	// StateFinalized: both flows finished, or the session retired; it is
	// over.
	StateFinalized State = "finalized"
	// StateStopped: its Client or Server stopped running it, or it halted.
	StateStopped State = "stopped"
)

// Outgoing is an application message to send: the codecs fix2gen sbe
// generates are. AppendTo appends the SBE message whole, its header
// included.
type Outgoing interface {
	AppendTo(dst []byte) ([]byte, error)
}

// Stats counts a session's traffic, across its connections.
type Stats struct {
	Establishments   atomic.Uint64 // connections it was established on
	MessagesSent     atomic.Uint64 // application messages written
	MessagesReceived atomic.Uint64 // application messages read
	// MessagesRetransmitted counts the messages written again, at the
	// peer's request.
	MessagesRetransmitted atomic.Uint64
	HeartbeatsSent        atomic.Uint64
	BytesSent             atomic.Uint64 // frames written, headers included
	BytesReceived         atomic.Uint64 // frames read, headers included
}

// callbackKey marks a context passed to a Handler callback.
type callbackKey struct{}

func inCallback(ctx context.Context) bool {
	return ctx != nil && ctx.Value(callbackKey{}) != nil
}

// Session is a FIXP session, of a Client or a Server.
type Session struct {
	cfg     Config
	role    store.Role
	handler Handler
	log     *slog.Logger
	cbCtx   context.Context
	q       *queue
	stats   Stats
	// assign, a semaphore, serializes the assignment, storing and
	// queueing of the messages sent: held for a store call, never while
	// waiting for room, so that the reader may send NotApplied while a Send
	// waits.
	assign chan struct{}

	done     chan struct{}
	doneOnce sync.Once
	// synced is the highest sequence number assigned when the store was
	// last synced: every message up to it is durable.
	synced atomic.Uint64
	// written is the highest sequence number written on a connection, or
	// stored before the process started: what the peer may ask for again.
	written atomic.Uint64

	mu      sync.Mutex
	id      fixp.SessionID
	owner   string
	state   State
	out, in fixp.FlowType
	st      store.Session // nil until negotiated
	conn    *conn
	err     error
	// outFin and inFin are the flows' finalization, as stored, changed
	// under finishMu.
	outFin, inFin store.Finish

	finishMu sync.Mutex
	// onFinalized and onUnbound, if set, tell the session's Server.
	onFinalized, onUnbound func(*Session)

	// inbound is the store's State.Inbound, owned by the reader of the
	// session's connection, of which there is one at a time.
	inbound uint64
	// evicted is the sequence number messages are evicted below, the
	// writer's, of which there is one at a time.
	evicted uint64
}

func newSession(cfg Config, role store.Role) *Session {
	s := &Session{
		cfg:     cfg,
		role:    role,
		handler: cfg.Handler,
		log:     cfg.Logger,
		q:       newQueue(),
		done:    make(chan struct{}),
		assign:  make(chan struct{}, 1),
	}
	s.cbCtx = context.WithValue(context.Background(), callbackKey{}, s)
	return s
}

// adopt makes s the session st keeps.
func (s *Session) adopt(st store.Session) {
	state := st.State()
	s.mu.Lock()
	s.st = st
	s.id = state.ID
	s.owner = state.Owner
	if s.role == store.Client {
		s.out, s.in = state.ClientFlow, state.ServerFlow
	} else {
		s.out, s.in = state.ServerFlow, state.ClientFlow
	}
	s.state = StateUnbound
	s.outFin, s.inFin = state.OutboundFinish, state.InboundFinish
	s.mu.Unlock()
	s.inbound = state.Inbound
	s.q.setNext(state.NextOutbound)
	s.synced.Store(state.NextOutbound - 1)
	// Stored before the process started: written, or to be retransmitted.
	s.written.Store(state.NextOutbound - 1)
}

func (s *Session) logAttrs(args []any) []any {
	return append([]any{"fixp_session", s.ID().String(), "role", s.role.String()}, args...)
}

func (s *Session) logInfo(msg string, args ...any) {
	if s.log != nil {
		s.log.Info(msg, s.logAttrs(args)...)
	}
}

func (s *Session) logWarn(msg string, args ...any) {
	if s.log != nil {
		s.log.Warn(msg, s.logAttrs(args)...)
	}
}

// ID returns the session's ID. A client's may change until it is
// negotiated.
func (s *Session) ID() fixp.SessionID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// Role returns which side of the session we are.
func (s *Session) Role() store.Role { return s.role }

// Owner returns, on a server, who negotiated the session (see
// ServerConfig.Authenticate); empty on a client.
func (s *Session) Owner() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner
}

// State returns the session's state.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) setState(st State) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

// Flows returns our flow and the peer's, as negotiated; a client's before
// negotiation, the ones it asks for and None.
func (s *Session) Flows() (outbound, inbound fixp.FlowType) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out, s.in
}

// NextSeqNo returns the sequence number the next message sent will take on
// a sequenced outbound flow.
func (s *Session) NextSeqNo() uint64 { return s.q.nextSeq() }

// Send sends an application message: an SBE message of any schema but
// FIXP's, whole (Outgoing). Sends are serialized: the order they return in
// is the order of their sequence numbers.
//
// Send first waits for room in the queue (Config.MaxPendingMessages),
// until ctx is done; from a callback, it does not wait, and fails with
// ErrQueueFull. Then the message is the flow's: it takes the next sequence
// number (0 on an Unsequenced flow), and on a Recoverable flow is stored,
// on an Idempotent one its sequence number is, ctx no longer withdrawing
// it, the store bounded by Config.StoreTimeout. Send returns its sequence
// number once it is queued for writing on the connection the session is
// established on, or the next one: it does not wait for one. If the store
// fails, the session halts, and Send returns the store's error: whether
// the message was stored, the store tells when the session is opened
// again.
//
// It fails with ErrUnnegotiated on a client's session not negotiated yet,
// fixp.ErrNoFlow on a None flow or one finishing or finished, ErrSchema
// for a message of the FIXP schema, ErrStopped once the session stopped.
func (s *Session) Send(ctx context.Context, m Outgoing) (uint64, error) {
	f, err := frame(m)
	if err != nil {
		return 0, err
	}
	if err := checkApplication(f[sofh.HeaderLen:]); err != nil {
		return 0, err
	}
	return s.send(ctx, f)
}

// SendRaw is Send for a message already encoded, borrowed for the call.
func (s *Session) SendRaw(ctx context.Context, msg []byte) (uint64, error) {
	if err := checkApplication(msg); err != nil {
		return 0, err
	}
	f, err := sofh.AppendFrame(make([]byte, 0, sofh.HeaderLen+len(msg)), sofh.SBE10LittleEndian, msg)
	if err != nil {
		return 0, err
	}
	return s.send(ctx, f)
}

// SendApplied sends Applied (§4.8.1), telling the peer its messages from
// to from+count-1 were applied: an application message of our flow, which
// must be sequenced.
func (s *Session) SendApplied(ctx context.Context, from uint64, count uint32) (uint64, error) {
	if out, _ := s.Flows(); !out.Sequenced() {
		return 0, fmt.Errorf("%w: Applied on an %s flow", fixp.ErrNoFlow, out)
	}
	return s.send(ctx, mustFrame(&codec.Applied{FromSeqNo: from, Count: count}))
}

// checkApplication checks msg is an SBE message of another schema than
// FIXP's.
func checkApplication(msg []byte) error {
	if len(msg) < codec.HeaderSize {
		return fmt.Errorf("%w: %d bytes, shorter than a header", ErrSchema, len(msg))
	}
	if h := readHeader(msg); h.SchemaID == codec.SchemaID {
		return fmt.Errorf("%w: of the FIXP schema (template %d)", ErrSchema, h.TemplateID)
	}
	return nil
}

// send queues frame f of an application message.
func (s *Session) send(ctx context.Context, f []byte) (uint64, error) {
	cb := inCallback(ctx)
	s.mu.Lock()
	state, out := s.state, s.out
	s.mu.Unlock()
	switch {
	case state == StateStopped || state == StateFinalized:
		return 0, ErrStopped
	case state == StateUnnegotiated || state == StateNegotiating:
		return 0, ErrUnnegotiated
	case out == fixp.None:
		return 0, fixp.ErrNoFlow
	case len(f) > s.cfg.MaxPendingBytes:
		return 0, fmt.Errorf("%w: a frame of %d bytes, MaxPendingBytes %d", ErrTooLarge, len(f), s.cfg.MaxPendingBytes)
	}
	for {
		// Finishing, Send waits no more: Finish wakes it.
		if s.outFinishing() {
			return 0, fmt.Errorf("%w: finishing", fixp.ErrNoFlow)
		}
		// The assignment is taken as the queue's room is: a callback
		// never waits for it, another Send waits as long as its ctx.
		if cb {
			select {
			case s.assign <- struct{}{}:
			default:
				return 0, ErrQueueFull
			}
		} else {
			select {
			case s.assign <- struct{}{}:
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-s.done:
				return 0, ErrStopped
			}
		}
		// Again under the assignment, which Finish holds: finishing since
		// the check above, no room will come.
		if s.outFinishing() {
			<-s.assign
			return 0, fmt.Errorf("%w: finishing", fixp.ErrNoFlow)
		}
		ok, room := s.q.hasRoom(len(f), s.cfg.MaxPendingMessages, s.cfg.MaxPendingBytes)
		if ok {
			seq, err := s.enqueue(f)
			<-s.assign
			return seq, err
		}
		<-s.assign
		if cb {
			return 0, ErrQueueFull
		}
		select {
		case <-room:
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-s.done:
			return 0, ErrStopped
		}
	}
}

// errInternalFull is a message of the session's own accord that the queue
// cannot take: the peer makes it send more than it writes.
var errInternalFull = errors.New("fixp/session: send queue full of the session's own messages")

// sendInternal queues a message the session sends of its own accord
// (NotApplied), beyond the queue's bounds, which a Send may hold, up to
// twice them: past, the peer is making it send faster than it writes.
func (s *Session) sendInternal(f []byte) (uint64, error) {
	s.assign <- struct{}{}
	defer func() { <-s.assign }()
	if s.q.pending() >= 2*s.cfg.MaxPendingMessages {
		return 0, errInternalFull
	}
	return s.enqueue(f)
}

// enqueue assigns f its sequence number, stores it, and queues it, under
// s.assign.
func (s *Session) enqueue(f []byte) (uint64, error) {
	s.mu.Lock()
	state, out, st, finishing := s.state, s.out, s.st, s.outFin.Finishing
	s.mu.Unlock()
	switch {
	case state == StateStopped || state == StateFinalized:
		return 0, ErrStopped
	case finishing:
		// FinishedSending is out, or queued: nothing after (§4.7, §6.3).
		return 0, fmt.Errorf("%w: finishing", fixp.ErrNoFlow)
	}
	var seq uint64
	if out.Sequenced() {
		seq = s.q.nextSeq()
		sctx, cancel := s.storeCtx()
		var err error
		if out == fixp.Recoverable {
			err = st.Append(sctx, seq, f[sofh.HeaderLen:])
		} else {
			err = st.SetNextOutbound(sctx, seq+1)
		}
		cancel()
		if err != nil {
			err = fmt.Errorf("fixp/session: storing message %d: %w", seq, err)
			s.halt(err)
			return 0, err
		}
	}
	s.q.pushApp(f, seq)
	return seq, nil
}

// syncTo makes the store durable up to sequence number seq at least.
func (s *Session) syncTo(seq uint64) error {
	if seq <= s.synced.Load() {
		return nil
	}
	assigned := s.q.nextSeq() - 1 // stored before being assigned
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	ctx, cancel := s.storeCtx()
	defer cancel()
	if err := st.Sync(ctx); err != nil {
		err = fmt.Errorf("fixp/session: syncing the store: %w", err)
		s.halt(err)
		return err
	}
	s.synced.Store(assigned)
	return nil
}

// retain evicts the messages of a Recoverable flow written before the last
// MaxStoredMessages, once a tenth of it more were written: the writer's,
// written being the highest sequence number out of the queue.
func (s *Session) retain(written uint64) {
	n := uint64(s.cfg.MaxStoredMessages)
	if n == 0 || written <= n {
		return
	}
	if out, _ := s.Flows(); out != fixp.Recoverable {
		return
	}
	below := written - n + 1
	if below < s.evicted+max(1, n/10) {
		return
	}
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	ctx, cancel := s.storeCtx()
	defer cancel()
	if err := st.Evict(ctx, below); err != nil {
		// Kept longer: a retention target, not a durability promise.
		s.logWarn("evicting stored messages", "below", below, "err", err)
		return
	}
	s.evicted = below
}

// setInbound records the inbound position, durably if sync.
func (s *Session) setInbound(seq uint64, sync bool) error {
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	ctx, cancel := s.storeCtx()
	defer cancel()
	err := st.SetInbound(ctx, seq)
	if err == nil && sync {
		err = st.Sync(ctx)
	}
	if err != nil {
		err = fmt.Errorf("fixp/session: recording message %d received: %w", seq, err)
		s.halt(err)
		return err
	}
	s.inbound = seq
	return nil
}

// Terminate unbinds the session from its connection: Terminate Finished
// with reason, after the messages queued before it, then the connection
// closes on the peer's answer, or after one keepalive interval without it
// (§4.4). The session lives on: a Client establishes it again on a new
// connection, a Server waits for the client to. Terminate returns once the
// Terminate is queued, without waiting for the answer: Handler.OnUnbound
// tells when the connection is closed. It does nothing if the session is
// not established.
func (s *Session) Terminate(ctx context.Context, reason string) error {
	s.mu.Lock()
	c := s.conn
	if c == nil || s.state != StateEstablished {
		s.mu.Unlock()
		return nil
	}
	s.state = StateTerminating
	s.mu.Unlock()
	c.terminate(reason)
	return nil
}

// Done is closed once the session is finalized or stopped.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns why the session stopped: nil if finalized, or still running.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Stats returns the session's counters.
func (s *Session) Stats() *Stats { return &s.stats }

// halt stops the session on an error of its own, its store's: its
// connection is closed, and it is done.
func (s *Session) halt(err error) {
	s.logWarn("halting", "err", err)
	s.stop(err)
}

// stop stops the session: err tells why, nil once finalized.
func (s *Session) stop(err error) {
	s.mu.Lock()
	if s.state == StateStopped || s.state == StateFinalized {
		s.mu.Unlock()
		return
	}
	s.state = StateStopped
	s.err = err
	c := s.conn
	s.mu.Unlock()
	if c != nil {
		c.close(err)
	}
	s.doneOnce.Do(func() { close(s.done) })
}

// closeStore releases the session's store, once the session is done.
func (s *Session) closeStore() {
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	if st != nil {
		st.Close()
	}
}

// run runs the session on c until c ends; ctx ending terminates it
// gracefully, with reason.
func (s *Session) run(ctx context.Context, c *conn, reason string) {
	s.mu.Lock()
	if s.state == StateStopped || s.state == StateFinalized {
		s.mu.Unlock()
		c.close(ErrStopped)
		close(c.unbound)
		return
	}
	s.conn = c
	s.state = StateEstablished
	s.mu.Unlock()
	// Registered once bound: Terminate finds the connection, ctx ended
	// already or not.
	stop := context.AfterFunc(ctx, func() { s.Terminate(context.Background(), reason) })
	defer stop()
	c.number = s.stats.Establishments.Add(1)
	s.logInfo("established", "remote", c.t.RemoteAddr().String())
	s.handler.OnEstablished(s.cbCtx, s)
	go c.writeLoop()
	err := c.startRecovery()
	if err == nil {
		err = c.startFinish()
	}
	if err != nil {
		c.failOn(err)
	} else {
		c.readLoop()
	}
	c.waitWriter()
	c.close(nil)
	err = c.result()
	finished := c.finishedExchange()
	s.mu.Lock()
	s.conn = nil
	if s.state == StateEstablished || s.state == StateTerminating {
		s.state = StateUnbound
		if finished {
			// Never unbound for another connection: it is over.
			s.state = StateFinalizing
		}
	}
	s.mu.Unlock()
	s.logInfo("unbound", "err", err)
	s.handler.OnUnbound(s.cbCtx, s, err)
	close(c.unbound)
	if finished {
		s.finalize()
	} else if s.onUnbound != nil {
		s.onUnbound(s)
	}
}
