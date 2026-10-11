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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/session/store"
	"github.com/sylr/fix2/session/store/memstore"
)

// phase is where a session is in its connection's life.
type phase uint8

const (
	phaseDisconnected phase = iota
	phaseAwaitingLogon        // acceptor: connected, waiting for the Logon
	phaseLogonSent            // initiator: Logon sent, waiting for the answer
	phaseEstablished          // logged on
	phaseLogoutSent           // our Logout sent, waiting for the answer
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

// Outgoing is a message to send: fix2.Builder and the builders fixgen
// generates, which embed it, are Outgoing. AppendBody appends the fields
// after the standard header the session writes, and fails if a field's tag
// is one reserved reports true for.
type Outgoing interface {
	MsgType() string
	AppendBody(dst []byte, reserved func(tag int) bool) ([]byte, error)
}

// Session is a FIX session. Create it with New and run it with an Initiator
// or an Acceptor.
type Session struct {
	id      SessionID
	cfg     Config
	prof    *profile
	prec    fix2.TimePrecision
	log     *slog.Logger
	handler Handler
	store   store.Store

	dicts dicts

	initiator bool // set by Initiator.Add or Acceptor.Add

	// bodyPool holds the buffers Send encodes bodies into.
	bodyPool sync.Pool

	mu      sync.Mutex
	st      store.State // generation, NextSender, and NextTarget: the next expected
	phase   phase
	epoch   uint64
	c       *conn // the current connection, nil when disconnected
	stopped bool
	stalled bool // a handler callback is stuck, see Config.MaxDeliveryStall
	resetting bool
	heartBt   time.Duration
	// peerApplVerID is the counterparty's DefaultApplVerID (FIXT).
	peerApplVerID string
	// sentReset is set when our Logon carried ResetSeqNumFlag=Y.
	sentReset bool
	recov     recovery
	stash     map[uint64][]byte // received ahead of a gap, by MsgSeqNum
	receipts  receipts
	deliv     *delivery
	// committed is the MsgSeqNum of the last received message processed:
	// delivered to the handler, or needing no delivery.
	committed uint64
	// stashMsg parses messages kept ahead of a gap.
	stashMsg fix2.Message
	// replyTo is the MsgSeqNum of the message being handled, which our
	// answers to it give as LastMsgSeqNumProcessed.
	replyTo uint64
	// gapFilling is set while a GapFill moves the expected MsgSeqNum.
	gapFilling bool

	stats Stats
}

// Stats counts what a session did; its fields are read atomically.
type Stats struct {
	MessagesSent, MessagesReceived atomic.Uint64
	BytesSent, BytesReceived       atomic.Uint64
	Resent, GapFilled              atomic.Uint64
	RejectsSent, Garbled           atomic.Uint64
	Connections                    atomic.Uint64
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
		heartBt: cfg.HeartBtInt,
	}
	s.log = cfg.Logger.With(slog.String("session", cfg.ID.String()))
	if s.store == nil {
		s.store = memstore.New()
	}
	var err error
	if s.dicts, err = newDicts(cfg, p); err != nil {
		return nil, fmt.Errorf("session %s: %w", cfg.ID, err)
	}
	st, err := s.store.Open(context.Background())
	if err != nil {
		return nil, fmt.Errorf("session %s: opening its store: %w", cfg.ID, err)
	}
	s.st = st
	s.committed = st.NextTarget - 1
	s.receipts.init(cfg.MaxTrackedReceipts)
	return s, nil
}

// ID returns the session's ID.
func (s *Session) ID() SessionID { return s.id }

// Stats returns the session's counters.
func (s *Session) Stats() *Stats { return &s.stats }

// State returns where the session is.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.stopped:
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

// Send sends m to the counterparty and returns its receipt. The session
// writes the standard header: m must not set the tags it does (BeginString,
// MsgSeqNum, the CompIDs, SendingTime...). Send returns once the message is
// sequenced and stored; it is written by the connection's writer. If the
// outbound queue is full it waits for room until ctx is done, unless
// Config.FailWhenFull is set or ctx is a handler callback's, in which cases
// it fails with ErrSlowConsumer or ErrInCallback.
func (s *Session) Send(ctx context.Context, m Outgoing) (Receipt, error) {
	bp, _ := s.bodyPool.Get().(*[]byte)
	if bp == nil {
		b := make([]byte, 0, 512)
		bp = &b
	}
	defer s.bodyPool.Put(bp)
	body, err := m.AppendBody((*bp)[:0], reserved)
	*bp = body[:0]
	if err != nil {
		return Receipt{}, fmt.Errorf("session %s: %w", s.id, err)
	}
	return s.send(ctx, m.MsgType(), body)
}

// SendRaw sends a message of type msgType whose body, the fields after the
// standard header, is already encoded, as a gateway forwarding messages
// does. body must not hold the tags the session writes.
func (s *Session) SendRaw(ctx context.Context, msgType string, body []byte) (Receipt, error) {
	return s.send(ctx, msgType, body)
}

