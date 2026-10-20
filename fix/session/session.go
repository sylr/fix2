// Package session runs FIX sessions: logon, sequence numbers, heartbeats,
// resends and logout, for FIX 4.0 to 4.4 and FIXT 1.1, as an initiator or an
// acceptor.
//
// A Session is configured in Go (see Config) and run by an Initiator, which
// dials its counterparty, or an Acceptor, which serves many sessions on its
// listeners. Each session has its own dictionaries, store and handler.
//
// Concurrency: everything that changes a session's state happens under one
// lock, held briefly and never across I/O or callbacks. Each connection has a
// reader goroutine, which frames, parses and checks received messages, and a
// writer goroutine, which writes what Send and the session queue and resends
// stored messages on request. Received application messages are delivered,
// in order, by a goroutine of their own, so that a slow handler does not stop
// heartbeats.
package session

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/session/schedule"
	"github.com/sylr/fix2/fix/session/store"
	"github.com/sylr/fix2/fix/session/store/memstore"
)

// phase is where a session is in its connection's life.
type phase uint8

const (
	phaseDisconnected  phase = iota
	phaseAwaitingLogon       // acceptor: connected, waiting for the Logon
	phaseLogonSent           // initiator: Logon sent, waiting for the answer
	phaseEstablished         // logged on
	phaseLogoutSent          // our Logout sent, waiting for the answer
)

func (p phase) String() string {
	return [...]string{"disconnected", "awaiting logon", "logon sent", "logged on", "logout sent"}[p]
}

// State is a session's state as Session.State reports it.
type State string

const (
	StateDisconnected  State = "disconnected"
	StateLoggingOn     State = "logging on"
	StateLoggedOn      State = "logged on"
	StateLoggingOut    State = "logging out"
	StateStopped       State = "stopped"
	StateStalled       State = "stalled"
	StateResettingSeqs State = "resetting sequence numbers"
)

// Outgoing is a message to send: fix.Builder and the builders fix2gen
// generates, which embed it, are Outgoing. AppendBody appends the fields
// after the standard header the session writes, and fails if a field's tag
// is one reserved reports true for, or if a value holds SOH but in a DATA
// field of p, the profile the counterparty parses the message with, after
// its LENGTH field (see fix.Builder.AppendBody).
type Outgoing interface {
	MsgType() string
	AppendBody(dst []byte, p *fix.Profile, reserved func(tag int) bool) ([]byte, error)
}

