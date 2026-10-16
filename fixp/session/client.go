package session

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync/atomic"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store"
	"github.com/sylr/fix2/sofh"
	"github.com/sylr/fix2/transport"
)

// Client runs a client's session: one at a time, as FIXP binds a session
// to a connection the client dials.
type Client struct {
	cfg     ClientConfig
	d       transport.Dialer
	s       *Session
	running atomic.Bool
}

// NewClient returns a Client of the session its store holds that is not
// finalized, or of a new one, negotiated when Run starts. It fails if the
// store holds more than one, or cfg is invalid (*ConfigError).
func NewClient(cfg ClientConfig, d transport.Dialer) (*Client, error) {
	cfg, err := cfg.resolve()
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, &ConfigError{"Dialer", "required"}
	}
	s := newSession(cfg.Config, store.Client)
	ctx, cancel := s.storeCtx()
	defer cancel()
	states, err := cfg.Store.Sessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("fixp/session: listing the store's sessions: %w", err)
	}
	var mine []store.State
	for _, st := range states {
		if st.Role == store.Client {
			mine = append(mine, st)
		}
	}
	switch len(mine) {
	case 0:
		s.id = fixp.NewSessionID()
		s.state = StateUnnegotiated
		s.out, s.in = cfg.Flow, fixp.None
	case 1:
		st, err := cfg.Store.Open(ctx, mine[0].ID)
		if err != nil {
			return nil, fmt.Errorf("fixp/session: opening session %s: %w", mine[0].ID, err)
		}
		s.adopt(st)
	default:
		return nil, fmt.Errorf("fixp/session: the store holds %d client sessions not finalized, one at most", len(mine))
	}
	return &Client{cfg: cfg, d: d, s: s}, nil
}

// Session returns the client's session.
func (c *Client) Session() *Session { return c.s }

// retryError is a failed attempt Run makes again.
type retryError struct{ err error }

func (e *retryError) Error() string { return e.err.Error() }
func (e *retryError) Unwrap() error { return e.err }

func retry(err error) error { return &retryError{err} }

// Run negotiates the session if it is not, establishes it on a connection,
// and establishes it again on a new connection whenever one ends, waiting
// between attempts. It returns:
//
//   - nil once the session is finalized;
//   - a *fixp.NegotiationRejectedError, or a
//     *fixp.EstablishmentRejectedError whose code retrying cannot cure
//     (Unnegotiated, Credentials, KeepaliveInterval), or a store error;
//   - ctx's error once ctx is done, after terminating the established
//     connection (Terminate Finished, its answer awaited for one keepalive
//     interval at most).
//
// A negotiation that times out, or whose answer is not its request's, is
// tried again under a new session ID (§7.1.7.5, §7.1.7.6); one rejected
// with DuplicateId too. Run runs once.
func (c *Client) Run(ctx context.Context) error {
	if !c.running.CompareAndSwap(false, true) {
		return errors.New("fixp/session: Client.Run called twice")
	}
	s := c.s
	defer s.closeStore()
	wait := c.cfg.ReconnectMin
	for ctx.Err() == nil {
		established, err := c.connect(ctx)
		if serr := s.Err(); serr != nil {
			return serr
		}
		if s.State() == StateFinalized {
			return nil
		}
		// Both flows finished, the Terminate exchange lost, the server
		// retired it: the session is over.
		var rej *fixp.EstablishmentRejectedError
		if out, in := s.finishes(); out.Finished && in.Finished && errors.As(err, &rej) && rej.Code == fixp.EstablishmentUnnegotiated {
			s.finalize()
			if s.State() == StateFinalized {
				return nil
			}
			return s.Err()
		}
		var re *retryError
		if err != nil && !errors.As(err, &re) && ctx.Err() == nil {
			s.logWarn("stopping", "err", err)
			s.stop(err)
			return err
		}
		if err != nil {
			s.logInfo("connection attempt failed", "err", err)
		}
		if established {
			wait = c.cfg.ReconnectMin
		}
		jitter := time.Duration(rand.Int64N(int64(wait)/5 + 1))
		t := time.NewTimer(wait + jitter)
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
		if !established {
			wait = min(wait*2, c.cfg.ReconnectMax)
		}
	}
	s.stop(ctx.Err())
	return ctx.Err()
}

