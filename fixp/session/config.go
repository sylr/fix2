// Package session is fix2's FIXP 1.0 engine: clients and servers of
// point-to-point sessions over TCP, framed with SOFH, their session
// messages in SBE (docs/fixp.md).
//
// A Client negotiates a session, then establishes it on every connection
// it dials, re-establishing it after any disconnection until it is
// finalized. A Server accepts the sessions clients negotiate, and their
// establishment on any connection. Both run their sessions' flows:
// application messages, opaque SBE messages of any schema but FIXP's, go
// out through Session.Send and come in through Handler.OnMessage.
package session

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/store"
)

// Config is what a session's two roles share.
type Config struct {
	// Handler receives the session's events and messages. Required.
	Handler Handler
	// KeepaliveInterval is ours, sent in Establish by a client, in
	// EstablishmentAck by a server: we send a heartbeat in every interval
	// in which we sent nothing else (§4.5). 1s if 0.
	KeepaliveInterval time.Duration
	// Leniency multiplies the peer's keepalive interval: silence beyond it
	// ends the connection. 1.5 if 0.
	Leniency float64
	// HandshakeTimeout bounds the wait for NegotiationResponse and
	// EstablishmentAck on a client, for Negotiate and Establish on a
	// server's new connection: the keepalive interval if 0 (§7.1.7.6,
	// §7.2.5.9).
	HandshakeTimeout time.Duration
	// WriteTimeout bounds each write to the connection, 10s if 0: a peer
	// that stops reading is cut off.
	WriteTimeout time.Duration
	// StoreTimeout bounds each store call, 10s if 0: a store that does not
	// answer halts the session.
	StoreTimeout time.Duration
	// MaxFrameLength bounds the frames read, header included, 1 MiB if 0;
	// a longer one ends the connection.
	MaxFrameLength int
	// MaxPendingMessages and MaxPendingBytes bound the messages Send
	// queued and not yet written, 4096 and 16 MiB if 0: Send waits for
	// room, until its ctx is done. A message whose frame is longer than
	// MaxPendingBytes is refused (ErrTooLarge).
	MaxPendingMessages, MaxPendingBytes int
	// MaxHeldMessages and MaxHeldBytes bound the messages kept after a gap
	// on a Recoverable inbound flow, until it is filled, 4096 and 16 MiB if
	// 0: beyond, they are dropped and asked for again (§4.6.1). (M6)
	MaxHeldMessages, MaxHeldBytes int
	// RetransmitBatch bounds the messages of a Retransmission we send, and
	// of a RetransmitRequest we ask, 1000 if 0: a longer request is
	// answered in part, the peer asking for the rest (§4.6.2.2). (M6)
	RetransmitBatch int
	// MaxReasonLength bounds the reasons we send, longer ones truncated,
	// and those we log, 256 bytes if 0.
	MaxReasonLength int
	// Logger receives the session's events, never credentials; nothing is
	// logged if nil.
	Logger *slog.Logger
}

// ClientConfig configures a Client.
type ClientConfig struct {
	Config
	// Flow is the client's flow, sent in Negotiate. Its zero value is
	// Recoverable.
	Flow fixp.FlowType
	// ServerFlows are the server flows the client accepts; others end the
	// negotiation with an error. Any if empty, but None with a None client
	// flow (§3.4.4.1).
	ServerFlows []fixp.FlowType
	// Store keeps the session: the client resumes the one it holds that is
	// not finalized, and negotiates a new one otherwise. Required:
	// memstore.New() keeps nothing across restarts.
	Store store.Store
	// Credentials returns the credentials of a Negotiate or an Establish,
	// none if nil. It may sign the request (req.SessionID, req.Timestamp).
	// The session reads what it returns until the message is encoded, and
	// keeps none of it; it never logs it. An error fails the attempt, Run
	// trying again later.
	Credentials func(ctx context.Context, req CredentialsRequest) ([]byte, error)
	// ReconnectMin and ReconnectMax bound the wait between connection
	// attempts, which doubles after each failure, with jitter: 1s and 30s
	// if 0.
	ReconnectMin, ReconnectMax time.Duration
	// DialTimeout bounds each connection attempt, 10s if 0.
	DialTimeout time.Duration
}