// Session is a FIX session. Create it with New and run it with an Initiator
// or an Acceptor.
type Session struct {
	id      SessionID
	cfg     Config
	prof    *profile
	prec    fix.TimePrecision
	log     *slog.Logger
	handler Handler
	store   store.Store
	syncer  store.Syncer // the store, if its Appends are durable only once synced

	dicts dicts

	initiator bool // set by Initiator.Add or Acceptor.Add
	// authWarned is set once the session warned it accepted a Logon
	// without authentication.
	authWarned atomic.Bool

	// lockedWrites and closeWrites are bumped before and after each store
	// write, odd while one runs (see watchStore): those made holding mu,
	// one at a time, and the store's Close, made once, when the session
	// stops, maybe while a setter writes. stuck holds why the session
	// halted when one took longer than StoreTimeout, which watchStore tells
	// without the lock the write holds; cur is the connection it closes.
	lockedWrites atomic.Uint64
	closeWrites  atomic.Uint64
	stuck        atomic.Pointer[error]
	stuckCh      chan struct{} // closed with stuck set
	cur          atomic.Pointer[conn]
	watching     atomic.Bool // a watchStore chain runs
	stopQueued   atomic.Bool // a stop runs, to complete once the write stuck returns

	// bodyPool holds the buffers Send encodes bodies into, of up to
	// maxPooledBuffer bytes.
	bodyPool sync.Pool
	// cbCtx is the context handler callbacks get.
	cbCtx context.Context
	// done is closed once the session is stopped and its callbacks
	// returned, see Done.
	done chan struct{}
	// detached counts the callbacks running for connections not yet
	// attached, see startDetached.
	detached sync.WaitGroup

	mu        sync.Mutex
	st        store.State // generation, NextSender, and NextTarget: the next expected
	phase     phase
	epoch     uint64
	c         *conn // the current connection, nil when disconnected
	prev      *conn // the last connection, whose end a new one waits for
	stopped   bool
	stopping  bool   // Stop runs: no new connection
	halted    error  // a store failure left the persisted state uncertain: no new connection
	resetOwed string // why a reset on disconnection that failed is owed: the next connection makes it first
	stalled   bool   // a handler callback is stuck, see Config.MaxDeliveryStall
	resetting bool
	heartBt   time.Duration
	// logoutWait is the LogoutTimeout of heartBt (see setHeartBtLocked),
	// for a shutdown, which must not wait for mu: a store write that hangs
	// holds it.
	logoutWait atomic.Int64
	// peerApplVerID is the counterparty's DefaultApplVerID (FIXT).
	peerApplVerID string
	// sentReset is set when our Logon carried ResetSeqNumFlag=Y.
	sentReset bool
	recov     recovery
	stash     map[uint64]keptMsg // received ahead of a gap, by MsgSeqNum
	stashSize int                // the bytes of stash's messages
	receipts  receipts
	deliv     *delivery
	// committed is the MsgSeqNum of the last received message processed:
	// delivered to the handler, or needing no delivery.
	committed uint64
	// stashMsg parses messages kept ahead of a gap.
	stashMsg fix.Message
	// replyTo is the MsgSeqNum of the message being handled, which our
	// answers to it give as LastMsgSeqNumProcessed.
	replyTo uint64
	// partial is the message being handled if it was parsed only up to a
	// malformed tag: a credential may follow the tag, unseen by withhold,
	// so nothing taken from it is logged, as garbled logs nothing (see
	// withholdLocked). headerCut tells the tag is within its header: a
	// header field it lacks may follow the tag rather than be missing (see
	// unseen).
	partial   *fix.Message
	headerCut bool
	// rejected lists the messages rejected (see rejectLocked) while session
	// messages are handled: one rejected does not reach Handler.FromAdmin.
	// Those held ahead of a gap, handled meanwhile, are listed after it.
	rejected     []rejection
	watchRejects int
	// rejectNotes holds, for a RejectObserver, the messages rejected while
	// a frame is handled, until the reader tells it (see notifyRejects).
	rejectNotes []Rejected
	// handlerRejecting is set while a Handler's error is answered: its
	// Rejects are not told a RejectObserver.
	handlerRejecting bool
	// gapFilling is set while a GapFill moves the expected MsgSeqNum.
	gapFilling bool
	// draining is set while advanceLocked handles messages kept ahead.
	draining bool
	// heldScanned counts the messages kept ahead of a gap a
	// SequenceReset-Reset looked at, for tests.
	heldScanned uint64
	// The schedule's window: whether the session is within one, which,
	// a channel closed when one opens, and how to stop following it.
	inWindow    bool
	window      schedule.Window
	windowOpen  chan struct{}
	schedCancel context.CancelFunc
	ret         retention

	stats Stats
}

// Stats counts what a session did; its fields are read atomically.
type Stats struct {
	// MessagesReceived and BytesReceived count the messages handled, those
	// ignored after the counterparty's Logout included, and those rejected
	// for a malformed tag, which the Observer does not see: it would show
	// what follows the tag, where the session cannot tell a credential (see
	// garbled).
	MessagesSent, MessagesReceived atomic.Uint64
	BytesSent, BytesReceived       atomic.Uint64
	RejectsSent, Garbled           atomic.Uint64
	Connections                    atomic.Uint64
	// Resent counts the messages a replay wrote again; GapFilled, the
	// sequence numbers its GapFills written stand for.
	Resent, GapFilled atomic.Uint64
	// Evicted counts the sequence numbers Config.Retention evicted from the
	// store, since the process started; BeyondRetention, the messages
	// gap-filled because they were evicted, each time the counterparty
	// asked for one.
	Evicted, BeyondRetention atomic.Uint64
}