// connect dials, negotiates the session if needed, establishes it, and
// runs it until the connection ends. established tells it was.
func (c *Client) connect(ctx context.Context) (established bool, err error) {
	s := c.s
	dctx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	t, err := c.d.Dial(dctx)
	cancel()
	if err != nil {
		return false, retry(fmt.Errorf("fixp/session: dialing: %w", err))
	}
	// ctx ending during the handshake ends it.
	stop := context.AfterFunc(ctx, func() { t.Close() })
	r := sofh.NewReader(t, sofh.ReaderOptions{MaxLength: s.cfg.MaxFrameLength})
	if s.State() == StateUnnegotiated {
		if err := c.negotiate(ctx, t, r); err != nil {
			stop()
			t.Close()
			s.mu.Lock()
			if s.state == StateNegotiating {
				s.state = StateUnnegotiated
			}
			s.mu.Unlock()
			return false, err
		}
	}
	cn, err := c.establish(ctx, t, r)
	stop()
	if err != nil {
		t.Close()
		s.mu.Lock()
		if s.state == StateEstablishing {
			s.state = StateUnbound
		}
		s.mu.Unlock()
		return false, err
	}
	s.run(ctx, cn, "")
	return true, nil
}

// credentials returns the credentials of a request.
func (c *Client) credentials(ctx context.Context, establish bool, id fixp.SessionID, ts time.Time) ([]byte, error) {
	if c.cfg.Credentials == nil {
		return nil, nil
	}
	creds, err := c.cfg.Credentials(ctx, CredentialsRequest{Establish: establish, SessionID: id, Timestamp: ts})
	if err != nil {
		return nil, retry(fmt.Errorf("fixp/session: credentials: %w", err))
	}
	if len(creds) > 65534 {
		return nil, fmt.Errorf("fixp/session: credentials of %d bytes, 65534 at most", len(creds))
	}
	return creds, nil
}

// writeHandshake writes a frame of the handshake.
func (c *Client) writeHandshake(t transport.Conn, f []byte) error {
	t.SetWriteDeadline(time.Now().Add(c.s.cfg.WriteTimeout))
	if _, err := t.Write(f); err != nil {
		return retry(fmt.Errorf("fixp/session: writing: %w", err))
	}
	return nil
}

// readHandshake reads the next session message of the handshake, before
// deadline.
func (c *Client) readHandshake(t transport.Conn, r *sofh.Reader, deadline time.Time) (codec.MessageHeader, []byte, error) {
	t.SetReadDeadline(deadline)
	enc, msg, err := r.Next()
	if err != nil {
		if isTimeout(err) {
			return codec.MessageHeader{}, nil, retry(errors.New("fixp/session: no answer in time"))
		}
		return codec.MessageHeader{}, nil, retry(fmt.Errorf("fixp/session: reading: %w", err))
	}
	kind, h, _, err := codec.Classify(enc, msg)
	if err == nil && kind != codec.Session {
		err = fmt.Errorf("template %d of schema %d", h.TemplateId, h.SchemaId)
	}
	if err != nil {
		return h, nil, retry(fmt.Errorf("fixp/session: unexpected message in the handshake: %w", err))
	}
	return h, msg, nil
}

// negotiate negotiates the session on t (§4.2). An attempt that fails
// after Negotiate was sent leaves the ID behind: the next one takes a new
// one (§7.1.7.6).
func (c *Client) negotiate(ctx context.Context, t transport.Conn, r *sofh.Reader) (err error) {
	s := c.s
	s.mu.Lock()
	id := s.id
	s.state = StateNegotiating
	s.mu.Unlock()
	ts := time.Now()
	creds, err := c.credentials(ctx, false, id, ts)
	if err != nil {
		return err
	}
	sent := false
	defer func() {
		if err != nil && sent {
			s.mu.Lock()
			s.id = fixp.NewSessionID()
			s.mu.Unlock()
		}
	}()
	f := mustFrame(&codec.Negotiate{SessionId: id, Timestamp: nanos(ts), ClientFlow: codec.FlowType(c.cfg.Flow), Credentials: creds})
	err = c.writeHandshake(t, f)
	clear(f) // our copy of the credentials; the caller's are its own
	if err != nil {
		return err
	}
	sent = true
	deadline := time.Now().Add(s.cfg.HandshakeTimeout)
	for {
		h, msg, err := c.readHandshake(t, r, deadline)
		if err != nil {
			return err
		}
		switch h.TemplateId {
		case codec.NegotiationResponseTemplateID:
			var resp codec.NegotiationResponse
			if err := resp.Decode(msg); err != nil {
				return retry(fmt.Errorf("fixp/session: NegotiationResponse: %w", err))
			}
			if resp.SessionId != id || resp.RequestTimestamp != nanos(ts) {
				continue // not ours (§7.1.7.5)
			}
			server := fixp.FlowType(resp.ServerFlow)
			if !flowsAllowed(c.cfg.Flow, server) || len(c.cfg.ServerFlows) > 0 && !slices.Contains(c.cfg.ServerFlows, server) {
				return fmt.Errorf("fixp/session: the server's flow, %s, is not one we accept", server)
			}
			sctx, cancel := s.storeCtx()
			st, err := c.cfg.Store.Create(sctx, store.Meta{
				ID: id, Role: store.Client, ClientFlow: c.cfg.Flow, ServerFlow: server, NegotiatedAt: time.Now(),
			})
			cancel()
			if err != nil {
				return fmt.Errorf("fixp/session: recording session %s: %w", id, err)
			}
			s.adopt(st)
			s.logInfo("negotiated", "client_flow", c.cfg.Flow.String(), "server_flow", server.String())
			return nil
		case codec.NegotiationRejectTemplateID:
			var rej codec.NegotiationReject
			if err := rej.Decode(msg); err != nil {
				return retry(fmt.Errorf("fixp/session: NegotiationReject: %w", err))
			}
			if rej.SessionId != id || rej.RequestTimestamp != nanos(ts) {
				continue
			}
			e := &fixp.NegotiationRejectedError{Code: fixp.NegotiationRejectCode(rej.Code), Reason: printable(rej.Reason, s.cfg.MaxReasonLength)}
			if e.Code == fixp.NegotiationDuplicateID {
				return retry(e)
			}
			return e
		default:
			return retry(fmt.Errorf("fixp/session: unexpected session message in the negotiation, template %d", h.TemplateId))
		}
	}
}

