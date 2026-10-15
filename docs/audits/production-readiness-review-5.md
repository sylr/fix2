# fix2 production readiness review — fifth audit

## Decision

**Audit four's specific findings are resolved. One new P2 remains: final Logout can wait indefinitely behind a replay read even when every relevant timeout is configured. Fix that closing-path regression before general production sign-off. Three P3 findings concern responses after final Logout, TCP listener reuse after cancellation, and generated builders for an implicit DATA group.**

No P0/P1 defect was confirmed. Existing root/contrib race suites, normal/SIMD builds, vet and all 505 external QuickFIX scripts pass. The new reproductions exercise cases absent from those suites. The P3 items have narrower exposure and are not blanket production blockers.

This audit changes documentation and evidence only. Production source and existing tests are unchanged. New regression sources are preserved as `.go.txt` files and executed in isolated archives; intentionally failing assertions are outside the normal test suite.

## Scope and evidence boundary

- **Date:** 15 October 2026.
- **Revision:** `0da258079503502ece32633a0030bf21e4b2a529`.
- **Previous audited revision:** `87fad5a6c5ce0388f204b5210b4fb6a5abe90672`.
- **Environment:** macOS arm64, Go 1.27.2; project modules target Go 1.27.
- **Scope:** audit-four remediation and follow-up changes, session protocol/arrival metadata, Logout and replay lifecycle, receipt history, SQLite/store behavior, transports, contrib clients, parser/framer/builders, dictionaries, generation, operations and CI.
- **Method:** three focused independent source reviews, leader review and independent execution of all four new reproductions, broad race/SIMD/contrib checks, explicit conformance, short fuzzing and dependency scans. Closing/replay and listener findings received additional independent cross-review.

This is a risk review with emphasis on recovery and completion boundaries, not a claim to have reread every generated line or rerun every historical reproduction. The [fourth report](production-readiness-review-4.md) and its review addendum are the remediation baseline. Earlier reports remain historical records.

## Findings

### A5-01 — P2: final Logout has no deadline while replay waits for storage

**Confidence: high; independently reproduced with the real SQLite store under the race detector and cross-reviewed.**