// send sequences, stores and queues an application message.
func (s *Session) send(ctx context.Context, msgType string, body []byte) (Receipt, error) {
	if isAdmin([]byte(msgType)) {
		return Receipt{}, fmt.Errorf("session %s: MsgType %q is a session-level message the session sends itself", s.id, msgType)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		switch {
		case s.stopped:
			return Receipt{}, ErrStopped
		case s.resetting:
			return Receipt{}, ErrResetting
		case s.phase != phaseEstablished || s.c == nil:
			return Receipt{}, ErrNotLoggedOn
		}
		c := s.c
		if c.replaying() {
			if len(c.heldFrames) >= s.cfg.MaxHeldMessages {
				return Receipt{}, ErrReplayInProgress
			}
			break
		}
		if c.hasRoom() {
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
func (s *Session) queueLocked(h *header, body []byte, control bool) (uint64, error) {
	c := s.c
	if c == nil {
		return 0, ErrNotLoggedOn
	}
	h.seq = s.st.NextSender
	h.sendingTime = time.Now()
	if s.cfg.LastMsgSeqNumProcessed && s.prof.lastProc {
		// The message answered, as QuickFIX sends it, else the last one
		// received in sequence.
		h.lastProcessed = s.replyTo
		if h.lastProcessed == 0 {
			h.lastProcessed = s.st.NextTarget - 1
		}
	}
	buf, frames := &c.pending, &c.frames
	if c.replaying() {
		buf, frames = &c.held, &c.heldFrames
	}
	start := len(*buf)
	*buf = s.appendFrame(*buf, h, body)
	raw := (*buf)[start:]
	if err := s.store.Append(context.Background(), s.st.Generation, h.seq, raw); err != nil {
		*buf = (*buf)[:start]
		s.log.LogAttrs(context.Background(), slog.LevelError, "storing a message failed",
			slog.Uint64("seq", h.seq), slog.String("msg_type", h.msgType), slog.Any("error", err))
		if errors.Is(err, store.ErrUnknownOutcome) || errors.Is(err, store.ErrSeqMismatch) {
			// The sequence number may be taken: never reuse it.
			s.disconnectLocked(c, &LogoutError{Text: "store failure", Err: err})
		}
		return 0, fmt.Errorf("session %s: storing message %d: %w", s.id, h.seq, err)
	}
	s.st.NextSender++
	*frames = append(*frames, pendingFrame{end: len(*buf), seq: h.seq, queued: h.sendingTime, probe: h.probe})
	_ = control
	c.wakeWriter()
	return h.seq, nil
}

// queueProbeLocked queues a TestRequest probing for acknowledgements.
func (s *Session) queueProbeLocked(id string) (uint64, error) {
	return s.queueLocked(&header{msgType: "1", probe: id}, adminBody(nil).str(tagTestReqID, id), true)
}

// sendAdminLocked sends a session-level message with header h and body.
// Failures disconnect: they are logged by queueLocked.
func (s *Session) sendAdminLocked(h *header, body []byte) {
	if _, err := s.queueLocked(h, body, true); err != nil && s.c != nil {
		s.disconnectLocked(s.c, &LogoutError{Text: "could not send " + h.msgType, Err: err})
	}
}

// adminBody is a scratch buffer for the bodies of session-level messages.
type adminBody []byte

func (b adminBody) str(tag int, v string) adminBody { return fix2.AppendTagValue(b, tag, v) }
func (b adminBody) bytes(tag int, v []byte) adminBody {
	return fix2.AppendTagValue(b, tag, v)
}
func (b adminBody) int(tag int, v uint64) adminBody { return fix2.AppendTagInt(b, tag, int64(v)) }

// dicts are a session's dictionaries, compiled for validation and parsing.
type dicts struct {
	// session validates FIX 4.x messages, and FIXT session messages.
	session *dictionary.Dict
	// apps validate FIXT application messages, by ApplVerID: transport
	// header and trailer composed with the application dictionary.
	apps map[string]*dictionary.Dict
	// parse is the profile messages are parsed with, apps the profiles of
	// the FIXT application versions.
	parse     *fix2.Profile
	parseApps map[string]*fix2.Profile
}

func newDicts(cfg Config, p *profile) (dicts, error) {
	d := dicts{session: cfg.Dictionaries.Session, parse: fix2.DefaultProfile()}
	if d.session != nil {
		d.parse = d.session.Profile()
	}
	if !p.fixt || d.session == nil {
		return d, nil
	}
	d.apps = make(map[string]*dictionary.Dict, len(cfg.Dictionaries.Apps))
	d.parseApps = make(map[string]*fix2.Profile, len(cfg.Dictionaries.Apps))
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
	return d, nil
}