// establish establishes the session on t (§4.3), and returns the
// connection to run it on.
func (c *Client) establish(ctx context.Context, t transport.Conn, r *sofh.Reader) (*conn, error) {
	s := c.s
	s.mu.Lock()
	id, out := s.id, s.out
	s.state = StateEstablishing
	s.mu.Unlock()
	ts := time.Now()
	creds, err := c.credentials(ctx, true, id, ts)
	if err != nil {
		return nil, err
	}
	est := codec.Establish{SessionId: id, Timestamp: nanos(ts), KeepaliveInterval: millis(s.cfg.KeepaliveInterval), Credentials: creds}
	if out.Sequenced() {
		// The next written: messages queued go out after the Establish.
		est.NextSeqNo = codec.Some(s.q.nextToWrite())
	}
	f := mustFrame(&est)
	err = c.writeHandshake(t, f)
	clear(f) // our copy of the credentials; the caller's are its own
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(s.cfg.HandshakeTimeout)
	for {
		h, msg, err := c.readHandshake(t, r, deadline)
		if err != nil {
			return nil, err
		}
		switch h.TemplateId {
		case codec.EstablishmentAckTemplateID:
			var ack codec.EstablishmentAck
			if err := ack.Decode(msg); err != nil {
				return nil, retry(fmt.Errorf("fixp/session: EstablishmentAck: %w", err))
			}
			if ack.SessionId != id || ack.RequestTimestamp != nanos(ts) {
				continue
			}
			if ack.KeepaliveInterval == 0 {
				return nil, retry(errors.New("fixp/session: EstablishmentAck with a keepalive interval of 0"))
			}
			cn := newConn(s, t, r, time.Duration(ack.KeepaliveInterval)*time.Millisecond)
			if ack.NextSeqNo.Valid {
				if err := cn.checkNextSeqNo(ack.NextSeqNo.Value); err != nil {
					// Established on the server's side: tell it why we go.
					var pe *protocolError
					reason := err.Error()
					if errors.As(err, &pe) {
						reason = pe.reason
					}
					c.writeHandshake(t, terminateFrame(id, fixp.TerminationUnspecifiedError, truncate(reason, s.cfg.MaxReasonLength)))
					return nil, retry(err)
				}
			}
			if err := c.recordEstablished(); err != nil {
				return nil, err
			}
			return cn, nil
		case codec.EstablishmentRejectTemplateID:
			var rej codec.EstablishmentReject
			if err := rej.Decode(msg); err != nil {
				return nil, retry(fmt.Errorf("fixp/session: EstablishmentReject: %w", err))
			}
			if rej.SessionId != id || rej.RequestTimestamp != nanos(ts) {
				continue
			}
			e := &fixp.EstablishmentRejectedError{Code: fixp.EstablishmentRejectCode(rej.Code), Reason: printable(rej.Reason, s.cfg.MaxReasonLength)}
			switch e.Code {
			case fixp.EstablishmentUnnegotiated, fixp.EstablishmentCredentials, fixp.EstablishmentKeepaliveInterval:
				return nil, e
			}
			return nil, retry(e)
		default:
			return nil, retry(fmt.Errorf("fixp/session: unexpected session message in the establishment, template %d", h.TemplateId))
		}
	}
}

// recordEstablished records the first establishment.
func (c *Client) recordEstablished() error {
	s := c.s
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	if !st.State().EstablishedAt.IsZero() {
		return nil
	}
	ctx, cancel := s.storeCtx()
	defer cancel()
	err := st.SetEstablished(ctx, time.Now())
	if err == nil {
		err = st.Sync(ctx)
	}
	if err != nil {
		err = fmt.Errorf("fixp/session: recording the establishment: %w", err)
		s.halt(err)
		return err
	}
	return nil
}