// New returns a session configured by cfg, which it validates.
func New(cfg Config) (*Session, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	p, _ := profileOf(cfg.ID.BeginString)
	s := &Session{
		id:      cfg.ID,
		cfg:     cfg,
		prof:    p,
		prec:    cfg.precision(p),
		handler: cfg.Handler,
		store:   cfg.Store,
		done:    make(chan struct{}),
		stuckCh: make(chan struct{}),
	}
	s.setHeartBtLocked(cfg.HeartBtInt)
	s.log = cfg.Logger.With(slog.String("session", cfg.ID.String()))
	s.cbCtx = context.WithValue(context.Background(), callbackKey{}, s)
	s.handler = trackCallbacks(s, cfg.Handler)
	if s.store == nil {
		s.store = memstore.New()
	}
	s.syncer, _ = s.store.(store.Syncer)
	var err error
	if s.dicts, err = newDicts(cfg, p); err != nil {
		return nil, fmt.Errorf("session %s: %w", cfg.ID, err)
	}
	// Bounded by StoreTimeout, as the Open after a Reset is, though through
	// ctx alone: no lock is held, only the caller waits. A store waiting
	// for a connection of a pool the application holds, say, fails Add
	// rather than hang it.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.StoreTimeout)
	st, err := s.store.Open(ctx)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("session %s: opening its store took longer than StoreTimeout (%s): %w", cfg.ID, cfg.StoreTimeout, err)
		}
		return nil, fmt.Errorf("session %s: opening its store: %w", cfg.ID, err)
	}
	s.st = st
	s.committed = st.NextTarget - 1
	s.receipts.init(cfg.MaxTrackedReceipts, st.Generation)
	s.ret.floor = 1
	s.storedLocked()
	return s, nil
}

// ID returns the session's ID.
func (s *Session) ID() SessionID { return s.id }

// Stats returns the session's counters.
func (s *Session) Stats() *Stats { return &s.stats }

// State returns where the session is.
func (s *Session) State() State {
	if !s.lockUnlessStuck() {
		return StateStopped // its lock held by a store write that hangs
	}
	defer s.mu.Unlock()
	switch {
	case s.stopped || s.halted != nil || s.stuck.Load() != nil:
		return StateStopped
	case s.stalled:
		return StateStalled
	case s.resetting:
		return StateResettingSeqs
	}
	switch s.phase {
	case phaseAwaitingLogon, phaseLogonSent:
		return StateLoggingOn
	case phaseEstablished:
		return StateLoggedOn
	case phaseLogoutSent:
		return StateLoggingOut
	}
	return StateDisconnected
}

// Err returns why the session halted, nil if it did not: a store failure
// left its persisted state uncertain (a sequence number perhaps taken, a
// reset perhaps made, a generation moved under it), so it neither sends nor
// connects again, and State reports it stopped. Reconcile the store, then
// remove the session from its Initiator or Acceptor (Remove), which stops
// it, and add it again. A store write taking longer than
// Config.StoreTimeout halts it too, Err telling it even while the write
// still runs.
func (s *Session) Err() error {
	if !s.lockUnlessStuck() {
		return *s.stuck.Load()
	}
	defer s.mu.Unlock()
	if s.halted == nil {
		if err := s.stuck.Load(); err != nil {
			return *err // stuck, the write not returned yet
		}
	}
	return s.halted
}

// now reads the session's wall clock (see Config.Now).
func (s *Session) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// setHeartBtLocked sets the heartbeat interval in effect, and the
// LogoutTimeout it gives.
func (s *Session) setHeartBtLocked(hb time.Duration) {
	s.heartBt = hb
	s.logoutWait.Store(int64(s.cfg.logoutTimeout(hb)))
}

// logoutTimeout returns the LogoutTimeout in effect, without mu.
func (s *Session) logoutTimeout() time.Duration {
	return time.Duration(s.logoutWait.Load())
}

// lockUnlessStuck takes mu and reports true, or reports false once the
// session is stuck (see storeStuck): the store write holding mu may never
// return, and a caller waiting for it before it was found stuck must not
// wait with it. Contended, it waits in mu's queue on a goroutine of its
// own, which hands mu over, or gives it back if the caller gave up.
func (s *Session) lockUnlessStuck() bool {
	if s.mu.TryLock() {
		return true
	}
	if s.stuck.Load() != nil {
		return false
	}
	got, gaveUp := make(chan struct{}), make(chan struct{})
	go func() {
		s.mu.Lock()
		select {
		case got <- struct{}{}:
		case <-gaveUp:
			s.mu.Unlock()
		}
	}()
	select {
	case <-got:
		return true
	case <-s.stuckCh:
		close(gaveUp)
		return false
	}
}

