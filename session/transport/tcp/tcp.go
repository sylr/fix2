// Package tcp provides FIX session connections over TCP, with or without TLS.
// FIX over TLS (FIXS) uses a port of its own: a plain connection is never
// upgraded.
package tcp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/sylr/fix2/session/transport"
)

// Dialer dials a counterparty over TCP, trying its addresses in turn: each
// Dial starts with the address after the one that last failed, so an
// initiator reconnecting after a failure tries the next one. Backoff between
// attempts is the initiator's.
type Dialer struct {
	// Addresses are "host:port" addresses, tried in order.
	Addresses []string
	// TLS, if set, makes connections TLS clients; ServerName defaults to
	// the host of the address dialed.
	TLS *tls.Config
	// Net dials the TCP connections; the zero net.Dialer if nil.
	Net *net.Dialer

	mu   sync.Mutex
	next int
}

var _ transport.Dialer = (*Dialer)(nil)

// Dial implements transport.Dialer. It returns the errors of every address
// tried, joined, if none answers.
func (d *Dialer) Dial(ctx context.Context) (transport.Conn, error) {
	if len(d.Addresses) == 0 {
		return nil, errors.New("tcp: Dialer has no address")
	}
	nd := d.Net
	if nd == nil {
		nd = &net.Dialer{}
	}
	d.mu.Lock()
	start := d.next
	d.mu.Unlock()
	var errs []error
	for i := range d.Addresses {
		k := (start + i) % len(d.Addresses)
		addr := d.Addresses[k]
		c, err := d.dialOne(ctx, nd, addr)
		if err == nil {
			d.mu.Lock()
			d.next = k
			d.mu.Unlock()
			return c, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	d.mu.Lock()
	d.next = (start + len(errs)) % len(d.Addresses)
	d.mu.Unlock()
	return nil, errors.Join(errs...)
}

func (d *Dialer) dialOne(ctx context.Context, nd *net.Dialer, addr string) (transport.Conn, error) {
	c, err := nd.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcp: dial %s: %w", addr, err)
	}
	if d.TLS == nil {
		return c, nil
	}
	cfg := d.TLS.Clone()
	if cfg.ServerName == "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("tcp: dial %s: %w", addr, err)
		}
		cfg.ServerName = host
	}
	tc := tls.Client(c, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("tcp: TLS handshake with %s: %w", addr, err)
	}
	return tc, nil
}

// Listener accepts TCP connections, TLS ones if configured. A TLS
// connection's handshake runs on its first read or when the acceptor calls
// HandshakeContext, never in Accept, so a slow client cannot hold up others.
type Listener struct {
	l   *net.TCPListener
	tls *tls.Config

	mu sync.Mutex // serializes Accept, which moves the listener's deadline
}

var _ transport.Listener = (*Listener)(nil)

// Listen listens on addr ("host:port", ":0" for any port). With a non-nil
// tlsConfig, accepted connections are TLS servers.
func Listen(addr string, tlsConfig *tls.Config) (*Listener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcp: listen %s: %w", addr, err)
	}
	return &Listener{l: l.(*net.TCPListener), tls: tlsConfig}, nil
}

// Accept implements transport.Listener.
func (l *Listener) Accept(ctx context.Context) (transport.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { l.l.SetDeadline(time.Unix(1, 0)) })
	c, err := l.l.Accept()
	if !stop() {
		l.l.SetDeadline(time.Time{})
		if c != nil {
			c.Close()
		}
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if l.tls != nil {
		return tls.Server(c, l.tls), nil
	}
	return c, nil
}

// Addr implements transport.Listener.
func (l *Listener) Addr() net.Addr { return l.l.Addr() }

// Close implements transport.Listener.
func (l *Listener) Close() error { return l.l.Close() }
