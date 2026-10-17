# binancefix

A Go client of Binance Spot's
[FIX SBE](https://developers.binance.com/docs/binance-spot-api-docs/fix-api#fix-sbe)
order entry API: SBE requests and answers over TLS, on port 9002 of
`fix-oe.binance.com` (`fix-oe.testnet.binance.vision` for the testnet).
A library, and a command, [`binancefix`](cmd/binancefix), that checks
against the Spot testnet what the library relies on where Binance's
documentation is silent: all of it [settled](#settled-on-the-testnet) on
2026-10-17, the command [stays](#checking-against-the-testnet) as the
regression check.

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
- refuses a LogonAck with a required field null or empty (HeartBtInt,
  SbeSchemaIdVersionDeprecated, UUID), a HeartBtInt outside 5 to 60
  seconds, or a deprecation flag neither true nor false: the Logon fails
  as a refused one (`ErrInvalidLogonAck`); logged on, ends the connection
  on a TestRequest without TestReqID (`ErrMissingRequired`), while a News
  without Headline, a notice, is told of, its countdown unknown. The codecs
  decode such values as they are;
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

## Settled on the testnet

Where Binance's documentation is silent, the client relied on
assumptions. All are settled: `binancefix logon --signature-time auto`,
then `binancefix probe --price-far 70000` (BTCUSDT at about 81,500), ran
against the Spot testnet on 2026-10-17: 8 confirmed, 0 contradicted, 0
inconclusive, the cleanup cancelling what was left (the amended order, two
cancel/replace new orders), nothing left open. What Binance sent, by probe
(see [the probes](#probe-the-answers-to-requests)):

- M0, the text of the Logon's SendingTime in the signed payload, which
  Binance does not document for SBE (its SBE sample's sendingTime,
  09:17:25.223 UTC, is two hours before the time its signature was made
  of, 11:17:25.223): the FIX form in UTC at **microsecond** precision, the
  SBE field's own (`SignatureTimeMicros`, `SignatureTime`'s zero value),
  the Logon's time truncated to the microsecond. Binance accepted the
  Logon signing `20261017-18:58:01.373000` and refused the millisecond
  form (`20261017-18:58:01.373`) with error -1022.
- (a) The LogonAck is numbered 1.
- (b) A limit order: ExecutionReport NEW. Its cancellation: ExecutionReport
  CANCELED carrying the cancellation's ClOrdID, the order's as
  OrigClOrdID.
- (c) An OrderAmendKeepPriorityRequest: ExecutionReport ExecType REPLACED,
  OrdStatus NEW, carrying the amendment's ClOrdID, the amended order's as
  OrigClOrdID; the order goes by the amendment's ClOrdID from then on (a
  later cancellation's CANCELED carries it as OrigClOrdID).
- (d) Cancel/replace, its cancellation accepted (STOP_ON_FAILURE):
  CANCELED carrying the CancelClOrdID, then the new order's NEW.
  STOP_ON_FAILURE, its cancellation refused: OrderCancelReject carrying
  the CancelClOrdID, -2011 "Unknown order sent.", then the new order's
  ExecutionReport REJECTED -2010 "Unable to cancel order, order placement
  not attempted." (it finds the request answered already). ALLOW_FAILURE,
  its cancellation refused: the new order's NEW came **before** the
  OrderCancelReject (-2011): the legs are answered on their own, in either
  order.
- (e) An OTO NewOrderList: ListStatus EXEC_STARTED/EXECUTING, then
  ExecutionReports NEW (the working order, OrdStatus NEW; the pending one,
  OrdStatus PENDING_NEW). Its cancellation (by OrigClListID): a ListStatus
  ALL_DONE/ALL_DONE carrying the cancellation's ClOrdID as ClListID
  **first**, then an ExecutionReport CANCELED per order carrying the
  cancellation's ClOrdID: the client's rule holds, a ListStatus never
  answers a cancellation, the CANCELED do.
- (f) A market order filled at once: NEW, then TRADE (OrdStatus FILLED).
- (g) An OrderMassCancelRequest: the cancelled order's ExecutionReport
  CANCELED carrying the mass cancellation's ClOrdID (the order's as
  OrigClOrdID), then the OrderMassCancelReport (TotalAffectedOrders 1)
  carrying its ClOrdID, which answers it.
- (h) A LimitQuery: LimitResponse carrying its ReqID.
- Filters, from an earlier run: a price outside `PERCENT_PRICE_BY_SIDE` is
  refused by an ExecutionReport REJECTED -1013 "Filter failure:
  PERCENT_PRICE_BY_SIDE"; an OTO list so refused gets a ListStatus
  ALL_DONE/REJECT with its orders' errors, then an ExecutionReport
  REJECTED per order. The testnet's BTCUSDT bands, around the 5-minute
  average price: bids from 0.5× to 1.2×, asks from 0.8× to 2×.

Not probed: a Reject of a request carrying its seqNum as RefSeqNum (no
probe provokes one), and the LogonAck's HeartBtInt mirroring ours (if it
differs, Binance's counts; `logon` shows it). The fake Binance of the
tests (`internal/fakebinance`) answers as the testnet did, in the same
order, and `legs_test.go` checks the client's receipts on those orders.

## Checking against the testnet

`cmd/binancefix` logs on to the testnet and runs scripted, bounded
exchanges ("probes"), each judging what the client relies on, settled
above, should Binance change it:

| Verdict | Meaning |
|---|---|
| confirmed | Binance answered as the client assumes: nothing to change |
| contradicted | a message Binance sent shows another behaviour: the client leaves a request unanswered, or answers it wrongly; it is to change (below) |
| inconclusive | the case did not arise: a refusal (a filter's, -1013, says to adjust `--price-far` or `--qty`; a balance's), a whole request refused by a Reject, a -1007 TIMEOUT, no answer within `--wait`, the connection lost, an interrupt. Fix the cause and run it again |

A missing answer is never a contradiction, only a timeout: a verdict other
than inconclusive rests on messages seen. One check rests on an absence,
by design, and says no more than it saw: STOP_ON_FAILURE's refused
cancellation gets an OrderCancelReject, its new order an ExecutionReport
REJECTED -2010 "order placement not attempted" (as Binance sent on
2026-10-17), and the probe confirms "no placement observed during
`--wait`": over that bounded window, on an uninterrupted connection, no
report of the new order but a REJECTED. An ExecutionReport NEW, TRADE or
other, or a ListStatus naming it, proves it executed: contradicted, even
after a -1007; a -1007 alone, the connection lost or the run interrupted
during the window: inconclusive. An absence cannot prove the order was
never attempted.

### A testnet key

The FIX API takes Ed25519 keys only. Make a key pair:

    openssl genpkey -algorithm ed25519 -out ed25519-private.pem
    openssl pkey -in ed25519-private.pem -pubout -out ed25519-public.pem

Log in to [testnet.binance.vision](https://testnet.binance.vision) (with a
GitHub account), register `ed25519-public.pem` as an Ed25519 API key, and
make sure it has the `FIX_API` permission (order entry refuses keys
without it). Keep the API key shown, then:

    export BINANCE_API_KEY=<the API key> BINANCE_PRIVATE_KEY=$PWD/ed25519-private.pem

The testnet's funds and orders are not real; it is reset from time to
time, keys included.

### logon: the signature (M0, settled)

    make                                   # dist/binancefix
    dist/binancefix logon                  # the client's default, us
    dist/binancefix logon --signature-time auto --heartbeat 5s

`logon` logs on to the order entry endpoint, prints the messages
exchanged, the LogonAck (its seqNum, HeartBtInt, UUID, deprecation flag)
or the Reject or Logout refusing it, then logs out: a check that the key,
the clock and the signature are right. `--signature-time` chooses the text
of the SendingTime signed: `us` (`20240627-11:17:25.223000`, the default,
which Binance checks), or, as diagnostics should Binance change, `ms`
(`…25.223`, refused with -1022 on 2026-10-17), `s` (`…25`), `ns`
(`…25.223000000`), `unix-us` (`1719487045223000`, the SBE field itself) or
`unix-ms`; `auto` tries them in that order until one is accepted, waiting twice
HeartBtInt between attempts (a refused SenderCompID's cooldown; 10 seconds
with `--heartbeat 5s`, at most 6 attempts, far within the 15 connection
attempts per 30 seconds Binance allows), and stops early on a refusal
naming another cause (bad API key or permissions, rate limits, clock skew:
error codes -1002, -1003, -1021, -2014, -2015). It exits 0 when the
default, `us`, is accepted, 3 when another form is (Binance changed:
make it the default, below), 1 when none is. Without `--production`,
`logon` refuses any endpoint but the testnet's; with it, it may log on to
`fix-oe.binance.com:9002` (nothing else: no order is sent).

### probe: the answers to requests

    dist/binancefix probe --price-far 70000              # all of them
    dist/binancefix probe --price-far 70000 order amend  # some, by name or letter
    dist/binancefix probe --price-far 70000 --json > probe.json
    dist/binancefix probe fill limits                    # no limit order: no --price-far
    dist/binancefix probes                               # the list, with the assumptions

| | Probe | What it sends | Confirmed when | Contradicted when |
|---|---|---|---|---|
| a | `logon-seqnum` | the Logon | the LogonAck is numbered 1 | it is numbered otherwise |
| b | `order` | a limit buy far below the market, then its cancellation | ExecutionReport NEW answers the order; ExecutionReport CANCELED carrying the cancellation's ClOrdID answers the cancellation | the order's first ExecutionReport is neither NEW nor REJECTED; the CANCELED carries another ClOrdID |
| c | `amend` | a limit order, then an OrderAmendKeepPriorityRequest halving its quantity | ExecutionReport REPLACED carrying the amendment's ClOrdID | another ExecutionReport (NEW...) answers it, or the answer carries another ClOrdID |
| d | `cancel-replace` | three OrderCancelRequestAndNewOrderSingle: STOP_ON_FAILURE cancelling a live order; STOP_ON_FAILURE and ALLOW_FAILURE cancelling an order that never was | 1: CANCELED (CancelClOrdID) then NEW; 2: OrderCancelReject, the new order's REJECTED -2010 "order placement not attempted", no placement observed during `--wait`, the connection kept; 3: OrderCancelReject and the new order's NEW, in either order (Binance sends the NEW first) | an answer carries other IDs; 2: a report of the new order (NEW, TRADE...) or a ListStatus naming it; a first ExecutionReport neither NEW nor REJECTED (a Reject of the whole request, nothing within `--wait`, or the connection lost: inconclusive) |
| e | `list-cancel` | an OTO list (a limit buy far below, a pending sell), then its cancellation by ClListID | ListStatus answers the list; ExecutionReports CANCELED carrying the cancellation's ClOrdID answer its cancellation | the CANCELED carry the orders' ClOrdIDs, or a ListStatus alone answers |
| f | `fill` | a market buy of `--qty`, then a market sell of what it bought | NEW first, then TRADE | TRADE (or EXPIRED) without NEW first |
| g | `mass-cancel` | a limit order, then an OrderMassCancelRequest of the symbol | OrderMassCancelReport carrying its ClOrdID; the order's CANCELED (whose ClOrdID it shows) | the report carries another ClOrdID |
| h | `limits` | a LimitQuery | LimitResponse carrying its ReqID (its limits shown) | it carries another |

Each probe prints its checks and the messages of its exchange (`→` ours,
`←` Binance's, `[answers X]` when the client matched the message to the
request of ID X). It exits 0 when all are confirmed, 3 when one is not, 1
on failure (the Logon refused, the connection lost, orders or a position left).

`--symbol` (BTCUSDT), `--price-far` and `--qty` (0.001) shape the orders.
`--price-far` has no default, the market moving: the probes resting limit
orders (`order`, `amend`, `cancel-replace`, `list-cancel`, `mass-cancel`)
refuse to run without it (a usage error, exit 2); `logon-seqnum`, `fill`
(a market order) and `limits` run without it. Pick it:

- below the market, so that the buys rest, never filled;
- inside both `PERCENT_PRICE_BY_SIDE` bands of the symbol, around its
  average price (`avg`): bids from `bidMultiplierDown`×avg to
  `bidMultiplierUp`×avg, asks from `askMultiplierDown`×avg to
  `askMultiplierUp`×avg (the list's pending order sells at it too). The
  filters are in
  `https://testnet.binance.vision/api/v3/exchangeInfo?symbol=BTCUSDT`, the
  average in `https://testnet.binance.vision/api/v3/avgPrice?symbol=BTCUSDT`;
  on 2026-10-17 BTCUSDT's bands were 0.5×–1.2× (bids) and 0.8×–2× (asks):
  between 0.8× and 1× the average, such as 70000 at about 81,500.

Out of the bands, Binance refuses the orders with -1013 "Filter failure:
PERCENT_PRICE_BY_SIDE": the probes say inconclusive, and to adjust. The
notional (`--qty` × `--price-far`) must stay above the symbol's minimum
(BTCUSDT: 5 USDT). The precision of the prices and quantities is the
digits given (`70000.00`: exponent -2). `--signature-time` (`us`) is
`logon`'s.

Safety:

- testnet only: `probe` refuses any endpoint but
  `fix-oe.testnet.binance.vision`, `--production` or not, and so does the
  probe package itself;
- small orders far from the market; `mass-cancel` cancels every order of
  the account on `--symbol`: use a symbol where the testnet account has
  nothing else open;
- cleanup: after each probe, and once more at the end, every order it
  placed and did not see done is cancelled (by OrderID once known), also
  on error, on `--timeout` and on SIGINT (a second SIGINT kills); the
  connection lost, it logs on again under another SenderCompID (Binance
  refuses the first for twice HeartBtInt) to cancel them. An order
  answered with -1007 (status unknown) is never taken for done: it is
  cancelled all the same, an "unknown order" answer settling it;
- `fill`'s position: each of its market orders counts by its own reports,
  the largest CumQty they carry, late ones included; their sum is the net
  quantity, which the probe trades back with a market order, and the
  cleanup again, up to 3 times, on the cleanup's connection, interrupted
  or not, refused or partly filled before. An order's fills are known once
  a report tells its final status (not -1007); before anything is traded
  back, each order not final is resolved: its final report waited for
  `--wait`, else the order cancelled, and, Binance knowing it no more
  (-2011: filled or expired unseen), its late reports waited for `--wait`
  again. One left unresolved, nothing more is traded (an unseen fill would
  be traded twice) and the exposure is reported unknown. What may still be
  open, or not traded back (its exact quantity, or the orders whose fills
  are unknown), is printed as a WARNING, to settle on the testnet's site,
  and the command exits 1;
- rate: one request per `--interval` (200ms) at most, 100 requests per run
  at most, against Binance's 10,000 messages per 10 seconds; one
  connection (two if it is lost), against 15 attempts per 30 seconds and
  10 at once; `--wait` (10s, Binance's own timeout) per answer, `--settle`
  (1s) for late messages, `--timeout` (5m) in all, the cleanup apart.

### Opt-in tests

The same probes run as Go tests, never by default nor in CI:

    BINANCEFIX_TESTNET=1 BINANCEFIX_PRICE_FAR=70000 go test -count=1 -run Testnet -v .   # or make test-testnet

with `BINANCE_API_KEY` and `BINANCE_PRIVATE_KEY` set, `BINANCEFIX_PRICE_FAR`
for `TestTestnetProbes` (picked as `--price-far`; the test fails, telling
how, without it), and optionally `BINANCEFIX_SIGNATURE_TIME`,
`BINANCEFIX_SYMBOL` and `BINANCEFIX_QTY`. `TestTestnetLogon` fails if the Logon, signed in
the default form, `us`, is refused, or the LogonAck numbered otherwise
than 1; `TestTestnetProbes` runs a subtest
per probe: confirmed passes, contradicted fails, inconclusive is skipped.

### Should Binance change

A verdict other than confirmed, once all were (2026-10-17), means Binance
changed: the client changes where it relied on the settled behaviour, its
tests (`legs_test.go`, `signature_test.go`, the fake of `fake_test.go`,
`internal/fakebinance`) first, then the record above:

- M0, should Binance change (`logon --signature-time auto` exits 3): make
  the form it accepts `SignatureTime`'s zero value (its constant first in
  `signature.go`'s list, `signedSendingTime` its form, `logonTime`'s
  truncation its precision), `checkLogon`'s default in `fake_test.go` and
  the fake's in `internal/fakebinance` too, as was done for `us`.
- (a): `newConn`'s `nextIn`, 1, becomes Binance's first number, or the
  LogonAck's number is taken as the start.
- (b), (c), (d), (e): in `answer.go`, `answerIDs` gives the IDs a message
  is matched by and `Receipt.apply` the leg it answers: add the ClOrdID
  Binance puts (the order's, `OrigClOrdID`...), the ExecType that answers
  an amendment, or, for (e), a ListStatus answering the cancellation of
  the list it names (`LegCancel` then needs the ClListID; mind the
  ListStatus of an earlier fill). (d)'s STOP_ON_FAILURE contradicted: the
  new order is to be awaited (`stopOnFailure`).
- (f): `Receipt.apply` takes a first TRADE (or EXPIRED) of a new order as
  its acceptance.
- (g), (h): the report and the LimitResponse are matched otherwise (the
  one request of its kind awaiting an answer).

## Tests

`make test` (or `go test -race ./...`) runs the client against a fake
Binance over in-memory connections: Logon and signature (each
`SignatureTime` form verified as the fake would), heartbeats and
TestRequests, gaps, Logouts both ways, Reject and News, receipts at every
phase of a connection, framing, a stalled Handler, Send from the Handler,
sequence number rollover, reconnection pacing. Nothing reaches the network.

Conformance: frames split in two reads at every byte boundary, and many in
one read; malformed frames (encodingType, lengths short, long or beyond
`MaxFrameLength`, a message short of its header, another schema) and
streams ending in a frame; a Handler blocked for several HeartBtInts,
heartbeats and TestRequest answers going on; a write failing at every
byte of a request's frame; the connection lost at each phase of a request;
Binance's documented Logon read through the client's framing and codecs.
A fake Binance answers with messages Real Logic's SBE codecs encoded
(`testdata/reallogic`, a copy of fix2's
`internal/gen/sbe/testdata/reallogic`, which tells how they are made):
the client hands them to the Handler with Real Logic's values, matches
its receipts by them, and its requests, of the same values, are Real
Logic's bytes.

The probes and the command are tested offline as well:
`internal/fakebinance`, an in-memory Binance that checks the Logon's
signature in the forms it is told to accept and answers each request as
told, the way the client assumes or otherwise, plays every probe both
ways, so that each verdict is seen confirmed, contradicted and
inconclusive. The command's tests cover key loading (flags, file,
environment), the refusal of non-testnet endpoints before any dial,
`--signature-time auto`'s attempts and cooldowns, the JSON output, the
cleanup on interrupt and after a lost connection, and the pacing of
requests. Only the opt-in tests above reach the testnet.