// storeLocked runs f, a store write made holding mu, where watchStore sees
// it: one that took longer than StoreTimeout halts the session once it
// returns, and fails, whatever it returned, the caller not carrying on as
// if the session were not halted.
func (s *Session) storeLocked(f func() error) error {
	err := s.storeWrite(&s.lockedWrites, f)
	if s.stuck.Load() != nil {
		s.haltLocked(errStoreTimeout)
		return errors.Join(store.ErrUnknownOutcome, errStoreTimeout)
	}
	return err
}

// storeWrite runs f, a store write counted by writes, one at a time, where
// watchStore sees it.
func (s *Session) storeWrite(writes *atomic.Uint64, f func() error) error {
	writes.Add(1)
	if !s.watching.Load() && s.watching.CompareAndSwap(false, true) {
		time.AfterFunc(max(s.cfg.StoreTimeout/2, time.Millisecond), func() { s.watchStore(writeWatch{}, writeWatch{}, 0) })
	}
	err := f()
	writes.Add(1)
	return err
}

// writeWatch is what watchStore saw of a kind of store writes: the count
// of the one running, and at how many checks in a row.
type writeWatch struct {
	seen  uint64
	times int
}

// see records n, the count of a kind of writes, and reports whether one
// has run at three checks in a row: for StoreTimeout at least, and at most
// half more.
func (w *writeWatch) see(n uint64) bool {
	switch {
	case n%2 == 0:
		w.times = 0
	case n == w.seen:
		w.times++
	default:
		w.seen, w.times = n, 1
	}
	return w.times >= 3
}

// watchStore checks, every half StoreTimeout, whether a store write, of
// either kind, has run for StoreTimeout (see writeWatch.see): the session is
// stuck (see storeStuck). It stops once none ran at two checks in a row,
// idle counting them: the next write starts it again.
func (s *Session) watchStore(locked, closing writeWatch, idle int) {
	l, c := s.lockedWrites.Load(), s.closeWrites.Load()
	if locked.see(l) || closing.see(c) {
		s.storeStuck()
		return
	}
	if l%2 == 0 && c%2 == 0 {
		if idle++; idle >= 2 {
			s.watching.Store(false)
			// A write started meanwhile saw the chain running.
			if s.lockedWrites.Load()%2 == 0 && s.closeWrites.Load()%2 == 0 || !s.watching.CompareAndSwap(false, true) {
				return
			}
			idle = 0
		}
	} else {
		idle = 0
	}
	time.AfterFunc(max(s.cfg.StoreTimeout/2, time.Millisecond), func() { s.watchStore(locked, closing, idle) })
}

// storeStuck halts the session whose lock a store write that does not
// return holds, without that lock: State and Err tell it, the connection is
// closed, so that the counterparty sees the session go, and Stop returns.
// The write, once it returns, halts it for good.
func (s *Session) storeStuck() {
	err := fmt.Errorf("session %s halted, check the store before restarting: %w", s.id, errStoreTimeout)
	if !s.stuck.CompareAndSwap(nil, &err) {
		return
	}
	close(s.stuckCh)
	if c := s.cur.Load(); c != nil {
		c.tc.Close()
	}
	// The write may have returned just before stuck was set, not halting
	// the session: halted, once the lock is free, whichever it is.
	go func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.haltLocked(errStoreTimeout)
	}()
	// Last: a logger writing to the disk that hangs may hang too.
	s.log.LogAttrs(context.Background(), slog.LevelError, "session halted: a store write has run longer than StoreTimeout",
		slog.Duration("store_timeout", s.cfg.StoreTimeout))
}

// NextSenderSeqNum returns the MsgSeqNum of the next message sent.
func (s *Session) NextSenderSeqNum() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.NextSender
}

// NextTargetSeqNum returns the MsgSeqNum expected of the next message
// received.
func (s *Session) NextTargetSeqNum() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.NextTarget
}

