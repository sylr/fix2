package binancefix

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
	"github.com/sylr/fix2/transport"
)

// The offsets, in a frame, of the message header's seqNum and sendingTime,
// which the writer stamps.
const (
	seqNumOffset      = lesofh.HeaderLen + 8
	sendingTimeOffset = lesofh.HeaderLen + 12
)

// maxBatch bounds the bytes the writer writes at once.
const maxBatch = 64 << 10

// maxLogoutText bounds the Text of our Logout.
const maxLogoutText = 200

type connState uint8

const (
	stateLogon   connState = iota // Logon sent, waiting for its answer
	stateActive                   // logged on: requests taken
	stateClosing                  // a Logout under way: no request taken
	stateClosed
)

// result is how a connection ended.
type result struct {
	err          error
	loggedOn     bool          // the Logon was answered
	refused      bool          // the Logon was refused
	clean        bool          // our Logout was written
	stopped      bool          // ended by Stop or Run's context
	logonWritten bool          // our Logon was written, wholly or partly
	reconnect    bool          // ended by Reconnect
	interval     time.Duration // the heartbeat interval
}

// adminKind is what a session message to write is.
type adminKind uint8

const (
	adminLogout    adminKind = iota + 1 // our Logout: the writer writes nothing after it
	adminHeartbeat                      // a Heartbeat of ours
	adminTest                           // our TestRequest
	adminAnswer                         // a Heartbeat answering a TestRequest of Binance's
)

// maxAnswers bounds the Heartbeats answering Binance's TestRequests that
// wait to be written: beyond, the writer is stuck, and the connection is
// ended rather than queue more.
const maxAnswers = 64

// adminFrame is a session message to write.
type adminFrame struct {
	frame []byte
	kind  adminKind
	test  string // an adminTest's TestReqID
}

// pending is a request to write: a Receipt's, or a subscription's
// request or unsubscribe.
type pending struct {
	frame []byte
	r     *Receipt
	s     *Subscription
	unsub bool
}

// conn is a connection and its session.
type conn struct {
	c        *Client
	tc       transport.Conn
	epoch    uint64
	interval time.Duration
	start    time.Time // the clock lastSent and lastRecv count from

	mu       sync.Mutex
	state    connState
	admin    []adminFrame
	app      []pending
	ids      map[string]*Receipt // requests awaiting an answer, by ID
	inflight map[uint32]*Receipt // requests written, awaiting an answer, by seqNum
	unsent   []*Receipt          // requests ended NeverWritten before the end
	order    uint64              // requests written so far
	queued   uint64              // requests queued so far
	hbQueued bool                // a Heartbeat of ours waits to be written
	testID   string              // our TestRequest awaiting its Heartbeat
	testAt   time.Duration       // when it was written
	testSent bool                // it was written: its answer is awaited
	answers  int                 // adminAnswer frames queued
	tests    int                 // TestRequests sent

	// The subscriptions, which outlive their requests' writing and
	// Unsubscribe: until the connection ends, an MDReqID and a stream
	// stay taken.
	subs    map[string]*Subscription // by MDReqID
	streams map[Stream]*Subscription
	subSeqs map[uint32]subRef // their requests and unsubscribes, by seqNum
	subList []*Subscription   // in the order made
	rate    *rateBudget       // market data's message budget, nil for order entry

	signal chan struct{} // wakes the writer

	endOnce   sync.Once
	ended     chan struct{} // closed by end
	endErr    error
	endLogout bool

	loggedOn      chan struct{} // closed on LogonAck
	peerLogout    chan struct{} // closed on Binance's Logout
	logoutWritten chan struct{} // closed once our Logout is written
	closing       chan struct{} // closed when the transport is closed
	readerDone    chan struct{}
	writerDone    chan struct{}
	monitorDone   chan struct{}

	lastSent atomic.Int64 // since start, in nanoseconds
	lastRecv atomic.Int64
	stalled  atomic.Bool // the reader waits for the Handler

	// The reader's.
	nextIn uint32
	// The writer's, once the Logon is written.
	nextOut uint32
}

func newConn(c *Client, tc transport.Conn, epoch uint64) *conn {
	interval := c.interval
	if interval == 0 {
		interval = c.cfg.HeartBtInt
	}
	var rate *rateBudget
	if c.rateLimit > 0 {
		rate = newRateBudget(c.rateLimit, c.rateReserve, c.rateWindow)
	}
	return &conn{
		c: c, tc: tc, epoch: epoch, interval: interval, start: time.Now(),
		ids:           map[string]*Receipt{},
		inflight:      map[uint32]*Receipt{},
		subs:          map[string]*Subscription{},
		streams:       map[Stream]*Subscription{},
		subSeqs:       map[uint32]subRef{},
		rate:          rate,
		signal:        make(chan struct{}, 1),
		ended:         make(chan struct{}),
		loggedOn:      make(chan struct{}),
		peerLogout:    make(chan struct{}),
		logoutWritten: make(chan struct{}),
		closing:       make(chan struct{}),
		readerDone:    make(chan struct{}),
		writerDone:    make(chan struct{}),
		monitorDone:   make(chan struct{}),
		nextIn:        1,
	}
}

