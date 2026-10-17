package binancefix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
)

var (
	// ErrNotLoggedOn is Send's error without a logged-on connection, or
	// once its Logout is under way.
	ErrNotLoggedOn = errors.New("binancefix: not logged on")
	// ErrQueueFull is Send's error once Config.MaxQueued requests wait to
	// be written: the request was not queued.
	ErrQueueFull = errors.New("binancefix: queue full")
	// ErrDuplicateID is Send's error for a request of an ID (ClOrdID,
	// ClListID, ReqID) another request awaiting an answer has.
	ErrDuplicateID = errors.New("binancefix: ID of a request awaiting an answer")
	// ErrInvalidID is Send's error for a request whose ID is missing or
	// not 1 to 36 letters, digits, - or _.
	ErrInvalidID = errors.New("binancefix: invalid ID")
	// ErrNotRequest is Send's error for a message that is not a request.
	ErrNotRequest = errors.New("binancefix: not a request")
	// ErrRunning is Run's error when called more than once.
	ErrRunning = errors.New("binancefix: Run called twice")
	// ErrPeerSilent ends a connection whose peer did not answer our
	// TestRequest within HeartBtInt.
	ErrPeerSilent = errors.New("binancefix: no Heartbeat answering our TestRequest")
	// ErrLogonTimeout ends a connection whose Logon was not answered
	// within Config.LogonTimeout.
	ErrLogonTimeout = errors.New("binancefix: no answer to our Logon")
	// ErrBacklog ends a connection whose writer is stuck while Binance's
	// TestRequests keep coming: their answers are not queued without
	// bound.
	ErrBacklog = errors.New("binancefix: session messages not written: writer stuck")
	// ErrLoggedOut ends the requests left by a Logout of ours.
	ErrLoggedOut = errors.New("binancefix: logged out")
)

// SequenceError ends a connection on which Binance's sequence number was
// not the one expected: Binance resends nothing, a gap is not recovered.
type SequenceError struct {
	Expected, Got uint32
}

func (e *SequenceError) Error() string {
	return fmt.Sprintf("binancefix: sequence number %d, expected %d", e.Got, e.Expected)
}

// PeerLogoutError ends a connection Binance logged out.
type PeerLogoutError struct{ Text string }

func (e *PeerLogoutError) Error() string {
	return fmt.Sprintf("binancefix: logged out by Binance: %q", e.Text)
}

// LogonRefusedError is Binance's refusal of our Logon, by Reject or Logout.
type LogonRefusedError struct {
	Text      string
	ErrorCode Optional[int32]
}

func (e *LogonRefusedError) Error() string {
	if e.ErrorCode.Valid {
		return fmt.Sprintf("binancefix: Logon refused: %q (error code %d)", e.Text, e.ErrorCode.Value)
	}
	return fmt.Sprintf("binancefix: Logon refused: %q", e.Text)
}

// ProtocolError ends a connection on which Binance sent what the client
// cannot follow: a malformed frame or header, a message of another schema,
// an unexpected message.
type ProtocolError struct{ Err error }

func (e *ProtocolError) Error() string { return "binancefix: protocol error: " + e.Err.Error() }
func (e *ProtocolError) Unwrap() error { return e.Err }

// Client is a FIX SBE order entry client: one connection at a time,
// reconnected by Run, its requests sent with Send.
type Client struct {
	cfg Config
	log *slog.Logger
	h   Handler

	events  chan event
	stop    chan struct{}
	stopped sync.Once
	recon   chan uint64 // the epoch a Reconnect asked to leave
	running atomic.Bool

	mu    sync.Mutex
	cur   *conn // the logged-on connection, nil if none
	epoch uint64

	// For tests: the heartbeat interval timers run on, HeartBtInt if 0,
	// and the first sequence numbers after the Logon's.
	interval          time.Duration
	nextOutAfterLogon uint32
	nextInAfterLogon  uint32
}

// New returns a Client of configuration cfg.
func New(cfg Config) (*Client, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Client{
		cfg:    cfg,
		log:    cfg.Logger,
		h:      cfg.Handler,
		events: make(chan event, cfg.MaxEvents),
		stop:   make(chan struct{}),
		recon:  make(chan uint64, 16),
	}, nil
}

