# FIXP, SOFH and SBE review at 8e9a7f4

Read-only production-source review. This scope is new since audit six: 149 files,
approximately 35,400 inserted lines, including generated codecs, fixtures and tests.
The tests below ran in `/private/tmp/fix2-audit7-fixp`, populated with
`git archive 8e9a7f4`. No production source or permanent test was edited.

## Result

No new concrete P0/P1/P2/P3 defect was confirmed in this scope. This is a focused
review and verification result, not a claim of exhaustive protocol conformance.

Inspected the delivery and persistence claims in README and docs/fixp.md;
handshake admission, authentication and stable owner binding; session lifecycle,
retirement and finalization; reader/writer queue admission; real-time and replay
numbering; held-message and request bounds; partial and late replay batches;
NotApplied/Applied classification; send/store/sync ordering; Terminate's
receive-only phase; finish acknowledgement ordering; SOFH fragmented-read,
timeout, fail-closed and size-bound paths; generated SBE count/length bounds,
schema evolution, group allocation and optional-field handling.

Recoverable delivery is explicitly at least once at the handler boundary, with
application deduplication required for exactly-once effects. Idempotent delivery
records admission durably before the handler, so process failure in between may
lose an operation without NotApplied. These are documented guarantees rather
than audit findings. Store implementations and callbacks must honor their
published context and concurrency contracts; cancellation cannot stop an
uncooperative callback or kernel operation. The default in-memory store provides
no restart durability, and MaxStoredMessages=0 deliberately retains all outbound
recoverable data. No additional remote allocation bypass was established.

## Verification

`go test -race -count=1 ./fixp/... ./sofh ./internal/gen/sbe/...`

Passed all 12 tested packages; six packages have no test files. See
`focused-race.log`. FIXP session tests took 96.398s. Silverflash reports `ok`, but
its optional external interoperability campaign is not asserted as enabled by
this invocation. Existing codec fixtures, decoder tests and crash-model tests
are included; no physical disk or process-power-loss campaign was performed.

`FIXP_TEST_LOG=1 go test -race -count=20 -run '^TestKeepalive$' -v ./fixp/session`

Passed 20/20; see `keepalive-repeat.log` (11.439s). This investigated the root
auditor's one broad-suite failure under parallel normal/SIMD/contrib/vet load.
TestKeepalive uses the test helper's 50ms interval and the default 1.5 leniency:
only 75ms of silence is permitted. Host scheduling delays beyond that bound can
legitimately expire a connection. The observed broad-run `mem: write to a closed
connection` event alone does not establish an engine liveness defect. The
isolated full scope and twenty repetitions did not reproduce it; the original
broad-run failure must still be disclosed with its load and timing limits.

The first sandboxed test attempt could not access the Go build cache. The valid
race runs were repeated with approved escalation. No network-bound skip or data
race was reported in the isolated package output; ordinary non-verbose output
does not enumerate skipped individual tests.