// maxPooledBuffer bounds the buffers kept for reuse, Send's for the next
// bodies and the delivery queue's for the next messages received: one
// large message does not keep its buffer.
const maxPooledBuffer = 64 << 10

// Send sends m to the counterparty and returns its receipt. The session
// writes the standard header: m must not set the tags it does (BeginString,
// MsgSeqNum, the CompIDs, SendingTime...), nor a MsgType or a value holding
// SOH but in a DATA field after its length, the DATA fields being those of
// the session's dictionaries, of the default application version for FIXT
// (see fix.Builder.AppendBody). Send returns
// once the message is sequenced and stored; it is written by the
// connection's writer. If the outbound queue is full it waits for room
// until ctx is done, unless Config.FailWhenFull is set or ctx is a handler
// callback's, in which cases it fails with ErrSlowConsumer or
// ErrInCallback.
func (s *Session) Send(ctx context.Context, m Outgoing) (Receipt, error) {
	bp, _ := s.bodyPool.Get().(*[]byte)
	if bp == nil {
		b := make([]byte, 0, 512)
		bp = &b
	}
	defer func() {
		// One large message does not keep its buffer.
		if cap(*bp) <= maxPooledBuffer {
			s.bodyPool.Put(bp)
		}
	}()
	// The counterparty parses it with the profile we parse its messages
	// with: of our default application version, for FIXT, as no ApplVerID
	// of ours tells another.
	body, err := m.AppendBody((*bp)[:0], s.dicts.parse, reserved)
	*bp = body[:0]
	if err != nil {
		return Receipt{}, fmt.Errorf("session %s: %w", s.id, err)
	}
	return s.send(ctx, m.MsgType(), body)
}

// SendRaw sends a message of type msgType whose body, the fields after the
// standard header, is already encoded, as a gateway forwarding messages
// does. body must not hold the tags the session writes; msgType must not
// hold SOH.
func (s *Session) SendRaw(ctx context.Context, msgType string, body []byte) (Receipt, error) {
	return s.send(ctx, msgType, body)
}

// send sequences, stores and queues an application message.
func (s *Session) send(ctx context.Context, msgType string, body []byte) (Receipt, error) {
	if isAdmin([]byte(msgType)) {
		return Receipt{}, fmt.Errorf("session %s: MsgType %q is a session-level message the session sends itself", s.id, msgType)
	}
	if strings.IndexByte(msgType, 1) >= 0 {
		// The session writes MsgType in the header, apart from the body
		// AppendBody checks: SOH would end it early.
		return Receipt{}, fmt.Errorf("session %s: %w", s.id, &fix.SOHValueError{Tag: tagMsgType})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		switch {
		case s.stopped:
			return Receipt{}, ErrStopped
		case s.resetting:
			return Receipt{}, ErrResetting
		case s.phase != phaseEstablished || s.c == nil || s.c.closing:
			// Closing, after our Logout: a stalled handler's leaves the
			// phase established.
			return Receipt{}, ErrNotLoggedOn
		}
		c := s.c
		if c.replaying() {
			if len(c.heldFrames) >= s.cfg.MaxHeldMessages || len(c.held) > 0 && len(c.held)+len(body) > s.cfg.MaxPendingBytes {
				return Receipt{}, ErrReplayInProgress
			}
			break
		}
		// Its body: its header is a few dozen bytes more.
		if c.hasRoom(len(body)) {
			break
		}
		if s.cfg.FailWhenFull {
			return Receipt{}, ErrSlowConsumer
		}
		if inCallback(ctx) {
			return Receipt{}, ErrInCallback
		}
		room := c.roomSignal()
		s.mu.Unlock()
		select {
		case <-room:
		case <-ctx.Done():
			s.mu.Lock()
			return Receipt{}, ctx.Err()
		}
		s.mu.Lock()
	}
	seq, err := s.queueLocked(&header{msgType: msgType}, body, false)
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{s: s, gen: s.st.Generation, seq: seq}, nil
}