// CredentialsRequest is a Negotiate or an Establish about to be sent.
type CredentialsRequest struct {
	Establish bool // false for a Negotiate
	SessionID fixp.SessionID
	Timestamp time.Time // its Timestamp, as sent
}

// ServerConfig configures a Server.
type ServerConfig struct {
	Config
	// Flow is the server's flow of the sessions it negotiates, sent in
	// NegotiationResponse. Its zero value is Recoverable. Sessions
	// negotiated before keep their flows (store.Meta).
	Flow fixp.FlowType
	// ClientFlows are the client flows the server accepts in a Negotiate;
	// others get NegotiationReject FlowTypeNotSupported. Any if empty, but
	// None with a None server flow.
	ClientFlows []fixp.FlowType
	// Store keeps the server's sessions: those not finalized can be
	// established again after a restart, and the IDs of finalized ones are
	// refused until forgotten (Retention). Required.
	Store store.Store
	// Authenticate checks a Negotiate or an Establish, and names its
	// owner: a stable identity of the authenticated party, such as an
	// account, not a credential. A Negotiate's owner is stored with the
	// session; an Establish whose owner is another is rejected with
	// Credentials, so that a session ID alone gives no one its session.
	// The owner may be logged. An error
	// rejects: a *fixp.NegotiationRejectedError or a
	// *fixp.EstablishmentRejectedError with its code and reason, any other
	// with Credentials and no reason, the error only logged. Required.
	Authenticate func(ctx context.Context, req *AuthRequest) (owner string, err error)
	// MinKeepalive and MaxKeepalive bound the keepalive interval a client
	// may ask for: 100ms and 1m if 0. Others get KeepaliveInterval.
	MinKeepalive, MaxKeepalive time.Duration
	// MaxClockSkew bounds the gap between a request's Timestamp and the
	// server's clock, 1m if 0 (§7.1.7.4).
	MaxClockSkew time.Duration
	// Retention is how long the IDs of finalized sessions are refused, 7
	// days if 0; negative keeps them for ever.
	Retention time.Duration
	// MaxSessions bounds the sessions not finalized, 1024 if 0: a Negotiate
	// beyond is rejected with Unspecified.
	MaxSessions int
	// UnestablishedTimeout retires a session negotiated and never
	// established within it, 1m if 0: a client that lost the
	// NegotiationResponse negotiates under a new ID, and the old one must
	// not hold a slot of MaxSessions. Its ID stays refused (Retention).
	UnestablishedTimeout time.Duration
	// MaxCredentialsLength bounds the credentials of a Negotiate or an
	// Establish, 1 KiB if 0: longer ones are rejected with Credentials.
	MaxCredentialsLength int
	// FirstMessageTimeout bounds the wait for a new connection's first
	// message, and its TLS handshake, the HandshakeTimeout if 0.
	FirstMessageTimeout time.Duration
	// MaxFirstFrameLength bounds a new connection's first frame, 4 KiB if
	// 0: nothing is known of the peer yet.
	MaxFirstFrameLength int
	// MaxPendingConnections bounds the connections waiting for their first
	// message, 1024 if 0: others are closed at once.
	MaxPendingConnections int
}

// AuthRequest is a Negotiate or an Establish a server authenticates.
type AuthRequest struct {
	Establish  bool // false for a Negotiate
	SessionID  fixp.SessionID
	Timestamp  time.Time
	ClientFlow fixp.FlowType // a Negotiate's; an Establish's is the session's
	// Credentials are borrowed for the call. They are never logged.
	Credentials []byte
	// Owner is, for an Establish, the session's owner, whom the request
	// must authenticate as.
	Owner string
	// RemoteAddr, TLS and Principal tell what the transport knows of the
	// peer: TLS is nil without TLS, Principal set by a
	// transport.PrincipalConn.
	RemoteAddr net.Addr
	TLS        *tls.ConnectionState
	Principal  any
}