// now returns the time since the connection started.
func (k *conn) now() time.Duration { return time.Since(k.start) }

// end ends the connection for err, after our Logout if logout is set. The
// first call counts.
func (k *conn) end(err error, logout bool) {
	k.endOnce.Do(func() {
		k.mu.Lock()
		if k.state == stateActive {
			// No request is taken any more.
			k.state = stateClosing
		}
		k.endErr, k.endLogout = err, logout
		k.mu.Unlock()
		if k.c.cfg.Reconnect.Disabled {
			// The last connection: Run connects no more. Ending is
			// closed before anything waits for the Handler, the reader
			// blocked on a full queue, finish queueing OnLogout, so that
			// a Handler waiting for it returns. Stop and Run's context
			// close it as they come; refused Logons queue nothing.
			k.c.closeEnding()
		}
		close(k.ended)
	})
}

func (k *conn) wake() {
	select {
	case k.signal <- struct{}{}:
	default:
	}
}

// logonFrame returns our Logon, sent at t: its seqNum, 1, and its
// sendingTime are set once, and signed.
func (k *conn) logonFrame(t time.Time) ([]byte, error) {
	cfg := &k.c.cfg
	t = logonTime(t)
	m := buildLogon(cfg, sign(cfg.PrivateKey, cfg.SignatureTime, cfg.SenderCompID, TargetCompID, 1, t))
	b, err := m.AppendWithHeader(lesofh.Reserve(nil), MessageHeader{SeqNum: 1, SendingTime: t.UnixMicro()})
	if err != nil {
		return nil, fmt.Errorf("binancefix: encoding the Logon: %w", err)
	}
	if err := lesofh.PutHeader(b, len(b)-lesofh.HeaderLen); err != nil {
		return nil, err
	}
	return b, nil
}

// buildLogon returns the Logon of cfg, of signature rawData.
func buildLogon(cfg *Config, rawData []byte) *fixsbe.Logon {
	m := &fixsbe.Logon{
		EncryptMethod:   fixsbe.Some[uint8](0),
		HeartBtInt:      uint32(cfg.HeartBtInt / time.Second),
		ResetSeqNumFlag: fixsbe.Some(fixsbe.BoolEnumTrue),
		MessageHandling: fixsbe.Some(cfg.MessageHandling),
		SenderCompId:    []byte(cfg.SenderCompID),
		TargetCompId:    []byte(TargetCompID),
		RawData:         rawData,
		Username:        []byte(cfg.APIKey),
	}
	if cfg.ResponseMode != 0 {
		m.ResponseMode = fixsbe.Some(cfg.ResponseMode)
	}
	if cfg.ExecutionReportType != 0 {
		m.ExecutionReportType = fixsbe.Some(cfg.ExecutionReportType)
	}
	if cfg.RecvWindow != 0 {
		m.RecvWindow = fixsbe.Some(uint32(cfg.RecvWindow / time.Microsecond))
	}
	return m
}