// queueLocked sequences a message with header h and body, stores it and
// queues it on the current connection (held back if it is resending). A
// control message may exceed the queue's bounds. It returns the MsgSeqNum.
// Once the connection closes after our Logout, it refuses any other with
// errClosing, using up no MsgSeqNum: what the counterparty sends meanwhile
// goes unanswered, as the timer sends nothing then.
func (s *Session) queueLocked(h *header, body []byte, control bool) (uint64, error) {
	c := s.c
	if c == nil {
		return 0, ErrNotLoggedOn
	}
	if c.closing {
		s.log.LogAttrs(context.Background(), slog.LevelDebug, "not sent after our Logout", slog.String("msg_type", h.msgType))
		return 0, errClosing
	}
	h.seq = s.st.NextSender
	if h.sendingTime.IsZero() {
		h.sendingTime = s.now()
	}
	if s.cfg.LastMsgSeqNumProcessed && s.prof.lastProc {
		// The last message processed: the one answered, as QuickFIX
		// sends it, if every message before it is processed.
		h.lastProcessed = s.committed
		if s.replyTo != 0 && s.committed+1 >= s.replyTo {
			h.lastProcessed = s.replyTo
		}
	}
	buf, frames := &c.pending, &c.frames
	if c.replaying() {
		buf, frames = &c.held, &c.heldFrames
	}
	start := len(*buf)
	*buf = s.appendFrame(*buf, h, body)
	raw := (*buf)[start:]
	if err := s.storeLocked(func() error { return s.store.Append(context.Background(), s.st.Generation, h.seq, raw) }); err != nil {
		*buf = (*buf)[:start]
		if start == 0 {
			// Nothing left for the writer, which would drop a buffer
			// this message grew past reuse.
			*buf = reusable(*buf, s.cfg.MaxPendingBytes)
		}
		s.log.LogAttrs(context.Background(), slog.LevelError, "storing a message failed",
			slog.Uint64("seq", h.seq), slog.String("msg_type", h.msgType), slog.Any("error", err))
		if uncertain(err) {
			// The sequence number may be taken: never reuse it. The
			// session stays down until the store is sorted out.
			s.haltLocked(fmt.Errorf("storing message %d: %w", h.seq, err))
		}
		return 0, fmt.Errorf("session %s: storing message %d: %w", s.id, h.seq, err)
	}
	s.st.NextSender++
	// Not idle once something is queued: until the writer, which took
	// c.pending and has not written it yet, dates lastWrite (see wrote),
	// the timer would queue a heartbeat every millisecond.
	c.lastWrite = time.Now()
	*frames = append(*frames, pendingFrame{end: len(*buf), seq: h.seq, queued: time.Now(), probe: h.probe, admin: isAdmin([]byte(h.msgType))})
	if s.cfg.MaxPendingAge > 0 && frames == &c.frames && len(c.frames) == 1 {
		// The first message waiting: the timer is due when it is too old,
		// not at its next deadline only.
		s.armLocked(c)
	}
	_ = control
	c.wakeWriter()
	return h.seq, nil
}

// uncertain reports whether a store error leaves the persisted state in
// doubt: a write that may or may not have been made, or a store that moved
// on without the session (diverged). Only reconciling the store settles it.
func uncertain(err error) bool {
	return errors.Is(err, store.ErrUnknownOutcome) || diverged(err)
}

// diverged reports whether a store error tells the store moved on without
// the session, or was closed, whatever else it wraps or is joined with,
// ErrUnknownOutcome included: setting a value again cannot settle that, nor
// is it what cancelling a call does.
func diverged(err error) bool {
	return errors.Is(err, store.ErrSeqMismatch) || errors.Is(err, store.ErrGenerationGone) ||
		errors.Is(err, store.ErrBackwards) || errors.Is(err, store.ErrClosed)
}

// haltLocked stops the session for err, a store failure that left its
// persisted state uncertain: the connection is dropped and no new one made
// (see Err). A stopped session closed its store itself: what fails after
// does not halt it.
func (s *Session) haltLocked(err error) {
	if s.stopped {
		return
	}
	if s.halted == nil {
		s.halted = fmt.Errorf("session %s halted, check the store before restarting: %w", s.id, err)
		s.log.LogAttrs(context.Background(), slog.LevelError, "session halted", slog.Any("error", err))
		// The receipts waited for will never be acknowledged, connected or
		// not.
		s.receipts.notify()
	}
	if s.c != nil {
		s.disconnectLocked(s.c, &LogoutError{Text: "store failure", Err: err})
	}
}