**Sources:** [Logout enters final closure](../../session/protocol.go#L1181), [peer-initiated response](../../session/protocol.go#L1200), [closing state](../../session/conn.go#L340), [timer skips closing connections](../../session/conn.go#L564), [held outbound queue](../../session/session.go#L550), [replay read](../../session/replay.go#L138).

The latest Logout change correctly avoids timing out a response as though it awaited another Logout. It also removes the remaining disconnect deadline while a final response is held behind replay. The writer is waiting in `Range(c.ctx)`; the final Logout cannot flush until that replay completes; `c.ctx` is canceled only when the connection disconnects, which now awaits the flush. The timer suppresses both Logout timeout and heartbeat/liveness checks while `c.closing` is true.

**Reproduction:** one SQLite session, a shared pool of two connections, and an application reserving the spare connection. Start a normal resend so `Range` waits for that connection, then process a valid peer Logout. Test both a peer-initiated Logout and crossing Logouts. Configure `LogoutTimeout`, `WriteTimeout`, `StoreTimeout` and `MaxPendingAge` to 50ms. After 350ms, `Stop(context.Background())` in the first case or `Logout(context.Background())` in the crossing case still waits, state remains `logging out`, and `Err` is nil. Closing only the peer transport immediately completes the call while the SQL connection remains reserved.

The forced-close control proves that the store honors cancellation. No handler or kernel operation is stuck. `WriteTimeout` does not bound a replay's pool wait; the write watchdog does not watch reads; `MaxPendingAge` checks `c.frames` while this Logout is in `c.heldFrames`. Thus none of those configured limits ends this wait.

**Impact and boundary:** graceful Logout/Stop can remain incomplete as long as the resource wait persists. This is a new final-closing interaction, not a recurrence of audit four's direct-disconnect cancellation defect. Explicit caller deadlines, peer closure or returning the SQL resource end the wait. `Initiator.Run` cancellation has its own bounded shutdown context; this reproduction does not establish indefinitely blocked Run cancellation.

**Correction and acceptance:** maintain a finite final-close budget encompassing replay reads, held frames, Sync and transport writes. Expiry must disconnect/cancel replay, while a healthy slow write still gets a chance to flush the Logout response. Cover peer-initiated and crossing Logouts with the SQL resource still reserved; verify bounded completion, cancellation and receipt/retention accounting. Do not restore the old zero-deadline disconnect that could discard the response before writing it.

**Evidence:** [SQLite diagnostic](audit-evidence/production-readiness-5/runtime/sqlite_logout_replay_test.go.txt), [race output](audit-evidence/production-readiness-5/runtime/sqlite-logout-replay.log), [leader's independent execution](audit-evidence/production-readiness-5/runtime/logout-pool-independent.log). The diagnostic passes by asserting the defective wait and forced-close control.

### A5-02 — P3: reader responses can follow the final Logout

**Confidence: high; three race executions plus an independent rerun and passing control.**

**Sources:** [TestRequest response](../../session/protocol.go#L952), [queue admission](../../session/session.go#L532), [writer closes after pending output](../../session/conn.go#L371).

Final closure suppresses timer traffic and acknowledgement probes, but incoming messages can still generate new output. `onRegularLocked` answers a TestRequest unconditionally; `queueLocked` admits the response while the connection is closing. The writer then sends it after the supposedly final Logout.

**Reproduction:** gate a cooperative store's Sync while the sequence-2 response to the peer's Logout waits to write. Send sequence-3 TestRequest and wait for it to be counted, then release Sync. Wire output is Logout sequence 2, Heartbeat sequence 3, then EOF; next sender is 4. Without the extra input, the control sends only Logout and closes with next sender 3.

**Impact and boundary:** an extra sequence number is consumed after session termination was acknowledged, and a peer closing on Logout may not read it. No persistent counter disagreement, application-order loss or duplicate execution was demonstrated. The trigger requires additional admin input after the peer's Logout; it concerns the final closing phase, not the earlier intentional grace period while awaiting a Logout response. The current commit's `Not-tested` trailer explicitly records this gap.

**Correction and acceptance:** enforce the final-output boundary for reader-triggered work as well as timers/probes. A deterministic Sync/write gate should show Logout as the last frame and no extra sender sequence consumed, while existing intentional resend and earlier Logout-grace behavior remains covered. This fix alone does not settle an already blocked replay in A5-01.

**Evidence:** [regression with control](audit-evidence/production-readiness-5/session/closing-input_test.go.txt), [three race runs](audit-evidence/production-readiness-5/session/closing-input.log), [independent run](audit-evidence/production-readiness-5/session/closing-input-independent.log).

### A5-03 — P3: a canceled Accept can leave the TCP listener unusable

**Confidence: high; three race executions and an independent reproduction.**

**Source:** [cancellation callback and deadline cleanup](../../session/transport/tcp/tcp.go#L123).

`Listener.Accept` uses `context.AfterFunc` to set an expired listener deadline on cancellation. When its stop function returns false, Accept clears the deadline without waiting for that callback to finish. False can mean the callback is scheduled or still running. It can therefore set the expired deadline after cleanup, affecting later Accept calls.

**Reproduction:** queue a TCP connection, then call Accept with an already-canceled context. With `GOMAXPROCS=1`, the queued connection lets Accept return before its cancellation callback runs. Let that callback finish, queue a second connection and call Accept with a fresh one-second context. It immediately returns `i/o timeout` instead of accepting. All three race executions fail on attempt 0; the independent run does too. The one-processor setting makes a permitted callback ordering reproducible; this is a lifecycle race, not a race-detector memory-access report.

**Impact and boundary:** callers reusing a TCP listener across canceled Accept contexts can retain a stale deadline, causing subsequent accepts to fail. Default `Acceptor.Run` closes its listeners on cancellation, so this does not demonstrate an outage of an ordinary active acceptor. Existing simple deadline/reuse coverage passes but does not exercise cancellation with an already queued connection.

**Correction and acceptance:** join a started cancellation callback before restoring the listener deadline and returning/releasing the Accept mutex. Cover canceled contexts with pending connections, concurrent cancellation, and reuse with a fresh context; preserve normal deadline cancellation and listener Close behavior.

**Evidence:** [regression](audit-evidence/production-readiness-5/transport/accept-cancel-reuse_test.go.txt), [three race runs](audit-evidence/production-readiness-5/transport/accept-cancel-reuse.log), [independent run](audit-evidence/production-readiness-5/session/tcp-accept-crossreview.log).

### A5-04 — P3: an implicit group DATA partner has no generated setter

**Confidence: high; a strict-valid wire control, generated-reader control, explicit-DATA control and independent reproduction establish the gap.**

**Sources:** [declared-field-only setter loop](../../cmd/fixgen/generate.go#L705), [private entry fields](../../cmd/fixgen/generate.go#L690), [contextual implicit partner handling](../../cmd/fixgen/generate.go#L1157).

Dictionary and layout generation support a DATA/LENGTH pair when a group explicitly lists its LENGTH field and the DATA partner exists globally. The entry-builder setter loop nevertheless considers only declared group fields. It skips LENGTH because a DATA setter computes it, but never generates that setter when every merged definition of the group omits the DATA reference. The entry's raw fields are private, so there is no public raw-field fallback on that generated entry builder.

**Reproduction:** generate a dictionary with NoG(500), a group listing RawDataLength(95), and global definitions for 95 and RawData(96). A generic wire message carrying one entry, `95=3` followed by binary `96=a<SOH>b`, passes ParseInto and the generated dictionary's `ValidateWith(CheckAll)`. The generated reader returns one entry. However, `NewNoGEntry()` has no `SetRawData` method and cannot populate that otherwise accepted entry. Explicitly listing RawData in the XML produces the setter and round-trips successfully.

**Impact and boundary:** the typed builder cannot express a group its dictionary and reader accept. This requires the described custom schema; no bundled-dictionary failure or remote runtime issue was demonstrated. Adding the explicit DATA reference is a workaround, and the generic builder can construct the valid wire control.

**Correction and acceptance:** derive entry setters from the effective paired layout, or otherwise include implicit DATA partners while retaining safe LENGTH handling. Preserve contextual/shared-group layouts and verify generated setter, strict validation and reader round-trip for both implicit and explicit cases.

**Evidence:** [generator regression and controls](audit-evidence/production-readiness-5/parser/length-only-group_test.go.txt), [execution](audit-evidence/production-readiness-5/parser/length-only-group.log), [leader's independent execution](audit-evidence/production-readiness-5/parser/generator-independent.log), [review notes](audit-evidence/production-readiness-5/parser/README.md).

## Review addendum

A second pass reran all four reproductions in an isolated archive at the audited revision, and they behaved as reported. The A5-01 diagnostic shows the wait in both cases, and the forced close ends it. The A5-02, A5-03 and A5-04 assertions fail and their controls pass. Each finding was then confirmed in the source, and this pass agrees with every severity. It adds the following items.

### A5-01: what changed in `0da2580`, and how far it reaches

- **The crossing case is a regression.** Before `0da2580`, the deadline our own `Logout` armed still applied once the counterparty's Logout arrived. [`!c.closing`](../../session/conn.go#L567) now skips it. The peer-initiated case never had a working bound: the zero deadline disconnected before the answer was written, which was the bug `0da2580` fixed.
- **The commit had flagged half of this.** Its `Not-tested` trailer names a store Sync that ignores its context. A replay read behind which the final Logout is held is the case it missed.
- **Shutdown through `Run` is bounded.** [`shutdown`](../../session/acceptor.go#L327) stops each session with a `LogoutTimeout + 1s` context. The exposure is a direct `Stop` or `Logout` call with an unbounded context.
- **A counterparty that follows its own LogoutTimeout also ends the wait**, by disconnecting when our answer does not come. The wait is only indefinite when the counterparty keeps the connection open and the store resource stays held.

The finite budget should be its own deadline, armed in `closeAfterFlushLocked`. Reusing `logoutDeadline` would bring back the "no Logout reply" message for a connection that awaits no reply. On expiry, `disconnectLocked` cancels `c.ctx`, which already ends `Range`, as the forced-close control shows.

### A5-02 widened: no reader path checks the final boundary

`c.closing` appears nowhere in `protocol.go` or `replay.go`, so the TestRequest is only one trigger. Every message the reader produces can follow the final Logout:

- a Reject;
- our ResendRequest, sent when the reader detects a gap;
- a replay the counterparty asks for. [`writeLoop`](../../session/conn.go#L365) starts a queued replay before it [checks `closing`](../../session/conn.go#L371). Once the Logout is written and `pending` is empty, a ResendRequest that arrived meanwhile is replayed after it. This was found by reading the source; it was not reproduced.

`0da2580` considered refusing every admin message but Logout in `queueLocked` once closing, and rejected it because it "reaches the older drain loop, beyond this fix". A4-01 (`289cbc1`) has since stopped the drain loops when the phase changes, so that obstacle should be gone; confirm it when fixing. Two cautions:

- `sendAdminLocked` disconnects on any `queueLocked` error. The refusal needs an error it treats as "dropped", not as a write failure.
- `startReplayLocked` should not start or queue a replay while closing. The counterparty will ask again on the next connection.

The gate must apply only to `c.closing`. While our Logout awaits its answer, the session must keep answering resends.

### A5-03: confirmed

`context.AfterFunc`'s stop function returns false both when the callback is running and when it is only scheduled, and Accept does not wait for either. A channel the callback closes, waited on when `stop()` returns false, before the deadline is cleared, is enough. `Acceptor.acceptLoop` passes `Run`'s context and returns on cancellation, so the default acceptor does not reuse a listener after a canceled Accept, as the audit says.

### A5-04: the read side already handles this

[`specLiteral`](../../cmd/fixgen/generate.go#L1153) adds the implicit DATA partner when a group declares only its LENGTH ("even when the group declares only one of them"). The entry-builder setter loop at [generate.go#L706](../../cmd/fixgen/generate.go#L706) should follow the same rule: when it skips a LENGTH field whose DATA partner is not declared in the group, it should emit that partner's setter.

## Audit-four remediation

| Previous item | Fifth-audit disposition |
|---|---|
| A4-01: buffered messages age against drain time; drain continues after fatal transition | Resolved for the reported paths. Stashed frames keep arrival time; normal, GapFill and Reset-mode recovery, held admin/Logout and drain boundaries pass focused race regressions. [Evidence](audit-evidence/production-readiness-5/session/a4-01-verification.log). |
| A4-02: disconnected replay read ignores cancellation; initial Open is unbounded | Resolved for the original disconnect/reconnect/Stop and pool-wait cases. Range receives connection cancellation; Open receives StoreTimeout. A5-01 is a later final-Logout timeout interaction. [SQLite remediation tests](audit-evidence/production-readiness-5/runtime/remediation-sqlite.log). |
| A4-03: reset loses settled receipt evidence | Resolved in focused Written, Skipped, rejection and bounded-generation tests. History preserves levels and settled outcomes within documented limits, with increasing-generation enforcement. [Race evidence](audit-evidence/production-readiness-5/runtime/remediation-session.log). |
| A4-04: profile fallback allocates a dense table for every pair | Resolved. The original 4,096-pair diagnostic now allocates approximately 15.1MB with the unsupported pair versus 14.8MB without it, rather than 14.84GB cumulatively. Range filtering precedes one profile construction. [Original diagnostic rerun](audit-evidence/production-readiness-5/parser/a4-04-remediation.log). |

Other earlier remediation remains covered by the current broad suites and focused reviews; this is not a fresh replay of every first-audit reproduction. No new persistence/receipt-history defect was confirmed in the focused runtime review. Existing documented limits on authentication, memory/store retention, observer completeness, callback cooperation and power-loss durability remain applicable.

## Verification

The [machine-readable summary](audit-evidence/production-readiness-5/verification-summary.json) records package outcomes, skips, commands, scan metadata and static-analysis results. Full test JSON, scanner JSON and coverage are retained in `/private/tmp/fix2-production-readiness-5-0da2580`; compact logs and reproductions accompany this report.

| Check | Result |
|---|---|
| Root normal race suite, uncached test execution, loopback permitted | Passed: 2,958 passing test/subtest events, three skips. TCP/TLS and end-to-end network paths ran. |
| Root SIMD race suite, loopback permitted | Passed: same 2,958 passing events and three skips. |
| Race-disabled allocation checks | Both allocation tests skipped under race passed separately. |
| Contrib race suites | Binance, Coinbase and SQLite passed. |
| `make vet` | Root and all three contrib modules passed. |
| Explicit external QuickFIX conformance with race | All 505 scripts passed; 506 events including the parent. Fixture revision `236c791cfcbca8d57a36f92927f9e8265349359b`. |
| 15-second Groups / dictionary Validate / Initiator fuzz campaigns | Passed: approximately 612,687 / 1,243,517 / 5,241 executions. |
| Independent 15-second Framer / XML Load fuzz campaigns | Passed: approximately 176,198 / 115,165 executions. |
| `govulncheck`, root and three contrib modules | Completed with no findings for scanned graphs. Scanner v1.7.0; official database snapshot last modified 8 October 2026 at 22:31:09 UTC. |
| Staticcheck through golangci-lint v2.14.0, built with Go 1.27.1 | Completed. Root exits 1 with 16 diagnostics: nine quick-fix suggestions, two style diagnostics, five SA4000 diagnostics in fixture singleton tests. Three contrib modules report zero issues. These diagnostics are pre-existing, not the new production findings above. |
| Standalone Staticcheck v0.8.1 | Still cannot decode Go 1.27 export data; not treated as a completed analysis. The compatible golangci-lint run supplies the static-analysis evidence instead. |
| New reproductions | A5-01 diagnostic passes while asserting the wait/control. A5-02, A5-03 and A5-04 assertions fail as expected; their applicable controls pass. |

The three skips in each broad root run are the two separately exercised allocation checks and the separately enabled conformance harness. No network skips occurred in those permitted runs. Selected statement coverage: parser/root 98.6%, dictionary 97.9%, XML loader 99.1%, generator 95.0%, session 93.7%, TCP transport 92.1%, memstore 91.9%. Coverage supports the audit but does not prove recovery or failure-mode completeness.

Staticcheck is not a clean check. Its five SA4000 diagnostics concern `Dictionary() != Dictionary()` in generated-fixture singleton tests; the others concern style and simplifications. The current CI gate uses vet, tests and fuzzing. The earlier Staticcheck compatibility gap is now closed by an actual compatible analysis, with diagnostics disclosed. [Root output](audit-evidence/production-readiness-5/root-golangci-staticcheck.log).

No Linux/amd64/arm64 CI run, long-duration load/soak, live venue session, benchmark-toolchain execution, machine crash, physical power loss or disk-full/fault campaign was performed. No general capacity or platform-wide durability guarantee follows from these local checks. The [operations guide](../operations.md) remains required deployment context.

## Reproduction and handoff

Use an isolated archive at the audited revision. These sources reuse test helpers already present there; the SQLite reproduction belongs in its separate module:

```sh
audit_tmp=$(mktemp -d)
git archive 0da258079503502ece32633a0030bf21e4b2a529 | tar -x -C "$audit_tmp"
cp docs/audits/audit-evidence/production-readiness-5/runtime/sqlite_logout_replay_test.go.txt "$audit_tmp/contribs/store/sqlite3/audit5_logout_test.go"
cp docs/audits/audit-evidence/production-readiness-5/session/closing-input_test.go.txt "$audit_tmp/session/audit5_closing_test.go"
cp docs/audits/audit-evidence/production-readiness-5/transport/accept-cancel-reuse_test.go.txt "$audit_tmp/session/transport/tcp/audit5_accept_test.go"
cp docs/audits/audit-evidence/production-readiness-5/parser/length-only-group_test.go.txt "$audit_tmp/cmd/fixgen/audit5_group_test.go"
(cd "$audit_tmp/contribs/store/sqlite3" && go test -race -count=1 -run '^TestAudit5' -v .)
(cd "$audit_tmp" && go test -race -count=1 -run '^TestAudit5' -v ./session ./session/transport/tcp)
(cd "$audit_tmp" && go test -count=1 -run '^TestAudit5' -v ./cmd/fixgen)
```

Loopback permission is required for the TCP reproduction. A writable GOCACHE may be needed in constrained environments. The second and third commands intentionally fail before correction; the first is a behavior diagnostic.

Prioritize the finite final-close budget, then enforce final-output admission. Address listener callback cleanup and implicit group setters for deployments using those APIs. Convert the relevant reproductions into permanent regressions and rerun broad race/SIMD/contrib checks, vet, static analysis and explicit conformance. Production sign-off remains conditional on fixing A5-01 and obtaining deployment-specific load/fault/platform evidence appropriate to the intended workload.
