# fix2

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
  dictionary as Go code; `-dispatch` adds a typed handler interface.
- **Sessions** (`session`): initiators and acceptors for FIX 4.0 to 4.4 and
  FIXT 1.1, configured in Go:
  - many sessions per process and per listener, each with its own
    dictionaries, store and handler;
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

## Conformance

The session engine passes the 505 session acceptance test scripts of
QuickFIX (FIX 4.0 to 5.0 SP2, NextExpectedMsgSeqNum, LastMsgSeqNumProcessed,
ResendRequest chunking). Run them with a quickfix-go checkout:

    FIX2_QUICKFIX_TESTS=/path/to/quickfix-go/_test go test -run TestConformance ./session/

## Performance

On an Apple M4: parsing a 24-field message into a reused `Message` takes
280 ns and no allocation; validating it 85 ns; sending an order between an
initiator and an acceptor in memory, 730 ns and one allocation.