// queueProbeLocked queues a TestRequest probing for acknowledgements.
func (s *Session) queueProbeLocked(id string) (uint64, error) {
	return s.queueLocked(&header{msgType: "1", probe: id}, adminBody(nil).str(tagTestReqID, id), true)
}

// sendAdminLocked sends a session-level message with header h and body.
// Failures disconnect: they are logged by queueLocked. One refused after
// our Logout is dropped.
func (s *Session) sendAdminLocked(h *header, body []byte) {
	if _, err := s.queueLocked(h, body, true); err != nil && !errors.Is(err, errClosing) && s.c != nil {
		s.disconnectLocked(s.c, &LogoutError{Text: "could not send " + h.msgType, Err: err})
	}
}

// adminBody is a scratch buffer for the bodies of session-level messages.
type adminBody []byte

func (b adminBody) str(tag int, v string) adminBody { return fix.AppendTagValue(b, tag, v) }
func (b adminBody) bytes(tag int, v []byte) adminBody {
	return fix.AppendTagValue(b, tag, v)
}
func (b adminBody) int(tag int, v uint64) adminBody { return fix.AppendTagInt(b, tag, int64(v)) }

// dicts are a session's dictionaries, compiled for validation and parsing.
type dicts struct {
	// session validates FIX 4.x messages, and FIXT session messages.
	session *dictionary.Dict
	// apps validate FIXT application messages, by ApplVerID: transport
	// header and trailer composed with the application dictionary.
	apps map[string]*dictionary.Dict
	// parse is the profile messages are parsed with, apps the profiles of
	// the FIXT application versions.
	parse     *fix.Profile
	parseApps map[string]*fix.Profile
	// versions are the keys of parseApps.
	versions []string
	// header tells, by tag, the fields of the standard header: for FIXT,
	// those headerProf reads, a message's ApplVerID looked for there; for
	// FIX 4.x, those sealing keeps in clear (see seal).
	header     []bool
	headerProf *fix.Profile
}

func newDicts(cfg Config, p *profile) (dicts, error) {
	d := dicts{session: cfg.Dictionaries.Session, parse: fix.DefaultProfile()}
	if d.session != nil {
		d.parse = d.session.Profile()
	}
	if !p.fixt {
		// FIX 4.4's standard header, as FIXT 1.1's but for ApplVerID and the
		// like, FIX 4.2 and 4.3's 370, and the session dictionary's.
		tags := append(fixtHeader[:], anyHeader[:]...)
		if d.session != nil {
			for _, f := range d.session.Header {
				tags = append(tags, f.Tag)
			}
		}
		d.header = make([]bool, slices.Max(tags)+1)
		for _, tag := range tags {
			d.header[tag] = true
		}
		return d, nil
	}
	if d.session == nil {
		// Without a dictionary, the standard header is FIXT 1.1's.
		d.header = make([]bool, slices.Max(fixtHeader[:])+1)
		for _, tag := range fixtHeader {
			d.header[tag] = true
		}
		d.headerProf = d.parse
		return d, nil
	}
	d.apps = make(map[string]*dictionary.Dict, len(cfg.Dictionaries.Apps))
	d.parseApps = make(map[string]*fix.Profile, len(cfg.Dictionaries.Apps))
	for v, app := range cfg.Dictionaries.Apps {
		if app == nil {
			return d, fmt.Errorf("Dictionaries.Apps[%q] is nil", v)
		}
		composed := dictionary.Compose(d.session, app)
		d.apps[v] = composed
		d.parseApps[v] = composed.Profile()
	}
	if def := d.parseApps[cfg.Dictionaries.DefaultApplVerID]; def != nil {
		d.parse = def
	}
	tags := []int{8, 9, tagMsgType}
	for _, f := range d.session.Header {
		tags = append(tags, f.Tag)
	}
	d.header = make([]bool, slices.Max(tags)+1)
	for _, tag := range tags {
		d.header[tag] = true
	}
	d.headerProf = d.session.Profile()
	// A message's header is read before its version is known: every version
	// must read it alike. So must it the session dictionary's other fields:
	// a session message is parsed with its version's profile, and written
	// with the session dictionary's (see adminProfile), a DATA value
	// holding SOH split otherwise.
	same := func(p *fix.Profile, tag int) bool {
		return p.DataTag(tag) == d.headerProf.DataTag(tag) && p.DataLengthTag(tag) == d.headerProf.DataLengthTag(tag)
	}
	d.versions = slices.Sorted(maps.Keys(d.parseApps))
	for _, v := range d.versions {
		p := d.parseApps[v]
		for tag, in := range d.header {
			if in && !same(p, tag) {
				return d, fmt.Errorf("Dictionaries.Apps[%q] changes whether standard header field %d is a LENGTH or DATA field, which Dictionaries.Session decides", v, tag)
			}
		}
		for _, tag := range slices.Sorted(maps.Keys(d.session.Fields)) {
			if !same(p, tag) {
				return d, fmt.Errorf("Dictionaries.Apps[%q] changes whether session field %d is a LENGTH or DATA field, or the field it pairs with, which Dictionaries.Session decides", v, tag)
			}
		}
	}
	return d, nil
}