// Stop asks Run to log out and return. It does not wait: Run returns once
// done.
func (c *Client) Stop() {
	c.stopped.Do(func() { close(c.stop) })
}

// Reconnect asks for connection epoch to be logged out and another opened
// at once, as Binance asks in its maintenance News (NewsEvent.Epoch). It
// does not wait. It does nothing once that connection is no longer the
// logged-on one: a News handled late does not end the next connection.
func (c *Client) Reconnect(epoch uint64) {
	select {
	case c.recon <- epoch:
	default:
	}
}

// Epoch returns the current connection's epoch, 0 before the first.
func (c *Client) Epoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

// LoggedOn reports whether a connection is logged on and takes requests.
func (c *Client) LoggedOn() bool {
	c.mu.Lock()
	k := c.cur
	c.mu.Unlock()
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.state == stateActive
}

// Request is a request Send takes: *NewOrderSingle, *OrderCancelRequest,
// *OrderCancelRequestAndNewOrderSingle, *NewOrderList,
// *OrderAmendKeepPriorityRequest, *OrderMassCancelRequest or *LimitQuery.
type Request interface {
	AppendWithHeader(dst []byte, h MessageHeader) ([]byte, error)
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,36}$`)

// Send queues request m on the logged-on connection, and returns its
// Receipt. It encodes m at once: m may be reused when Send returns. It
// never waits: without room in the queue, it fails with ErrQueueFull.
// Requests are never carried over to another connection, nor resent: see
// Receipt and State.
func (c *Client) Send(m Request) (*Receipt, error) {
	tid, legs, stop, err := requestLegs(m)
	if err != nil {
		return nil, err
	}
	frame, err := encode(m)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	k := c.cur
	c.mu.Unlock()
	if k == nil {
		return nil, ErrNotLoggedOn
	}
	return k.enqueue(frame, tid, legs, stop)
}

// encode returns the frame of m, its header's seqNum and sendingTime zero:
// the writer stamps them.
func encode(m interface {
	AppendWithHeader([]byte, MessageHeader) ([]byte, error)
}) ([]byte, error) {
	b, err := m.AppendWithHeader(lesofh.Reserve(nil), MessageHeader{})
	if err != nil {
		return nil, fmt.Errorf("binancefix: encoding %T: %w", m, err)
	}
	if err := lesofh.PutHeader(b, len(b)-lesofh.HeaderLen); err != nil {
		return nil, err
	}
	return b, nil
}

// Run connects, logs on and reconnects, until ctx is done, Stop is called,
// reconnecting is disabled, or Binance refuses the first Logon. It logs out
// before returning, and returns once the Handler handled every event: nil
// after Stop or once ctx is done, the error that ended it otherwise. Run is
// called once.
func (c *Client) Run(ctx context.Context) error {
	if !c.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.dispatch()
	}()
	err := c.loop(ctx)
	close(c.events)
	<-done
	return err
}

// loop runs connections, one after the other.
func (c *Client) loop(ctx context.Context) error {
	p := newPacer(c.cfg.Reconnect)
	refusals := 0
	for {
		if wait := p.wait(time.Now()); wait > 0 {
			c.log.LogAttrs(ctx, slog.LevelInfo, "waiting before connecting", slog.Duration("wait", wait))
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil
			case <-c.stop:
				t.Stop()
				return nil
			}
		}
		if ctx.Err() != nil || c.isStopped() {
			return nil
		}
		p.attempt(time.Now())
		res := c.connect(ctx)
		now := time.Now()
		switch {
		case res.stopped:
			return nil
		case res.loggedOn:
			refusals = 0
			p.succeeded()
			if !res.reconnect {
				p.failed(now)
			}
		case res.refused:
			// A refusal may pass, the SenderCompID's cooldown or a
			// connection limit, or not, bad credentials: retried, at
			// most MaxRefusals in a row.
			refusals++
			if refusals >= c.cfg.Reconnect.MaxRefusals {
				return res.err
			}
			p.failed(now)
		default:
			p.failed(now)
		}
		if res.logonWritten && !res.clean {
			// Binance refuses the SenderCompID for twice HeartBtInt
			// after a session ended without Logout: ours may have been
			// logged on unseen.
			p.cooldown(now.Add(2 * res.interval))
		}
		if res.err != nil {
			c.log.LogAttrs(ctx, slog.LevelWarn, "connection ended", slog.Any("error", res.err))
		}
		if c.cfg.Reconnect.Disabled {
			return res.err
		}
	}
}

func (c *Client) isStopped() bool {
	select {
	case <-c.stop:
		return true
	default:
		return false
	}
}

// connect dials and runs a connection until it ends.
func (c *Client) connect(ctx context.Context) result {
	dctx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	go func() {
		select {
		case <-c.stop:
			cancel()
		case <-dctx.Done():
		}
	}()
	tc, err := c.cfg.Dialer.Dial(dctx)
	cancel()
	if ctx.Err() != nil || c.isStopped() {
		if err == nil {
			tc.Close()
		}
		return result{stopped: true}
	}
	if err != nil {
		return result{err: fmt.Errorf("binancefix: dialing: %w", err)}
	}
	c.mu.Lock()
	c.epoch++
	epoch := c.epoch
	c.mu.Unlock()
	return newConn(c, tc, epoch).run(ctx)
}

// event is what the dispatcher hands to the Handler.
type event struct {
	in   Inbound
	msg  any // *ExecutionReport, ..., *Reject, *LogonEvent, *LogoutEvent, *NewsEvent
	body []byte
	err  error
}

// dispatch hands the events to the Handler, in order, until Run ends.
func (c *Client) dispatch() {
	for e := range c.events {
		switch m := e.msg.(type) {
		case *LogonEvent:
			c.h.OnLogon(*m)
		case *LogoutEvent:
			c.h.OnLogout(*m)
		case *NewsEvent:
			c.h.OnNews(*m)
		case *Reject:
			c.h.OnReject(e.in, m)
		case *ExecutionReport:
			c.h.OnExecutionReport(e.in, m)
		case *ExecutionReportAck:
			c.h.OnExecutionReportAck(e.in, m)
		case *OrderCancelReject:
			c.h.OnOrderCancelReject(e.in, m)
		case *OrderAmendReject:
			c.h.OnOrderAmendReject(e.in, m)
		case *ListStatus:
			c.h.OnListStatus(e.in, m)
		case *OrderMassCancelReport:
			c.h.OnOrderMassCancelReport(e.in, m)
		case *LimitResponse:
			c.h.OnLimitResponse(e.in, m)
		default:
			c.h.OnUnknown(e.in, e.body, e.err)
		}
	}
}

// pacer paces connection attempts: a backoff doubling at every failure in
// a row, at most MaxAttempts in any Window, and the cooldown of a
// SenderCompID whose session ended without Logout.
type pacer struct {
	p         Reconnect
	backoff   time.Duration
	notBefore time.Time
	attempts  []time.Time
}

func newPacer(p Reconnect) *pacer { return &pacer{p: p, backoff: p.MinBackoff} }

// wait returns how long to wait, at now, before the next attempt.
func (p *pacer) wait(now time.Time) time.Duration {
	at := now
	if p.notBefore.After(at) {
		at = p.notBefore
	}
	for len(p.attempts) > 0 && now.Sub(p.attempts[0]) >= p.p.Window {
		p.attempts = p.attempts[1:]
	}
	if n := len(p.attempts); n >= p.p.MaxAttempts {
		if t := p.attempts[n-p.p.MaxAttempts].Add(p.p.Window); t.After(at) {
			at = t
		}
	}
	return at.Sub(now)
}

// attempt records an attempt at now.
func (p *pacer) attempt(now time.Time) { p.attempts = append(p.attempts, now) }

// failed delays the next attempt by the backoff, which doubles.
func (p *pacer) failed(now time.Time) {
	if t := now.Add(p.backoff); t.After(p.notBefore) {
		p.notBefore = t
	}
	p.backoff = min(2*p.backoff, p.p.MaxBackoff)
}

// succeeded resets the backoff.
func (p *pacer) succeeded() { p.backoff = p.p.MinBackoff }

// cooldown delays the next attempt until t at least.
func (p *pacer) cooldown(t time.Time) {
	if t.After(p.notBefore) {
		p.notBefore = t
	}
}