// run logs on and runs the session until it ends.
func (k *conn) run(ctx context.Context) result {
	c, cfg := k.c, &k.c.cfg
	log := c.log.With(slog.Uint64("epoch", k.epoch))
	res := result{interval: k.interval}
	frame, err := k.logonFrame(time.Now())
	if err != nil {
		k.tc.Close()
		res.err = err
		return res
	}
	k.tc.SetWriteDeadline(time.Now().Add(cfg.LogonTimeout))
	n, err := k.tc.Write(frame)
	k.traceWritten(frame, n)
	res.logonWritten = n > 0
	if err != nil {
		k.tc.Close()
		res.err = fmt.Errorf("binancefix: writing the Logon: %w", err)
		return res
	}
	k.lastSent.Store(int64(k.now()))
	if k.rate != nil {
		k.mu.Lock()
		k.rate.sending(1)
		k.rate.wrote(time.Now(), 0, 1)
		k.mu.Unlock()
	}
	k.nextOut = 2
	if c.nextOutAfterLogon != 0 {
		k.nextOut = c.nextOutAfterLogon
	}
	go k.readLoop()

	logonTimer := time.NewTimer(cfg.LogonTimeout)
	defer logonTimer.Stop()
	early := false
	// Logged on already, another case ready too, as a Stop from OnLogon
	// before this select is reached: the way below, which logs out.
	select {
	case <-k.loggedOn:
	case <-k.ended:
	case <-logonTimer.C:
		if !k.isLoggedOn() {
			k.end(ErrLogonTimeout, false)
			early = true
		}
	case <-ctx.Done():
		if !k.isLoggedOn() {
			k.end(nil, false)
			early, res.stopped = true, true
		}
	case <-c.stop:
		if !k.isLoggedOn() {
			k.end(nil, false)
			early, res.stopped = true, true
		}
	}
	if early || !k.isLoggedOn() {
		// Ended before the Logon was answered, or as it was. Once
		// logged on, an end, even at once, takes the way below: a
		// Logout of Binance's is answered.
		res.err = k.endErr
		var refused *LogonRefusedError
		res.refused = errors.As(res.err, &refused)
		k.close()
		<-k.readerDone
		if k.isLoggedOn() {
			res.loggedOn = true
			k.finish(log, res.err)
		}
		return res
	}
	res.loggedOn, res.interval = true, k.interval
	go k.writeLoop()
	go k.monitor()

	logout, waitPeer := false, false
wait:
	for {
		select {
		case <-k.ended:
			logout = k.endLogout
		case <-ctx.Done():
			logout, waitPeer, res.stopped = true, true, true
		case <-c.stop:
			logout, waitPeer, res.stopped = true, true, true
		case e := <-c.recon:
			if e != k.epoch {
				continue // for a connection gone
			}
			logout, waitPeer, res.reconnect = true, true, true
		}
		break wait
	}
	if waitPeer {
		// Our own end: what happens during the Logout does not change it.
		k.end(nil, false)
	}
	if logout {
		text, reason := "", error(ErrLoggedOut)
		select {
		case <-k.ended:
			var peer *PeerLogoutError
			if k.endErr != nil {
				reason = k.endErr
				if !errors.As(k.endErr, &peer) {
					text = k.endErr.Error()
				}
			}
		default:
		}
		k.logout(text, reason)
		t := time.NewTimer(cfg.LogoutTimeout)
		written := false
		select {
		case <-k.logoutWritten:
			written = true
		case <-k.writerDone:
			// The writer ends once the Logout is written, which it tells
			// first: both may be ready.
			select {
			case <-k.logoutWritten:
				written = true
			default:
			}
		case <-t.C:
			log.LogAttrs(ctx, slog.LevelWarn, "our Logout not written in time")
		}
		if written && waitPeer {
			select {
			case <-k.peerLogout:
			case <-k.readerDone:
			case <-t.C:
				log.LogAttrs(ctx, slog.LevelWarn, "no Logout answering ours")
			}
		}
		t.Stop()
	}
	k.end(nil, false)
	res.err = k.endErr
	k.close()
	<-k.readerDone
	<-k.writerDone
	<-k.monitorDone
	select {
	case <-k.logoutWritten:
		res.clean = true
	default:
	}
	k.finish(log, res.err)
	return res
}

func (k *conn) isLoggedOn() bool {
	select {
	case <-k.loggedOn:
		return true
	default:
		return false
	}
}

// close closes the transport, ending the reader and the writer.
func (k *conn) close() {
	k.mu.Lock()
	select {
	case <-k.closing:
	default:
		close(k.closing)
	}
	k.mu.Unlock()
	k.tc.Close()
}

// finish ends the connection's requests left unanswered, once the reader
// and the writer are done, and tells the Handler.
func (k *conn) finish(log *slog.Logger, err error) {
	c := k.c
	c.mu.Lock()
	if c.cur == k {
		c.cur = nil
	}
	c.mu.Unlock()
	reason := err
	if reason == nil {
		reason = ErrLoggedOut
	}
	k.mu.Lock()
	k.state = stateClosed
	queued := k.app
	k.app = nil
	sent := make([]*Receipt, 0, len(k.inflight))
	for _, r := range k.inflight {
		sent = append(sent, r)
	}
	never := k.unsent
	k.ids, k.inflight, k.unsent = nil, nil, nil
	subs := k.subList
	k.subs, k.streams, k.subSeqs, k.subList = nil, nil, nil, nil
	k.mu.Unlock()
	for _, p := range queued {
		if p.s != nil {
			p.s.unsent(p.unsub)
			continue
		}
		if p.r.end(reason, true) {
			never = append(never, p.r)
		}
	}
	for _, s := range subs {
		s.end(reason)
	}
	slices.SortFunc(never, func(a, b *Receipt) int { return cmp.Compare(a.queued, b.queued) })
	slices.SortFunc(sent, func(a, b *Receipt) int { return cmp.Compare(a.order, b.order) })
	var possibly []*Receipt
	for _, r := range sent {
		if r.end(reason, false) {
			possibly = append(possibly, r)
		}
	}
	ev := &LogoutEvent{Epoch: k.epoch, Err: err, NeverWritten: never, PossiblyTransmitted: possibly, Subscriptions: subs}
	select {
	case <-k.logoutWritten:
		ev.Clean = true
	default:
	}
	log.LogAttrs(context.Background(), slog.LevelInfo, "logged out", slog.Any("reason", err), slog.Bool("clean", ev.Clean),
		slog.Int("never_written", len(never)), slog.Int("possibly_transmitted", len(possibly)), slog.Int("subscriptions", len(subs)))
	// Its error may retain Binance's Logout text: charged, as a message
	// is. The dispatcher frees room as the Handler returns. A Handler
	// waiting on something of its own until Ending (a blocked output)
	// returns once it is closed: already, when this end is the last (see
	// end; Stop and Run's context close it as they come). Otherwise the
	// wait is backpressure: Run does not reconnect meanwhile, and Stop or
	// Run's context ends it.
	size := 0
	if err != nil {
		size = len(err.Error())
	}
	c.room.take(size)
	c.events <- event{msg: ev, size: size}
}