// fixtHeader are the tags of the FIXT 1.1 standard header, NoHops (627) and
// its entries' fields included.
var fixtHeader = [...]int{8, 9, 35, 49, 56, 115, 128, 90, 91, 34, 50, 142, 57, 143, 116, 144, 129, 145,
	43, 97, 52, 122, 212, 213, 347, 369, 627, 628, 629, 630, 1128, 1129, 1156}

// applVerID returns the ApplVerID (1128) in frame's standard header: among
// the fields before the first the header does not define, DATA fields read
// by their length. ok is false if it is not there, or the header does not
// scan: parsing the frame then tells what is wrong.
func (d *dicts) applVerID(frame []byte) (v []byte, ok bool) {
	data, n := 0, 0 // the DATA field the previous one announced, and its length
	for i := 0; i < len(frame); {
		// The tag, a header one: bounded by the header's highest.
		tag, j := 0, i
		for ; j < len(frame) && frame[j] != '='; j++ {
			if c := frame[j] - '0'; c <= 9 && tag < len(d.header) {
				tag = tag*10 + int(c)
				continue
			}
			return nil, false
		}
		if j == len(frame) || tag >= len(d.header) || !d.header[tag] {
			return nil, false
		}
		start := j + 1
		end := start + n
		if data == 0 {
			if end = bytes.IndexByte(frame[start:], 1); end < 0 {
				return nil, false
			}
			end += start
		} else if tag != data || n >= len(frame)-start || frame[end] != 1 {
			// The length comes from the peer: compared by subtraction.
			return nil, false
		}
		if tag == tagApplVerID {
			return frame[start:end], end > start
		}
		if data, n = d.headerProf.DataTag(tag), 0; data != 0 {
			var err error
			if n, err = fix.ParseInt(frame[start:end]); err != nil || n < 0 {
				return nil, false
			}
		}
		i = end + 1
	}
	return nil, false
}

// headerApplVerID is applVerID for m, parsed.
func (d *dicts) headerApplVerID(m *fix.Message) (v []byte, ok bool) {
	for _, f := range m.Fields() {
		if f.Tag >= len(d.header) || !d.header[f.Tag] {
			break
		}
		if f.Tag == tagApplVerID {
			return f.Value, len(f.Value) > 0
		}
	}
	return nil, false
}

// inHeader reports whether tag is a field of the standard header.
func (d *dicts) inHeader(tag int) bool {
	return tag >= 0 && tag < len(d.header) && d.header[tag]
}

// version returns v as a string, without allocating if it has a dictionary.
func (d *dicts) version(v []byte) string {
	for _, name := range d.versions {
		if name == string(v) {
			return name
		}
	}
	return string(v)
}

// appProfile returns the profile of application version v, the default one
// if v has no dictionary.
func (d *dicts) appProfile(v string) *fix.Profile {
	return cmp.Or(d.parseApps[v], d.parse)
}

// adminProfile returns the profile of session messages: the session
// dictionary's, the default one without.
func (d *dicts) adminProfile() *fix.Profile {
	if d.session != nil {
		return d.session.Profile()
	}
	return fix.DefaultProfile()
}
