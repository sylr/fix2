# fix2

[![Go](https://github.com/sylr/fix2/actions/workflows/go.yaml/badge.svg)](https://github.com/sylr/fix2/actions/workflows/go.yaml)
[![codecov](https://codecov.io/gh/sylr/fix2/graph/badge.svg)](https://codecov.io/gh/sylr/fix2)
[![pkg.go.dev](https://pkg.go.dev/badge/github.com/sylr/fix2)](https://pkg.go.dev/github.com/sylr/fix2)

A FIX protocol library for Go: a zero-copy parser, validating dictionaries,
typed code generation and a session engine.

The FIX tagvalue encoding and its session layer (FIX 4.x, FIXT 1.1) live
under `fix/`; what other protocols can share, such as `transport`, lives at
the root.

- **Parsing** (`fix`): `Parse` and `ParseInto` read messages without copying their
  values (SIMD checksums with `GOEXPERIMENT=simd`); `Framer` reads them off a
  stream, resynchronising after garbage; `Profile` holds the DATA fields of a
  dictionary.
- **Building** (`fix`): `Builder` encodes messages into reusable buffers without
  allocating; `RewriteForResend` turns a stored message into its
  retransmission.
- **Dictionaries** (`fix/dictionary`, `fix/dictionary/xmldict`): validation against
  QuickFIX XML dictionaries, the FIX Orchestra repositories the FIX Trading
  Community publishes (FIX Latest, FIX 4.2, FIX 4.4 and the FIXT session
  layer, from
  [FIXTradingCommunity/orchestrations](https://github.com/FIXTradingCommunity/orchestrations)),
  or generated ones, with the SessionRejectReason of each error; `Compose`
  for FIXT 1.1 application messages.
- **Code generation** (`fix/cmd/fixgen`): typed readers, builders and the
  dictionary as Go code, from a QuickFIX dictionary or an Orchestra
  repository; `-dispatch` adds a typed handler interface, `-bytes` getters
  of raw values (exact decimal prices, for one).
- **Sessions** (`fix/session`): initiators and acceptors for FIX 4.0 to 4.4 and
  FIXT 1.1, configured in Go:
  - many sessions per process and per listener, each with its own
    dictionaries, store and handler;
  - dynamic sessions, created by the acceptor as their counterparties log
    on;
  - logon, heartbeats, gap recovery, resends behind a barrier, sequence
    resets, NextExpectedMsgSeqNum, LastMsgSeqNumProcessed;
  - receipts telling when a sent message was written, and when the
    counterparty's engine provably read it;
  - retention, evicting from the store the messages the counterparty
    acknowledged (LastMsgSeqNumProcessed, NextExpectedMsgSeqNum,
    ResendRequest) beyond the last few and a minimum age, and any written
    beyond a bound;
  - schedules (`fix/session/schedule`) with boundaries fired on time;
  - stores (`fix/session/store`: in memory, and SQLite in
    `contribs/store/sqlite3`), transports
    (`transport`: TCP, TLS, in-memory), logging through `log/slog`
    (zerolog and zap through their slog handlers) and a redacting message
    observer;
  - Reject and Logout texts that start with the specification's wording and
    give the details: tags, names, values and what was expected.

## Contribs

Command-line tools and stores built on fix2, each a module of its own under
`contribs/`, so that their dependencies stay out of fix2's. They build
against the fix2 of the repository (a `replace` directive), and their tests
run from their directories: `go test ./...` at the root leaves them out.

The tools each have a Makefile: `make` builds `dist/<name>`, `make all`
cross builds with checksums, `make test`, `make lint` and `make
generate-verify` (the generated code is up to date). Their
messages come from packages fixgen generates from the venues' dictionaries
(`go generate ./...` runs fixgen as a Go tool), they sign their Logon with
the session's `LogonSigner` hook, resubscribe after every reconnection, and
log every message field by field with `--trace`.

### binancemd

Streams Binance Spot market data over the
[Binance FIX API](https://developers.binance.com/en/docs/products/spot/fix-api)
(FIX 4.4, Ed25519-signed Logon): trades, best bid and offer, order book
depth, or an order book kept up to date locally; it also lists instruments
and limits. It needs an Ed25519 API key with the `FIX_API` or
`FIX_API_READ_ONLY` permission, and reconnects when Binance announces
maintenance:

    cd contribs/binancemd && make
    export BINANCE_API_KEY=... BINANCE_PRIVATE_KEY=/path/to/ed25519-private.pem
    dist/binancemd trades BTCUSDT ETHUSDT
    dist/binancemd ticker --json BTCUSDT
    dist/binancemd book --levels 1000 --show 10 BTCUSDT
    dist/binancemd trades --api-key-file api-key --key-file ed25519-private.pem BTCUSDT
    dist/binancemd instruments --testnet BTCUSDT

### coinbasemd

Streams Coinbase Exchange market data over the
[Coinbase Exchange FIX API](https://docs.cdp.coinbase.com/exchange/fix-api/market-data)
(FIXT 1.1 and FIX 5.0 SP2, HMAC-SHA256-signed Logon): trades, best bid and
offer, the 10 best levels of each side, or that book kept up to date; it
also lists instruments. It needs an Exchange API key: its key, secret and
passphrase (flags, `--…-file` flags, or `COINBASE_API_KEY`,
`COINBASE_API_SECRET`, `COINBASE_API_PASSPHRASE`); keys of the public
sandbox work with `--sandbox`:

    cd contribs/coinbasemd && make
    dist/coinbasemd ticker --sandbox BTC-USD ETH-USD
    dist/coinbasemd book --json --show 5 BTC-USD
    dist/coinbasemd instruments BTC-USD

On macOS it verifies Coinbase's certificate with Go's verifier and
`/etc/ssl/cert.pem` (`--ca-file` to choose): macOS's own rejects Coinbase's
FIX certificates, which carry no Certificate Transparency timestamps.

### store/sqlite3

A `fix/session/store` keeping sessions in a SQLite database file, through
[mattn/go-sqlite3](https://github.com/mattn/go-sqlite3) (cgo): a session
resumes its sequence numbers, and can resend its messages, after the
process restarts. One file holds any number of sessions, each under a key
of its own; its tables are prefixed with `fix2_` and may share a database
with others:

    st, err := sqlite3.OpenFile("fix2.db", id.String())
    s, err := initiator.Add(session.Config{ID: id, Store: st, ...}, dialer)

The database runs in WAL mode: every call commits on its own, without
waiting for the disk, and `Sync`, which the session calls before it writes,
flushes the write-ahead log once for all the messages stored since. `New`
takes a `*sql.DB` the application opened itself.

## Testing

`make test` runs the module's tests and `make test-contribs` the contribs';
`RACE=1` adds the race detector, `CODECOV=1` writes `coverage.txt`. The
end-to-end suite, `fix/tests/e2e`, logs an initiator on to an acceptor in FIX
4.4 and in FIX 5.0 SP2, over TCP and in memory, with limited dictionaries
both sessions validate against: the initiator sends NewOrderSingle
messages, the acceptor acknowledges and fills each with ExecutionReport
messages (`go test ./fix/tests/e2e/`). `fix/tests/quickfix-go` replays the
behavioural issues reported to QuickFIX/Go, one file per issue. `make
fuzz` runs every fuzz target, the contribs' too, for `FUZZTIME` each (30s by
default); a failing input lands in its package's `testdata/fuzz`. CI runs
the tests on Linux, amd64 and arm64, and on macOS, arm64, with and without
`GOEXPERIMENT=simd`, and fuzzes on Linux amd64 and arm64: 20s per target on
every push, 3 minutes every night.

## Conformance

The session engine passes the 505 session acceptance test scripts of
QuickFIX (FIX 4.0 to 5.0 SP2, NextExpectedMsgSeqNum, LastMsgSeqNumProcessed,
ResendRequest chunking). Run them with a quickfix-go checkout:

    FIX2_QUICKFIX_TESTS=/path/to/quickfix-go/_test go test -run TestConformance ./fix/session/

## Performance

On an Apple M4: parsing a 24-field message into a reused `Message` takes
280 ns and no allocation; validating it 85 ns; sending an order between an
initiator and an acceptor in memory, 730 ns and one allocation.

`fix/tests/benchmarks` compares fix2 with other engines, QuickFIX/Go,
QuickFIX/J and QuickFIX C++, on orders and their acknowledgements over TCP, each end in a
process of its own, either engine on either end, their toolchains pinned
by a Nix flake: `cd fix/tests/benchmarks && nix develop -c make bench`.

## Operating

[docs/operations.md](docs/operations.md) says what a deployment has to size,
configure and watch: memory, authentication, TLS, what the logs hold, audit
trails, the SQLite store's disk, and a disk that stops answering.

## License

MIT, see [LICENSE](LICENSE).
