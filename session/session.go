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
	"sync"
	"sync/atomic"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/session/schedule"
	"github.com/sylr/fix2/session/store"
	"github.com/sylr/fix2/session/store/memstore"
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
	syncer  store.Syncer // the store, if its Appends are durable only once synced

	dicts dicts

	initiator bool // set by Initiator.Add or Acceptor.Add

	// bodyPool holds the buffers Send encodes bodies into.
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
	MessagesSent, MessagesReceived atomic.Uint64
	BytesSent, BytesReceived       atomic.Uint64
	Resent, GapFilled              atomic.Uint64
	RejectsSent, Garbled           atomic.Uint64
	Connections                    atomic.Uint64
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
		heartBt: cfg.HeartBtInt,
		done:    make(chan struct{}),
	}
	s.log = cfg.Logger.With(slog.String("session", cfg.ID.String()))
	s.cbCtx = context.WithValue(context.Background(), callbackKey{}, s)
	if s.store == nil {
		s.store = memstore.New()
	}
	s.syncer, _ = s.store.(store.Syncer)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.stopped || s.halted != nil:
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
// it, and add it again.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.halted
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
	if h.sendingTime.IsZero() {
		h.sendingTime = time.Now()
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
	if err := s.store.Append(context.Background(), s.st.Generation, h.seq, raw); err != nil {
		*buf = (*buf)[:start]
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
	*frames = append(*frames, pendingFrame{end: len(*buf), seq: h.seq, queued: h.sendingTime, probe: h.probe, admin: isAdmin([]byte(h.msgType))})
	_ = control
	c.wakeWriter()
	return h.seq, nil
}

// uncertain reports whether a store error leaves the persisted state in
// doubt: a write that may or may not have been made, or a store that moved
// on without the session. Only reconciling the store settles it.
func uncertain(err error) bool {
	return errors.Is(err, store.ErrUnknownOutcome) || errors.Is(err, store.ErrSeqMismatch) ||
		errors.Is(err, store.ErrGenerationGone) || errors.Is(err, store.ErrBackwards) || errors.Is(err, store.ErrClosed)
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
	// versions are the keys of parseApps.
	versions []string
	// header tells, by tag, the fields of the FIXT standard header, which
	// headerProf reads: a message's ApplVerID is looked for there.
	header     []bool
	headerProf *fix2.Profile
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
	// must read it alike.
	d.versions = slices.Sorted(maps.Keys(d.parseApps))
	for _, v := range d.versions {
		p := d.parseApps[v]
		for tag, in := range d.header {
			if in && (p.DataTag(tag) != d.headerProf.DataTag(tag) || p.DataLengthTag(tag) != d.headerProf.DataLengthTag(tag)) {
				return d, fmt.Errorf("Dictionaries.Apps[%q] changes whether standard header field %d is a LENGTH or DATA field, which Dictionaries.Session decides", v, tag)
			}
		}
	}
	return d, nil
}

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
			if n, err = fix2.ParseInt(frame[start:end]); err != nil || n < 0 {
				return nil, false
			}
		}
		i = end + 1
	}
	return nil, false
}

// headerApplVerID is applVerID for m, parsed.
func (d *dicts) headerApplVerID(m *fix2.Message) (v []byte, ok bool) {
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
func (d *dicts) appProfile(v string) *fix2.Profile {
	return cmp.Or(d.parseApps[v], d.parse)
}
