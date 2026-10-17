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
	// in which we sent nothing else (§4.5). 1s if 0; 1ms to 2^32-2 ms, the
	// wire's uint32 of milliseconds but its null.
	KeepaliveInterval time.Duration
	// KeepaliveLeniency multiplies the peer's keepalive interval: silence
	// beyond it ends the connection (§4.4 recommends "some leniency", and
	// sets none). 1.5 if 0; at least 1, finite, and its product with
	// MaxKeepalive a time.Duration.
	KeepaliveLeniency float64
	// MinKeepalive and MaxKeepalive bound the peer's keepalive interval,
	// 100ms and 1m if 0, 2^32-2 ms at most (§4.3.1: "Counterparties may agree on a valid range
	// for KeepaliveInterval"): a server rejects an Establish outside with
	// KeepaliveInterval; a client terminates on an EstablishmentAck
	// outside, and Run returns an error.
	MinKeepalive, MaxKeepalive time.Duration
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
	// answered in part, the peer asking for the rest (§4.6.2.2). (M6) At
	// most 2^32-2: a count's uint32 but its null.
	RetransmitBatch int
	// MaxReasonLength bounds the reasons we send, longer ones truncated,
	// and those we log, 256 bytes if 0.
	MaxReasonLength int
	// MaxStoredMessages is the retention target of a Recoverable flow: the
	// messages written before the last MaxStoredMessages are evicted from
	// the store, by batches of a tenth of it; the peer asking for them is
	// answered RetransmitReject OutOfRange "no longer available". Messages
	// queued and not written yet are never evicted. 0 keeps every message.
	MaxStoredMessages int
	// RejectRetransmitRequestBeyondSent answers a RetransmitRequest for
	// sequence numbers we never sent with RetransmitReject OutOfRange, as
	// FIXP's examples do (§7.4.5.1, §7.4.5.2); by default it terminates the
	// connection, ReRequestOutOfBounds, the request being a protocol
	// violation (§6.3, whose "should be immediately terminated" leaves the
	// choice).
	RejectRetransmitRequestBeyondSent bool
	// Observer, if set, sees every frame of an established connection.
	Observer Observer
	// Logger receives the session's events, never credentials; nothing is
	// logged if nil.
	Logger *slog.Logger
	// Tolerations, all off by default, depart from FIXP, SOFH and SBE as
	// some peers need (docs/conformance.md).
	Tolerations
}

// Tolerations depart from what FIXP 1.0, SOFH and SBE require, as some
// peers need: a session follows them strictly unless told otherwise, each
// departure its own field, all off by default. docs/conformance.md gives
// the rule each relaxes.
type Tolerations struct {
	// ToleratePeerWithoutKeepalives keeps the connection however long the
	// peer stays silent, as Silverflash, which sends no heartbeat, needs;
	// FIXP §4.5: "Each peer must send a heartbeat message during each
	// interval in which no application messages were sent." A dead peer is
	// then found by the transport only.
	ToleratePeerWithoutKeepalives bool
	// TolerateTrailingBytes accepts bytes after a session message within
	// its frame, beyond what its decoder consumed, when the message
	// header's version is not above the schema's, as Silverflash pads its
	// session messages. By default they make the message malformed, and
	// the connection terminates (§4.4); bytes after a newer version's
	// message may be its groups or data, and are refused either way.
	TolerateTrailingBytes bool
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
