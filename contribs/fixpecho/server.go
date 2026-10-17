package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/session"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/transport"
)

// serverOptions configures the echo server.
type serverOptions struct {
	flow        fixp.FlowType   // the server's flow
	clientFlows []fixp.FlowType // those it accepts
	token       string          // the credentials required, any if empty
	keepalive   time.Duration
	echoWait    time.Duration // how long an echo waits for room in the queue
	maxPending  int           // the session's queue, in messages, the engine's default if 0
	logger      *slog.Logger
}

// echo answers each application message with itself. Once the client
// finished its flow, the session finishes the server's, after the echoes.
type echo struct {
	session.BaseHandler
	wait time.Duration
}

// OnMessage echoes msg. A callback's Send does not wait for room in the
// queue: when it is full, the echo waits for it, at most e.wait, holding
// back the connection's reads, and so the client. Past that, the
// connection terminates: on the client's Recoverable flow the message is
// asked for again on the next connection; on another it was taken, and is
// not echoed.
func (e *echo) OnMessage(ctx context.Context, s *session.Session, msg []byte, _ session.Inbound) error {
	_, err := s.SendRaw(ctx, msg)
	if !errors.Is(err, session.ErrQueueFull) {
		return err
	}
	wctx, cancel := context.WithTimeout(context.Background(), e.wait)
	defer cancel()
	if _, err := s.SendRaw(wctx, msg); err != nil {
		return fmt.Errorf("echo: %w", err)
	}
	return nil
}

// newServer returns the echo server, for its listeners.
func newServer(o serverOptions) (*session.Server, error) {
	if o.flow == fixp.None {
		return nil, errors.New("an echo server needs a flow to echo on, not None")
	}
	token := []byte(o.token)
	return session.NewServer(session.ServerConfig{
		Config: session.Config{
			Handler:            &echo{wait: o.echoWait},
			KeepaliveInterval:  o.keepalive,
			MaxPendingMessages: o.maxPending,
			Logger:             o.logger,
		},
		Flow:        o.flow,
		ClientFlows: o.clientFlows,
		Store:       memstore.New(),
		Authenticate: func(_ context.Context, req *session.AuthRequest) (string, error) {
			if len(token) > 0 && subtle.ConstantTimeCompare(req.Credentials, token) != 1 {
				return "", errors.New("bad credentials")
			}
			return "fixpecho", nil
		},
	})
}

// serve runs the echo server on l until ctx is done, then terminates its
// sessions; it closes l.
func serve(ctx context.Context, o serverOptions, l transport.Listener) error {
	srv, err := newServer(o)
	if err != nil {
		l.Close()
		return err
	}
	srv.Listen(l)
	if err := srv.Run(ctx); !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
