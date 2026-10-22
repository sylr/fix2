# FIXP / SOFH / SBE / FIX codecs review at c7e5579

Read-only production-source audit. No production source or permanent test was
edited. Diagnostic tests were copied into `/private/tmp/fix2-audit8-codecs`, a
`git archive HEAD` extraction. Parent owns broad normal/race/static/generation
validation; this lane did not duplicate the full suite.

## Confirmed: P2 — FIXP callbacks overlap on authenticated reconnect

**High confidence: actual handshake and application delivery, three race runs,
with a passing release-before-reconnect control.**

`fixp/session/session.go:595–605` clears the old connection and publishes
StateUnbound **before** OnUnbound returns. `server.go:605–618` admits another
authenticated Establish at that point; its connection's `Session.run` calls
OnEstablished and delivers application messages while the preceding connection's
OnUnbound is still executing. This violates `handler.go:28–33`'s per-session
callback serialization promise. Application handlers relying on it can race
their connection teardown against initialization/message processing, or let old
teardown remove newly established per-session state.

`rebind-callback_test.go.txt` uses the actual server and in-memory listener,
Negotiate/Establish with `user:alice`, closes the first transport, holds the first
OnUnbound at a channel gate, and makes a second authenticated Establish under the
same UUID. Its peer receives EstablishmentAck; the server calls OnEstablished and
OnMessage for an actual application frame before the first OnUnbound is released.
The gate exposes an ordering window; it does not mutate engine state or bypass
authentication. All three reproductions complete in less than the verbose
runner's 0.01s display precision. No indefinitely blocked callback or exhausted
timeout is needed for overlap. Releasing the callback before the second handshake
produces no overlap. Atomics observe the ordering, so no application data race is
deliberately introduced into the test.

The same early detach causes an administrative variant:
`Server.retireSession`, `server.go:225` reads `s.conn`; after the early clear it
skips `server.go:251–254`'s unbind wait and invokes `OnFinalized` at line 273 while
OnUnbound remains active. `retire-callback_test.go.txt` reproduces this three times;
retiring after callback release is a passing control. This is one lifecycle
serialization finding, not two independent defects.

Evidence: `rebind-callback-race.log` and `retire-callback-race.log`.

Correction should retain exclusive session lifecycle ownership through the old
OnUnbound callback, prevent new establishment/finalization callbacks from passing
it, and preserve callback-safe Send/Terminate/Finish semantics. A plain mutex held
across arbitrary callbacks needs care because callback methods legitimately call
session methods. Test fast authenticated reconnect, administrative retirement,
server shutdown, and normal callback reentrancy. No business-data corruption,
cross-owner access or deployed incident was demonstrated; the callback-order
failure itself is proved.

## Confirmed: P3 — generated SBE decoders retain historical input frames

**High confidence: 513 valid inputs, three race repetitions, post-GC heap
measurement and direct stale-reference verification.**

The generator reslices group storage without releasing removed pointer-bearing
entries: `internal/gen/sbe/gen.go:1097` (empty group), `:1113` (positive shrink),
and `:1302` (group absent from an older version). For example the tracked
`tests/rich/msg_order.go:499–500` and `:524–527` implement those first two paths.
The backing array remains reachable from the empty/smaller group's slice, and
removed entries' []byte fields retain their old input buffers. Nested group
slices remain reachable too.

`group-retention_test.go.txt` repeatedly decodes into one unmodified generated
rich.Order, with 513 separately owned frames under 80,000 bytes each, its Legs
count decreasing from 512 to zero. Each frame has at most 1024 total decoded
entries, well below MaxEntries=65535. The empty final public group keeps 512
hidden Note references into historical frames. After GC, the diagnostic retains
about 39.54 MB, versus 147–168 KB in a same-count control using the same number
and size of input frames. This is retention of old frames, beyond the small group
backing array whose reuse is intentional. No malformed frame or count-limit
bypass is involved.

`group-version-retention_test.go.txt` decodes a newer.Order with Fees, then a valid
older.Order with that version-2 group absent. Public Fees has len=0, but its old
FeeMemo and nested Taxes/TaxCode remain reachable through the backing array.
The zero-value workaround (`got = newer.Order{}` before the next decode) passes.
There is no generated Reset method.

Evidence: `group-retention-race.log` and `group-version-retention-race.log`.

Scope limits: accumulation needs a decoder reused across calls and distinct
retained input allocations. It has a ceiling determined by schema/group capacity
and maximum input allocation; it is not unbounded for a fixed finite schema and
bounded inputs. Reusing one SOFH input buffer does not independently pin one new
allocation per message. FIXP's own generated session messages have no groups.
Binance's generated MiscFees has []byte members
(`contribs/binancefix/internal/fixsbe/msg_execution_report.go:941`), but its current
production connection path allocates a new message decoder at `conn.go:985` and
its market-data path likewise uses a new decoder per event (`marketdata.go:531+`).
No current Binance connection memory-exhaustion exploit was established.

Correction belongs in the generator, followed by regeneration. Release obsolete
input aliases and nested references when shrinking, clearing to zero, or reading
an older version, while preserving the desired reuse of live group storage.
Test pointer-bearing and numeric-only groups, nesting, absent-version groups,
decode failure and warm-decoder allocation behavior. Users can reset/replace the
message value as an immediate workaround, paying the corresponding reallocations.

## Other inspected boundaries / limits

Read the seventh report and FIXP audit notes as baseline. Inspected current
FIXP connection queue, send/assign/store/sync ordering, partial recovery and
request bounds, retransmission numbering, retained-message ownership, keepalive
and shutdown handling, server authentication/owner binding, negotiation limits,
retirement and finalization. Reviewed the post-audit-seven FinishedReceiving
restart/owed changes; no additional finish defect confirmed.

Inspected SOFH reader bounded growth, timeout resumption and fail-closed malformed
length paths; SBE loader integer/count/schema checks and generated decode bounds;
FIX parser DATA/LENGTH/body-length boundaries and dictionary validation/compiled
lookup construction. No other concrete defect confirmed. This is a bounded
source review, not exhaustive standards conformance or a new fault campaign.

## Reproduction

Copy each `.go.txt` into its corresponding package in an isolated archive and run:

```
go test -race -count=3 -run '^TestAudit8RebindOverlapsUnbound$' -v ./fixp/session
go test -race -count=3 -run '^TestAudit8RetireOverlapsUnbound$' -v ./fixp/session
go test -race -count=3 -run '^TestAudit8GroupTailRetention$' -v ./internal/gen/sbe/tests/rich
go test -race -count=3 -run '^TestAudit8AbsentGroupRetention$' -v ./internal/gen/sbe/tests/newer
```

Each command intentionally fails the defect assertion on HEAD; embedded controls
pass. Go's build cache needed approved sandbox escalation for the version and
lifecycle commands. The first successful retention run used an already available
cache; the discarded source-worktree invocation found no diagnostic tests and is
not evidence. The retained logs are from the isolated archive with the diagnostic
present. No race detector warning occurred: callback overlap is observed with
atomics rather than creating an unsafe application state access.