// enqueue queues request frame, of template tid and legs legs.
func (k *conn) enqueue(frame []byte, tid uint16, legs []Leg, stop bool) (*Receipt, error) {
	k.mu.Lock()
	if k.state != stateActive {
		k.mu.Unlock()
		return nil, ErrNotLoggedOn
	}
	if len(k.app) >= k.c.cfg.MaxQueued {
		k.mu.Unlock()
		return nil, ErrQueueFull
	}
	for _, l := range legs {
		if k.ids[l.ID] != nil {
			k.mu.Unlock()
			return nil, fmt.Errorf("%w: %q", ErrDuplicateID, l.ID)
		}
	}
	if k.rate != nil && !k.rate.admit(time.Now()) {
		k.mu.Unlock()
		return nil, ErrRateLimited
	}
	r := newReceipt(k.epoch, tid, legs, stop)
	k.queued++
	r.queued = k.queued
	for _, id := range r.ids {
		k.ids[id] = r
	}
	k.app = append(k.app, pending{frame: frame, r: r})
	k.mu.Unlock()
	k.wake()
	return r, nil
}

// queueAdmin queues session message m, of kind kind. It fails, queueing
// nothing, when maxAnswers answers wait already.
func (k *conn) queueAdmin(m interface {
	AppendWithHeader([]byte, MessageHeader) ([]byte, error)
}, kind adminKind, test string) error {
	frame, err := encode(m)
	if err != nil {
		// The session's messages are built within their bounds.
		panic(err)
	}
	k.mu.Lock()
	if kind == adminAnswer {
		if k.answers >= maxAnswers {
			k.mu.Unlock()
			return ErrBacklog
		}
		k.answers++
	}
	k.admin = append(k.admin, adminFrame{frame: frame, kind: kind, test: test})
	k.mu.Unlock()
	k.wake()
	return nil
}

