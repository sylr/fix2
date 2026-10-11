# fix2

[![Go](https://github.com/sylr/fix2/actions/workflows/go.yaml/badge.svg)](https://github.com/sylr/fix2/actions/workflows/go.yaml)
[![codecov](https://codecov.io/gh/sylr/fix2/graph/badge.svg)](https://codecov.io/gh/sylr/fix2)
[![pkg.go.dev](https://pkg.go.dev/badge/github.com/sylr/fix2)](https://pkg.go.dev/github.com/sylr/fix2)

A FIX protocol library for Go: a zero-copy parser, validating dictionaries,
typed code generation and a session engine.

- **Parsing**: `Parse` and `ParseInto` read messages without copying their
  values (SIMD checksums with `GOEXPERIMENT=simd`); `Framer` reads them off a
  stream, resynchronising after garbage; `Profile` holds the DATA fields of a
  dictionary.
- **Building**: `Builder` encodes messages into reusable buffers without
  allocating; `RewriteForResend` turns a stored message into its
  retransmission.
- **Dictionaries** (`dictionary`, `dictionary/xmldict`): validation against
  QuickFIX XML dictionaries or generated ones, with the SessionRejectReason
  of each error; `Compose` for FIXT 1.1 application messages.
- **Code generation** (`cmd/fixgen`): typed readers, builders and the
  dictionary as Go code; `-dispatch` adds a typed handler interface,
  `-bytes` getters of raw values (exact decimal prices, for one).
- **Sessions** (`session`): initiators and acceptors for FIX 4.0 to 4.4 and
  FIXT 1.1, configured in Go:
  - many sessions per process and per listener, each with its own
    dictionaries, store and handler;
  - dynamic sessions, created by the acceptor as their counterparties log
    on;
  - logon, heartbeats, gap recovery, resends behind a barrier, sequence
    resets, NextExpectedMsgSeqNum, LastMsgSeqNumProcessed;
  - receipts telling when a sent message was written, and when the
    counterparty's engine provably read it;
  - schedules (`session/schedule`) with boundaries fired on time;
  - stores (`session/store`, an in-memory one for now), transports
    (`session/transport`: TCP, TLS, in-memory), logging through `log/slog`
    (zerolog and zap through their slog handlers) and a redacting message
    observer;
  - Reject and Logout texts that start with the specification's wording and
    give the details: tags, names, values and what was expected.

## Contribs

Command-line tools built on fix2, each a module of its own under
`contribs/`. They build against the fix2 of the repository (a `replace`
directive), and their tests run from their directories: `go test ./...` at
the root leaves them out. Each has a Makefile: `make` builds
`dist/<name>`, `make all` cross builds with checksums, `make test`, `make
lint` and `make generate-verify` (the generated code is up to date). Their
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

## Testing

`make test` runs the module's tests and `make test-contribs` the contribs';
`RACE=1` adds the race detector, `CODECOV=1` writes `coverage.txt`. `make
fuzz` runs every fuzz target, the contribs' too, for `FUZZTIME` each (30s by
default); a failing input lands in its package's `testdata/fuzz`. CI runs
the tests on Linux and macOS, amd64 and arm64, with and without
`GOEXPERIMENT=simd`, and fuzzes on Linux amd64 and arm64: 20s per target on
every push, 3 minutes every night.

## Conformance

The session engine passes the 505 session acceptance test scripts of
QuickFIX (FIX 4.0 to 5.0 SP2, NextExpectedMsgSeqNum, LastMsgSeqNumProcessed,
ResendRequest chunking). Run them with a quickfix-go checkout:

    FIX2_QUICKFIX_TESTS=/path/to/quickfix-go/_test go test -run TestConformance ./session/

## Performance

On an Apple M4: parsing a 24-field message into a reused `Message` takes
280 ns and no allocation; validating it 85 ns; sending an order between an
initiator and an acceptor in memory, 730 ns and one allocation.
