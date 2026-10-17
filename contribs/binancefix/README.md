# binancefix

A Go client of Binance Spot's
[FIX SBE](https://developers.binance.com/docs/binance-spot-api-docs/fix-api#fix-sbe)
order entry API: SBE requests and answers over TLS, on port 9002 of
`fix-oe.binance.com` (`fix-oe.testnet.binance.vision` for the testnet).
A library: no command yet.

FIX SBE is Binance's own protocol: its documentation is the specification
here, out of the scope of fix2's strictness rule
([docs/conformance.md](../../docs/conformance.md)). The codecs are generated
by `fix2gen sbe` from Binance's schema, vendored and pinned
([internal/fixsbe](internal/fixsbe)), the same as fix2's
`internal/gen/sbe/binance`, and exported by the package through aliases
([codecs.go](codecs.go)). `go generate ./...` (`make generate`) regenerates
both; nothing is downloaded.

```go
key, err := binancefix.LoadPrivateKey("ed25519-private.pem")
c, err := binancefix.New(binancefix.Config{
	Endpoint:     binancefix.TestnetEndpoint,
	APIKey:       os.Getenv("BINANCE_API_KEY"),
	PrivateKey:   key,
	SenderCompID: "OE1",
	Handler:      handler, // embeds binancefix.BaseHandler
})
go c.Run(ctx)
// Once OnLogon was called:
r, err := c.Send(&binancefix.NewOrderSingle{ClOrdID: []byte("o-1"), Symbol: []byte("BTCUSDT"), ...})
state, err := r.Wait(ctx) // Answered, StatusUnknown, NeverWritten or PossiblyTransmitted
```

## Session

[doc.go](doc.go) has the details. In short, each connection:

- logs on with a Logon whose seqNum (1, with ResetSeqNumFlag: Binance
  resets every Logon) and sendingTime are set once, then signed with
  Ed25519 (`Username` the API key, `RawData` the signature);
- numbers session messages and requests in one series, from 2, rolling
  over to 0 after 4,294,967,295, and stamps each sendingTime when writing;
- ends on a gap or a repeat in Binance's numbers, after our Logout:
  Binance resends nothing;
- heartbeats after 7/8 of HeartBtInt without writing, answers TestRequests,
  sends a TestRequest after HeartBtInt without reading, and ends the
  connection if it is not answered within another HeartBtInt;
- answers Binance's Logout, and logs out on `Stop`, `Reconnect` or Run's
  context, waiting `LogoutTimeout` for the answer;
- writes session messages first, from a queue of their own: a full request
  queue never delays a Heartbeat or a Logout.

Run reconnects with a doubling backoff, at most 15 attempts in 30 seconds
(Binance's order entry limit), and waits twice HeartBtInt after a session
that ended without our Logout (Binance refuses its SenderCompID that
long, ours possibly logged on unseen included). A refused Logon is
retried after twice HeartBtInt, `MaxRefusals` (3) times in a row at most:
a refusal may pass (that cooldown, a connection limit) or not (bad
credentials). News, Binance's maintenance countdown, reaches `OnNews`;
`Reconnect(e.Epoch)` moves to another server with a clean Logout, and does
nothing if that connection is already gone.

## Requests are never resent

There is no store: `Send` queues a request on the logged-on connection (it
never waits: `ErrQueueFull`, `ErrNotLoggedOn`), and its `Receipt` tells
its fate:

| State | Meaning | What to do |
|---|---|---|
| `Queued`, `Written` | waiting to be written, written | wait |
| `Answered` | every leg answered (accepted or refused, see `Legs`) | nothing |
| `StatusUnknown` | answered with -1007 TIMEOUT: "execution status unknown" | reconcile |
| `NeverWritten` | the connection ended (or our Logout was queued) before a byte of it was written | safe to send again |
| `PossiblyTransmitted` | the connection ended after it was written, or partly written, before an answer | reconcile, then decide |

A request belongs to its connection (`Epoch`); its state is final once the
connection ends, and `OnLogout` lists the connection's `NeverWritten` and
`PossiblyTransmitted` requests. A request has legs (`Receipt.Legs`), each
answered on its own, by ID and by the kind of answer its kind takes: an
order by an ExecutionReport NEW or REJECTED (or its ListStatus entry), a
cancellation by CANCELED or an OrderCancelReject, an amendment by REPLACED
or an OrderAmendReject (never by its order's fills), a list by its
ListStatus, a mass cancellation and a limits query by their reports; a
Reject (by RefSeqNum) refuses all. A cancel/replace
(OrderCancelRequestAndNewOrderSingle, its `CancelClOrdID` required) is
answered once both its cancellation and its new order are. The request is
answered when all its legs are; a -1007 on any makes it `StatusUnknown`.

## Handler

One goroutine calls the Handler, in order: a connection's `OnLogon`, its
messages, its `OnLogout`, then the next connection's. Its methods may call
`Send`, `Stop` and `Reconnect`; they must not wait for `Run`, nor for a
`Receipt` (its answer may be queued behind the call). The messages
they get are valid during the call only. Up to `MaxEvents` received
messages wait for a slow Handler; beyond, reading waits, and Binance's
TestRequests with it. Our heartbeats never wait for the Handler.

## Assumptions to settle against the testnet

- The text of the Logon's SendingTime in the signed payload: the FIX form
  in UTC at millisecond precision, `20240627-11:17:25.223`, the Logon's
  time truncated to the millisecond (`signedSendingTime`). Binance does not
  document it for SBE, and its SBE sample's sendingTime (09:17:25.223 UTC)
  is two hours before the time its signature was made of (11:17:25.223).
- Binance's first message, the LogonAck, is numbered 1.
- The ExecutionReport answering an OrderCancelRequest carries the cancel's
  ClOrdID; a Reject of a request carries its seqNum as RefSeqNum.
- An amendment's success is an ExecutionReport of ExecType REPLACED; a new
  order always gets an ExecutionReport NEW or REJECTED first, even filled
  at once (otherwise it stays unanswered: PossiblyTransmitted at the end);
  the OrderCancelReject of a cancel/replace carries its CancelClOrdID or
  its ClOrdID.
- The LogonAck's HeartBtInt mirrors ours (if it differs, Binance's counts).

## Tests

`make test` (or `go test -race ./...`) runs the client against a fake
Binance over in-memory connections: Logon and signature, heartbeats and
TestRequests, gaps, Logouts both ways, Reject and News, receipts at every
phase of a connection, framing, a stalled Handler, Send from the Handler,
sequence number rollover, reconnection pacing. Nothing reaches the network.