// logout queues our Logout of text: requests still queued are dropped,
// NeverWritten for reason, and no other is taken.
func (k *conn) logout(text string, reason error) {
	if len(text) > maxLogoutText {
		text = text[:maxLogoutText]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	k.mu.Lock()
	if k.state == stateActive {
		k.state = stateClosing
	}
	dropped := k.app
	k.app = nil
	for _, p := range dropped {
		if p.r == nil {
			continue
		}
		for _, id := range p.r.ids {
			delete(k.ids, id)
		}
	}
	if k.rate != nil {
		k.rate.dropped(len(dropped))
	}
	k.mu.Unlock()
	for _, p := range dropped {
		if p.s != nil {
			p.s.unsent(p.unsub)
			continue
		}
		if p.r.end(reason, true) {
			k.mu.Lock()
			k.unsent = append(k.unsent, p.r)
			k.mu.Unlock()
		}
	}
	k.queueAdmin(&fixsbe.Logout{Text: []byte(text)}, adminLogout, "")
}

// stamp writes seqNum seq and sendingTime t into frame's header.
func stamp(frame []byte, seq uint32, t time.Time) {
	binary.LittleEndian.PutUint32(frame[seqNumOffset:], seq)
	binary.LittleEndian.PutUint64(frame[sendingTimeOffset:], uint64(t.UnixMicro()))
}

// span is a request's place in a batch.
type span struct {
	start, end int
	r          *Receipt
	s          *Subscription
	unsub      bool
}

// writeLoop writes the session messages first, then the requests, each
// stamped with the next seqNum and the time of the write.
func (k *conn) writeLoop() {
	defer close(k.writerDone)
	var buf []byte
	var spans []span
	for {
		select {
		case <-k.signal:
		case <-k.closing:
			return
		}
		for {
			k.mu.Lock()
			admin := k.admin
			k.admin = nil
			var batch []pending
			size := 0
			logout := slices.ContainsFunc(admin, func(a adminFrame) bool { return a.kind == adminLogout })
			if !logout && k.state == stateActive {
				for len(k.app) > 0 && size < maxBatch {
					batch = append(batch, k.app[0])
					size += len(k.app[0].frame)
					k.app[0] = pending{}
					k.app = k.app[1:]
				}
			}
			if len(admin) == 0 && len(batch) == 0 {
				k.mu.Unlock()
				break
			}
			now := time.Now()
			buf, spans = buf[:0], spans[:0]
			hb, test, written := false, "", 0
			for _, a := range admin {
				stamp(a.frame, k.nextOut, now)
				k.nextOut++
				written++
				buf = append(buf, a.frame...)
				switch a.kind {
				case adminHeartbeat:
					hb = true
				case adminTest:
					test = a.test
				case adminAnswer:
					k.answers--
				}
				if a.kind == adminLogout {
					break
				}
			}
			for _, p := range batch {
				stamp(p.frame, k.nextOut, now)
				if p.s != nil {
					p.s.sending(p.unsub)
					k.subSeqs[k.nextOut] = subRef{s: p.s, unsub: p.unsub}
				} else {
					k.order++
					p.r.sending(k.nextOut, k.order)
					k.inflight[k.nextOut] = p.r
				}
				k.nextOut++
				spans = append(spans, span{start: len(buf), end: len(buf) + len(p.frame), r: p.r, s: p.s, unsub: p.unsub})
				buf = append(buf, p.frame...)
			}
			if hb {
				k.hbQueued = false
			}
			if k.rate != nil {
				// Counted as in flight until the write returns: Binance may
				// read them any time until then.
				k.rate.sending(written)
			}
			k.mu.Unlock()

			k.tc.SetWriteDeadline(now.Add(k.c.cfg.WriteTimeout))
			n, err := k.tc.Write(buf)
			k.traceWritten(buf, n)
			k.lastSent.Store(int64(k.now()))
			if k.rate != nil {
				k.mu.Lock()
				k.rate.wrote(time.Now(), len(batch), written)
				k.mu.Unlock()
			}
			if err != nil {
				// What lies wholly before n was written; what lies wholly
				// past it never reached the connection.
				k.mu.Lock()
				var unsent []*Receipt
				for _, s := range spans {
					if s.s != nil {
						switch {
						case s.end <= n:
							s.s.written(s.unsub)
						case s.start >= n:
							s.s.unsent(s.unsub)
						}
						continue
					}
					if s.start >= n {
						seq, _ := s.r.SeqNum()
						delete(k.inflight, seq)
						for _, id := range s.r.ids {
							delete(k.ids, id)
						}
						unsent = append(unsent, s.r)
					}
				}
				k.mu.Unlock()
				werr := fmt.Errorf("binancefix: writing: %w", err)
				for _, r := range unsent {
					if r.end(werr, true) {
						k.mu.Lock()
						k.unsent = append(k.unsent, r)
						k.mu.Unlock()
					}
				}
				k.end(werr, false)
				return
			}
			for _, s := range spans {
				if s.s != nil {
					s.s.written(s.unsub)
				} else {
					s.r.written()
				}
			}
			if test != "" {
				// Its answer is awaited from now: the time it waited to be
				// written is not the peer's. A TestRequest answered, and
				// another queued, while this write returned is not this
				// one.
				k.mu.Lock()
				if k.testID == test {
					k.testAt, k.testSent = k.now(), true
				}
				k.mu.Unlock()
			}
			if logout {
				close(k.logoutWritten)
				return
			}
		}
	}
}

// monitor sends our Heartbeats after HeartBtInt without writing, and our
// TestRequest after HeartBtInt without reading; it ends the connection
// when the TestRequest goes unanswered for another HeartBtInt.
func (k *conn) monitor() {
	defer close(k.monitorDone)
	tick := time.NewTicker(max(k.interval/16, time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-k.closing:
			return
		case <-k.ended:
			return
		}
		now := k.now()
		k.mu.Lock()
		if k.state != stateActive {
			k.mu.Unlock()
			continue
		}
		// Sent a little before HeartBtInt, so that Binance, counting
		// HeartBtInt of silence, never sees one.
		if !k.hbQueued && now-time.Duration(k.lastSent.Load()) >= k.interval-k.interval/8 {
			k.hbQueued = true
			k.mu.Unlock()
			k.queueAdmin(&fixsbe.Heartbeat{TestReqID: []byte{}}, adminHeartbeat, "")
			k.mu.Lock()
		}
		if k.stalled.Load() {
			// Not reading is ours, not the peer's silence.
			k.mu.Unlock()
			continue
		}
		switch {
		case k.testID == "" && now-time.Duration(k.lastRecv.Load()) >= k.interval:
			k.tests++
			k.testID = "T" + strconv.FormatUint(k.epoch, 10) + "-" + strconv.Itoa(k.tests)
			k.testSent = false
			id := k.testID
			k.mu.Unlock()
			k.queueAdmin(&fixsbe.TestRequest{TestReqID: []byte(id)}, adminTest, id)
			continue
		case k.testID != "" && k.testSent && now-k.testAt >= k.interval:
			k.mu.Unlock()
			k.end(ErrPeerSilent, true)
			return
		}
		k.mu.Unlock()
	}
}

// readLoop reads and handles Binance's messages until the connection ends.
func (k *conn) readLoop() {
	defer close(k.readerDone)
	fr := lesofh.NewReader(k.tc, k.c.cfg.MaxFrameLength)
	for {
		msg, err := fr.Next()
		if err != nil {
			select {
			case <-k.closing:
				k.end(nil, false)
			case <-k.peerLogout:
				k.end(nil, false)
			default:
				if errors.Is(err, lesofh.ErrShortLength) || errors.Is(err, lesofh.ErrLongLength) || errors.Is(err, lesofh.ErrEncoding) {
					k.end(&ProtocolError{Err: err}, true)
				} else {
					k.end(fmt.Errorf("binancefix: reading: %w", err), false)
				}
			}
			return
		}
		received := time.Now()
		k.trace(false, false, msg)
		k.lastRecv.Store(int64(k.now()))
		if err := k.handle(msg, received); err != nil {
			k.end(err, k.isLoggedOn())
			return
		}
	}
}

// emit hands e to the dispatcher, waiting for room, in events and in
// bytes: reading waits for the Handler, the monitor knows.
func (k *conn) emit(e event) {
	if k.c.room.tryTake(e.size) {
		select {
		case k.c.events <- e:
			return
		default:
		}
		k.stalled.Store(true)
	} else {
		k.stalled.Store(true)
		k.c.room.take(e.size)
	}
	k.c.events <- e
	// The time waited is not the peer's: our TestRequest's answer may be
	// unread yet.
	now := k.now()
	k.lastRecv.Store(int64(now))
	k.mu.Lock()
	if k.testID != "" && k.testSent {
		k.testAt = now
	}
	k.mu.Unlock()
	k.stalled.Store(false)
}

// handle handles a received message, returning the error ending the
// connection.
func (k *conn) handle(msg []byte, received time.Time) error {
	select {
	case <-k.peerLogout:
		return nil // nothing counts after Binance's Logout
	default:
	}
	h, body, err := fixsbe.ReadHeader(msg)
	if err != nil {
		return &ProtocolError{Err: err}
	}
	if h.SchemaId != fixsbe.SchemaID {
		return &ProtocolError{Err: fmt.Errorf("message of schema %d, template %d", h.SchemaId, h.TemplateId)}
	}
	loggedOn := k.isLoggedOn()
	if !loggedOn {
		switch h.TemplateId {
		case fixsbe.RejectTemplateID:
			var m fixsbe.Reject
			if _, err := m.DecodeBody(h, body); err != nil {
				return &ProtocolError{Err: fmt.Errorf("Reject: %w", err)}
			}
			return &LogonRefusedError{Text: string(m.Text), ErrorCode: m.ErrorCode}
		case fixsbe.LogoutTemplateID:
			var m fixsbe.Logout
			if _, err := m.DecodeBody(h, body); err != nil {
				return &ProtocolError{Err: fmt.Errorf("Logout: %w", err)}
			}
			return &LogonRefusedError{Text: string(m.Text)}
		case fixsbe.LogonAckTemplateID:
		default:
			return &ProtocolError{Err: fmt.Errorf("template %d before the LogonAck", h.TemplateId)}
		}
	}
	if h.SeqNum != k.nextIn {
		return &SequenceError{Expected: k.nextIn, Got: h.SeqNum}
	}
	k.nextIn = h.SeqNum + 1 // 4,294,967,295 rolls over to 0
	in := Inbound{Epoch: k.epoch, Header: h, Received: received}
	switch h.TemplateId {
	case fixsbe.LogonAckTemplateID:
		if loggedOn {
			return &ProtocolError{Err: errors.New("a second LogonAck")}
		}
		var m fixsbe.LogonAck
		if _, err := m.DecodeBody(h, body); err != nil {
			return &ProtocolError{Err: fmt.Errorf("LogonAck: %w", err)}
		}
		if err := checkLogonAck(&m); err != nil {
			return &LogonRefusedError{Text: err.Error(), Err: err}
		}
		return k.onLogon(&m)
	case fixsbe.HeartbeatTemplateID:
		var m fixsbe.Heartbeat
		if _, err := m.DecodeBody(h, body); err != nil {
			return &ProtocolError{Err: fmt.Errorf("Heartbeat: %w", err)}
		}
		k.mu.Lock()
		if k.testID != "" && string(m.TestReqID) == k.testID {
			k.testID = ""
		}
		k.mu.Unlock()
		return nil
	case fixsbe.TestRequestTemplateID:
		var m fixsbe.TestRequest
		if _, err := m.DecodeBody(h, body); err != nil {
			return &ProtocolError{Err: fmt.Errorf("TestRequest: %w", err)}
		}
		if len(m.TestReqID) == 0 {
			// Its answer could not be told from a Heartbeat.
			return &ProtocolError{Err: fmt.Errorf("TestRequest: %w: TestReqID", ErrMissingRequired)}
		}
		return k.queueAdmin(&fixsbe.Heartbeat{TestReqID: bytes.Clone(m.TestReqID)}, adminAnswer, "")
	case fixsbe.LogoutTemplateID:
		var m fixsbe.Logout
		if _, err := m.DecodeBody(h, body); err != nil {
			return &ProtocolError{Err: fmt.Errorf("Logout: %w", err)}
		}
		k.mu.Lock()
		ours := k.state == stateClosing
		k.state = max(k.state, stateClosing)
		k.mu.Unlock()
		close(k.peerLogout)
		if !ours {
			// Answered with ours by run.
			k.end(&PeerLogoutError{Text: string(m.Text)}, true)
		}
		return nil
	}

	// What follows reaches the Handler: decoded from a copy of its own.
	body = bytes.Clone(body)
	if k.c.cfg.Kind == MarketData {
		if err := checkHeader(h); err != nil {
			return &ProtocolError{Err: err}
		}
		if ok, err := k.handleMarketData(h, body, in); ok {
			return err
		}
	}
	var m any
	var refSeq Optional[uint32]
	var code Optional[int32]
	switch h.TemplateId {
	case fixsbe.RejectTemplateID:
		x := new(fixsbe.Reject)
		_, err = x.DecodeBody(h, body)
		m, refSeq, code = x, x.RefSeqNum, x.ErrorCode
	case fixsbe.NewsTemplateID:
		x := new(fixsbe.News)
		if _, err = x.DecodeBody(h, body); err == nil {
			headline := string(x.Headline)
			if headline == "" {
				// Required, but a notice: told of, empty, its countdown
				// unknown, rather than ending the session.
				k.c.log.LogAttrs(context.Background(), slog.LevelWarn, "Binance news without its required Headline", slog.Uint64("epoch", k.epoch))
			}
			k.c.log.LogAttrs(context.Background(), slog.LevelWarn, "Binance news", slog.Uint64("epoch", k.epoch), slog.String("headline", headline))
			k.emit(event{msg: &NewsEvent{Inbound: in, Headline: headline, Remaining: remaining(headline)}, size: len(headline)})
			return nil
		}
		m = x
	case fixsbe.ExecutionReportTemplateID:
		x := new(fixsbe.ExecutionReport)
		_, err = x.DecodeBody(h, body)
		m, code = x, x.ErrorCode
	case fixsbe.ExecutionReportAckTemplateID:
		x := new(fixsbe.ExecutionReportAck)
		_, err = x.DecodeBody(h, body)
		m, code = x, x.ErrorCode
	case fixsbe.OrderCancelRejectTemplateID:
		x := new(fixsbe.OrderCancelReject)
		_, err = x.DecodeBody(h, body)
		m, code = x, fixsbe.Some(x.ErrorCode)
	case fixsbe.OrderAmendRejectTemplateID:
		x := new(fixsbe.OrderAmendReject)
		_, err = x.DecodeBody(h, body)
		m, code = x, fixsbe.Some(x.ErrorCode)
	case fixsbe.ListStatusTemplateID:
		x := new(fixsbe.ListStatus)
		_, err = x.DecodeBody(h, body)
		m = x
		for _, o := range x.Orders {
			if o.ErrorCode.Valid {
				code = o.ErrorCode
				if code.Value == errTimeout {
					break
				}
			}
		}
	case fixsbe.OrderMassCancelReportTemplateID:
		x := new(fixsbe.OrderMassCancelReport)
		_, err = x.DecodeBody(h, body)
		m, code = x, x.ErrorCode
	case fixsbe.LimitResponseTemplateID:
		x := new(fixsbe.LimitResponse)
		_, err = x.DecodeBody(h, body)
		m = x
	default:
		k.emit(event{in: in, body: body, size: len(body)})
		return nil
	}
	if err != nil {
		k.c.log.LogAttrs(context.Background(), slog.LevelWarn, "message not decoded", slog.Uint64("epoch", k.epoch),
			slog.Int("template_id", int(h.TemplateId)), slog.Any("error", err))
		k.emit(event{in: in, body: body, err: err, size: len(body)})
		return nil
	}
	in.Receipt = k.correlate(m, refSeq, answerOf(h, code))
	if x, ok := m.(*fixsbe.Reject); ok && in.Receipt == nil {
		in.Subscription = k.rejectedBySeq(x, in)
	}
	k.emit(event{in: in, msg: m, size: len(body)})
	return nil
}

// handleMarketData hands market data message h, body to the Handler, with
// the request it answers or the subscription it belongs to. It reports
// false for a template that is not market data. A market data message
// decodeMarketData refuses ends the connection, after our Logout: a
// corrupted update would silently desynchronise a book, and a new
// connection is a clean boundary.
func (k *conn) handleMarketData(h MessageHeader, body []byte, in Inbound) (bool, error) {
	m, symbol, ok, err := decodeMarketData(h, body)
	if !ok {
		return false, nil
	}
	if err != nil {
		return true, &ProtocolError{Err: err}
	}
	switch x := m.(type) {
	case *fixsbe.InstrumentList:
		in.Receipt = k.correlate(x, Optional[uint32]{}, answerOf(h, Optional[int32]{}))
	case *fixsbe.MarketDataRequestReject:
		in.Subscription = k.rejectedByID(x, in)
	default:
		in.Subscription = k.route(h.TemplateId, string(symbol), in)
	}
	k.emit(event{in: in, msg: m, size: len(body)})
	return true, nil
}

// correlate returns the request message m answers a leg of, nil if none:
// a written request awaiting answers, named by one of the IDs m carries,
// or, for a Reject, by its RefSeqNum refSeq. Which messages answer which
// legs is Receipt.apply's.
func (k *conn) correlate(m any, refSeq Optional[uint32], a Answer) *Receipt {
	k.mu.Lock()
	defer k.mu.Unlock()
	var candidates []*Receipt
	for _, id := range answerIDs(m) {
		if x := k.ids[string(id)]; x != nil && x.isSent() && !slices.Contains(candidates, x) {
			candidates = append(candidates, x)
		}
	}
	if refSeq.Valid {
		if x := k.inflight[refSeq.Value]; x != nil {
			candidates = append(candidates, x)
		}
	}
	for _, r := range candidates {
		answers, final := r.answered(m, a)
		if !answers {
			continue
		}
		if final {
			for _, id := range r.ids {
				delete(k.ids, id)
			}
			seq, _ := r.SeqNum()
			delete(k.inflight, seq)
		}
		return r
	}
	return nil
}

// checkLogonAck returns why the client refuses LogonAck m, nil if it does
// not: its required HeartBtInt null (uint32's null, 4,294,967,295) or out
// of Binance's bounds, 5 to 60 seconds; its required
// SbeSchemaIdVersionDeprecated null (255) or neither of its values; its
// required UUID empty. Its EncryptMethod is optional.
func checkLogonAck(m *fixsbe.LogonAck) error {
	switch {
	case m.HeartBtInt == math.MaxUint32:
		return fmt.Errorf("%w: %w: HeartBtInt", ErrInvalidLogonAck, ErrMissingRequired)
	case time.Duration(m.HeartBtInt)*time.Second < MinHeartBtInt || time.Duration(m.HeartBtInt)*time.Second > MaxHeartBtInt:
		return fmt.Errorf("%w: HeartBtInt %d, not from %v to %v", ErrInvalidLogonAck, m.HeartBtInt, MinHeartBtInt, MaxHeartBtInt)
	case m.SbeSchemaIdVersionDeprecated == 255:
		return fmt.Errorf("%w: %w: SbeSchemaIdVersionDeprecated", ErrInvalidLogonAck, ErrMissingRequired)
	case m.SbeSchemaIdVersionDeprecated != fixsbe.BoolEnumFalse && m.SbeSchemaIdVersionDeprecated != fixsbe.BoolEnumTrue:
		return fmt.Errorf("%w: SbeSchemaIdVersionDeprecated %d", ErrInvalidLogonAck, m.SbeSchemaIdVersionDeprecated)
	case len(m.UUID) == 0:
		return fmt.Errorf("%w: %w: UUID", ErrInvalidLogonAck, ErrMissingRequired)
	}
	return nil
}

// onLogon makes the connection the Client's, and tells the Handler.
func (k *conn) onLogon(m *fixsbe.LogonAck) error {
	c := k.c
	if c.nextInAfterLogon != 0 {
		k.nextIn = c.nextInAfterLogon
	}
	ev := &LogonEvent{Epoch: k.epoch, HeartBtInt: time.Duration(m.HeartBtInt) * time.Second, UUID: string(m.UUID),
		SchemaDeprecated: m.SbeSchemaIdVersionDeprecated == fixsbe.BoolEnumTrue}
	if c.interval == 0 && ev.HeartBtInt >= MinHeartBtInt && ev.HeartBtInt <= MaxHeartBtInt && ev.HeartBtInt != k.interval {
		// Binance mirrors ours; were it to differ, its own counts.
		k.mu.Lock()
		k.interval = ev.HeartBtInt
		k.mu.Unlock()
	}
	k.mu.Lock()
	k.state = stateActive
	k.mu.Unlock()
	c.mu.Lock()
	c.cur = k
	c.mu.Unlock()
	close(k.loggedOn)
	log := c.log.With(slog.Uint64("epoch", k.epoch))
	log.LogAttrs(context.Background(), slog.LevelInfo, "logged on", slog.String("uuid", ev.UUID), slog.Duration("heartbeat", ev.HeartBtInt))
	if ev.SchemaDeprecated {
		log.LogAttrs(context.Background(), slog.LevelWarn, "Binance deprecated the SBE schema in use", slog.Int("schema_id", fixsbe.SchemaID), slog.Int("schema_version", fixsbe.SchemaVersion))
	}
	k.emit(event{msg: ev, size: len(ev.UUID)})
	return nil
}
