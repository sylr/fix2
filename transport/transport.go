// Package transport defines the connections sessions run over, whatever
// their protocol.
//
// Sessions over a stream, such as classic FIX sessions (FIX 4.x and FIXT
// 1.1), need a reliable, ordered byte stream: a lost or reordered byte is a lost or garbled message, which the
// session layer cannot tell from a counterparty error. TCP, TLS, QUIC streams
// and unix sockets provide one; raw UDP does not, and no adapter for it is
// offered. Any net.Conn is a Conn.
package transport

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"time"
)

// Conn is a connection carrying one session. Read and Write may be
// called concurrently, from one goroutine each; Close unblocks both.
type Conn interface {
	io.Reader
	io.Writer
	io.Closer
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
}

// Dialer opens connections to a counterparty, for an initiator.
type Dialer interface {
	Dial(ctx context.Context) (Conn, error)
}

// Listener accepts connections from counterparties, for an acceptor.
type Listener interface {
	// Accept waits for the next connection, until ctx is done.
	Accept(ctx context.Context) (Conn, error)
	Addr() net.Addr
	Close() error
}

// TLSConn is a Conn over TLS, whose peer certificates an acceptor can match
// against the sessions it serves.
type TLSConn interface {
	Conn
	// HandshakeContext runs the TLS handshake, if not done yet.
	HandshakeContext(ctx context.Context) error
	ConnectionState() tls.ConnectionState
}

// PrincipalConn is a Conn whose transport authenticated its peer, such as a
// QUIC or proxy connection, and tells who it is.
type PrincipalConn interface {
	Conn
	Principal() any
}

// DialerFunc adapts a function to a Dialer.
type DialerFunc func(ctx context.Context) (Conn, error)

// Dial calls f.
func (f DialerFunc) Dial(ctx context.Context) (Conn, error) { return f(ctx) }
