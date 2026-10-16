# fix2 production readiness review — fourth audit

## Decision

**Audit three's T01 and T02 are resolved. Three newly confirmed P2 defects remain in gap recovery, replay cancellation and receipt history. Resolve these before general production sign-off. A fourth, P3 allocation issue concerns custom dictionary loading; deployments accepting runtime or tenant dictionaries should treat it as a conditional P2.**

No P0/P1 defect was confirmed. The existing regression suites and all 505 external QuickFIX conformance scripts pass. The new reproductions demonstrate behavior those suites do not cover; passing conformance alone does not settle recovery, shutdown or receipt guarantees.

This audit changes documentation and evidence only. Production source and existing tests remain unchanged. Deliberately failing regression assertions are preserved as `.go.txt` files outside the test suite and were executed in isolated copies of the audited revision.

## Scope and evidence boundary

- **Date:** 15 October 2026.
- **Revision:** `87fad5a6c5ce0388f204b5210b4fb6a5abe90672`.
- **Previous audit revision:** `f24332d945009938ebfc92f697cab00bf6f5ab82`.
- **Environment:** macOS arm64, Go 1.27.2; all project modules target Go 1.27.
- **Scope:** remediation since audit three plus focused reviews of session protocol/recovery, receipts, persistence, store watchdogs, lifecycle, transports, contrib clients, parser/framer/builders, dictionaries, generation, operations and CI.
- **Method:** three independent focused source reviews, leader review and independent reproduction of each primary finding, broad normal/SIMD race suites, explicit conformance, vet, short fuzz campaigns and vulnerability scans. Receipt and buffered-arrival findings received additional cross-review.

This is a fresh risk review with emphasis on failure paths, not a claim to have reread every generated line or replayed every first-audit reproduction. The reports from [audit one](production-readiness-review.md), [audit two](production-readiness-review-2.md) and [audit three](production-readiness-review-3.md) remain historical records.

## Findings

### A4-01 — P2: gap recovery rejects messages that were valid when received

**Fixed in:** [`289cbc1`](https://github.com/sylr/fix2/commit/289cbc19cb1694839c079fe22605b4537ee3a56d), [`cc8d2ba`](https://github.com/sylr/fix2/commit/cc8d2ba2bdc840fc88ac1243ae268c256098e9ff) (drain stopped before the last advance too).

**Confidence: high; reproduced repeatedly with the race detector and independently cross-reviewed.**

**Sources:** [stash replay timestamp](../../session/protocol.go#L874), [regular header checks](../../session/protocol.go#L897), [counting a fatal header rejection](../../session/protocol.go#L485), [Inbound.Received contract](../../session/handler.go#L19).

An ahead-of-sequence message passes CompID and SendingTime checks before being stashed. The stash retains its bytes but not its original arrival time. When the gap closes, `replayStashedLocked` sets `Inbound.Received` to `time.Now()` and runs the header checks again against that drain time. A message valid on arrival can therefore fail SendingTime accuracy solely because recovery took time.

**Reproduction:** establish a session with `MaxLatency=500ms`, receive valid order sequence 3 while expecting 2, and wait for the engine's ResendRequest, proving the original header check passed. Wait 700ms, then provide fresh sequence 2. The buffered order receives Reject reason 10 and triggers Logout; it never reaches `FromApp`. Its sequence is counted, so ordinary reconnect recovery does not request it again. A subsequent sequence-4 barrier makes the observed final `NextTarget` 5; `apps=[]`.

The reduced latency setting makes the test short. With the default 120-second latency bound, the same mechanism applies to sufficiently long recovery, or sooner for a message already near the permitted age on arrival. No matching recovery-duration limit prevents it. Even shorter recovery incorrectly changes the application's `Inbound.Received` from arrival time to drain time.

**Impact:** valid traffic can be rejected and counted without application delivery during recovery, accompanied by an unnecessary session Logout. The evidence demonstrates a protocol rejection, not silent wire disappearance or duplicate execution.

**Correction and acceptance:** retain original arrival metadata with stashed frames and use it for arrival-relative validation and `Inbound.Received`. Preserve checks that must still run during recovery. Cover ordinary missing-message completion, GapFill and Reset-mode recovery, with application delivery and received-time assertions.

A related boundary needs coverage in the same area: `advanceLocked` and `resetPastLocked` continue draining after an earlier held message's header failure starts Logout, contrary to `checkHeaderLocked`'s comment that messages held ahead remain unprocessed. Both paths are reproduced in the companion test. This is specifically a buffered-recovery invariant; the current design generally allows normal input processing during Logout grace, so this report does not claim every callback while logging out is forbidden. Repairing arrival metadata removes the demonstrated stale-time trigger; preserve the fatal-header boundary independently as well.

**Evidence:** [arrival-time regression](audit-evidence/production-readiness-4/session/buffered-arrival-time_test.go.txt), [related drain regression](audit-evidence/production-readiness-4/session/stale-held-header_test.go.txt), [three repeated race runs](audit-evidence/production-readiness-4/session/stale-held-header-repro.log), [leader reproduction](audit-evidence/production-readiness-4/session/independent-reproductions.log), [independent cross-review run](audit-evidence/production-readiness-4/runtime/buffered-arrival-crossreview.log).

### A4-02 — P2: disconnected replay reads cannot be canceled

**Fixed in:** [`7dcccf2`](https://github.com/sylr/fix2/commit/7dcccf2762dc7179b4bacefebcca90efc50f1a07), [`cc8d2ba`](https://github.com/sylr/fix2/commit/cc8d2ba2bdc840fc88ac1243ae268c256098e9ff) (no chunk flushed once the connection ended).

**Confidence: high; independently reproduced with the real SQLite store and race detector.**

**Sources:** [replay Range context](../../session/replay.go#L131), [SQLite pool acquisition](../../contribs/store/sqlite3/sqlite3.go#L757), [connection waits for writer](../../session/conn.go#L262), [initiator waits for connections](../../session/initiator.go#L240).

Replay calls `Store.Range(context.Background(), ...)`. Disconnect cancels the connection's context, but cannot cancel a store read waiting on a resource. The connection waits for its replay writer to finish before retiring, so the initiator cannot finish that connection or reconnect. Cancellation of `Initiator.Run` also waits for it. The store-write watchdog does not cover this read.

**Reproduction:** use one SQLite session and a shared `sql.DB` with `MaxOpenConns=2`, satisfying the documented n+1 minimum. An application reserves the spare connection. A direct `Range` with a 30ms context correctly returns deadline exceeded, demonstrating that SQLite supports cancellation here. A normal peer ResendRequest starts session replay and waits for the same connection. Disconnect the peer: replay remains blocked and no reconnect occurs over more than ten configured reconnect intervals. `Stop` returns at its 30ms deadline with a durability-related error, but `Done` stays open; canceling `Initiator.Run` leaves it waiting too. After 1.4 seconds, over 35 configured StoreTimeout periods, `Err` is still nil. Returning the application's spare SQL connection immediately allows shutdown to finish.

**Impact:** shared-pool contention or another delayed, cancellable store read can stop reconnection and keep shutdown incomplete for as long as the read waits. This does not require a stuck handler, a defective store ignoring context, or an uncancelable kernel write. The reproduction does not show `Stop(ctx)` itself overrunning its deadline. Larger pools reduce contention but do not repair the missing cancellation path.

**Correction and acceptance:** pass connection cancellation into replay reads, check it while processing replay callbacks, and retire canceled replay work without spurious loss notifications or attempts to write to an ended connection. Test disconnection followed by reconnection while the unrelated SQL resource remains reserved, and `Run`/`Done` completion after cancellation. Keep genuine store-error and partial-write accounting intact.

**Evidence:** [public SQLite diagnostic](audit-evidence/production-readiness-4/runtime/sqlite_replay_cancel_test.go.txt), [race execution with reconnect observation](audit-evidence/production-readiness-4/runtime/sqlite-replay-cancel.log), [leader's independent execution](audit-evidence/production-readiness-4/runtime/sqlite-replay-independent.log). The diagnostic passes by asserting the defective behavior and its release control.

### A4-03 — P2: a completed Written receipt becomes unknown after reset

**Fixed in:** [`2f27fed`](https://github.com/sylr/fix2/commit/2f27fedcc4790792529b474f07e435e0e8ff9805), [`cc8d2ba`](https://github.com/sylr/fix2/commit/cc8d2ba2bdc840fc88ac1243ae268c256098e9ff) (ended rejections in maps of their size).

**Confidence: high; reproduced with the race detector and independently cross-reviewed.**

**Sources:** [public monotonic Wait guarantee](../../session/receipt.go#L60), [old-generation lookup](../../session/receipt.go#L129), [generation-history preservation](../../session/receipt.go#L353).

`Receipt.Wait` promises that a reached level keeps returning success. Within a generation, a complete write satisfies `Written` independently of acknowledgement. On sequence reset, however, `newGeneration` preserves only acknowledged records and clears the others. `receiptState` then returns `ErrUnknownDelivery` for a previously written, unacknowledged message, even when the caller already observed successful `Wait(Written)`.

**Reproduction:** send an order, read it at the peer and await `Written` successfully. Receive a valid active reset Logon and its response. Waiting for `Written` on the same receipt now returns `ErrUnknownDelivery`. A control that acknowledges the order before reset keeps returning success.

**Impact:** applications can lose previously established transport-write evidence across active resets or other generation transitions. This can mislead retry/reconciliation decisions. The reproduction does not prove a duplicate order or application-level execution failure. An unacknowledged receipt becoming unknown at the **Acknowledged** level is intentional; the defect is losing the already reached **Written** level.

**Correction and acceptance:** preserve historical evidence by level within documented tracking bounds. After reset: written-only receipts retain successful `Written` but report unknown acknowledgement; acknowledged receipts retain both; never-written receipts remain unknown. Cover repeated resets, existing tracking limits and eviction, without adding unbounded history.

**Evidence:** [regression and acknowledged control](audit-evidence/production-readiness-4/session/written-receipt-reset_test.go.txt), [race output](audit-evidence/production-readiness-4/session/written-receipt-reset.log), [independent rerun](audit-evidence/production-readiness-4/session/independent-reproductions.log). The written-only assertion fails; the acknowledged control passes.

### A4-04 — P3, conditional P2: profile fallback amplifies custom-dictionary allocation

**Fixed in:** [`8d7381c`](https://github.com/sylr/fix2/commit/8d7381c91cca65b179c0fb2e344b7b60a0d6cb62).

**Confidence: high for the allocation mechanism; exposure depends on dictionary provenance.**

**Sources:** [fallback filtering](../../dictionary/profile.go#L110), [dense table allocation](../../profile.go#L45).

When any inferred DATA/LENGTH pair exceeds the profile's tag range, `dataProfile` filters pairs by calling `NewProfile` separately for every pair. Each valid high-tag pair allocates a dense table up to that tag merely to test validity. One unsupported pair changes work from a single table allocation to approximately pair count × highest supported tag.

**Reproduction:** load XML with 4,096 DATA/LENGTH pairs around tag 900,000. Without an unsupported pair, the 444,303-byte dictionary allocates about 14.8MB. Adding a 109-byte pair at tags 1,048,576/1,048,577 produces about **14.84GB of cumulative allocation**. Both loads succeed. The original diagnostic measured 17ms versus 1.55s; the leader's independent rerun reproduced the allocation and took approximately 0.83s for the outlier case. Timings are machine-dependent diagnostics, not benchmark thresholds. Cumulative allocation is not peak resident memory.

**Impact:** excessive allocation and GC/startup work when importing custom dictionaries. FIX messages alone cannot trigger compilation. None of the nine bundled XML dictionaries or eight generated packages examined contains tags outside the supported range; their highest observed tag is 29,004. Treat this as P2 for a service exposing tenant/runtime dictionary imports, otherwise P3 robustness work.

**Correction and acceptance:** filter unsupported pairs without allocating dense profiles per pair, then build the profile once, or explicitly reject unsupported dictionaries. Measure allocation for high-tag dictionaries with one unsupported pair and preserve supported DATA parsing semantics.

**Evidence:** [public loader diagnostic](audit-evidence/production-readiness-4/parser/profile-fallback-cost_test.go.txt), [allocation/control results](audit-evidence/production-readiness-4/parser/profile-fallback-cost.log), [independent rerun](audit-evidence/production-readiness-4/parser/profile-cost-independent.log), [review notes](audit-evidence/production-readiness-4/parser/README.md).

## Review addendum

A second pass reran every reproduction at the audited revision, confirmed each finding in the source and agreed with its severity. It adds the following items.

### A4-01: the drain-after-Logout boundary is its own fix

**Fixed in:** [`289cbc1`](https://github.com/sylr/fix2/commit/289cbc19cb1694839c079fe22605b4537ee3a56d), [`cc8d2ba`](https://github.com/sylr/fix2/commit/cc8d2ba2bdc840fc88ac1243ae268c256098e9ff) (drain stopped before the last advance too).

Keeping the arrival time removes the trigger the reproductions use, but not the boundary itself. Any header failure that starts a Logout while held messages drain still lets `advanceLocked` and `resetPastLocked` deliver the messages held after it (`MUST-STAY-BUFFERED` is delivered while logging out). Both loops must stop once the session leaves the established phase. The same drain also hands the drain time to `onSequenceResetLocked`, `onLogoutLocked` and `onResendRequestLocked`. Keeping the arrival time with the held frame covers them, and the regressions should include a held SequenceReset.

### A4-03 widened: a reset loses every settled answer, not only Written

**Fixed in:** [`2f27fed`](https://github.com/sylr/fix2/commit/2f27fedcc4790792529b474f07e435e0e8ff9805), [`cc8d2ba`](https://github.com/sylr/fix2/commit/cc8d2ba2bdc840fc88ac1243ae268c256098e9ff) (ended rejections in maps of their size).

`newGeneration` keeps only acknowledged records, so a reset also loses:

- the rejection a [`Receipt.Rejection`](../../session/receipt.go#L108) call returned, which checks the generation and then answers false;
- `ErrSkipped`: a receipt `Wait` reported as gap-filled, so never delivered, reports `ErrUnknownDelivery` after the reset. That answer is more cautious, but it is not stable.

A daily sequence reset with any message written but not yet acknowledged is enough to trigger the Written case. The correction should keep each level's evidence (Written, Skipped, the rejection) across generations, within the existing tracking bounds.

### A4-02: store Open at session creation

**Fixed in:** [`7dcccf2`](https://github.com/sylr/fix2/commit/7dcccf2762dc7179b4bacefebcca90efc50f1a07).

[`store.Open(context.Background())`](../../session/session.go#L225) runs when a session is created, outside the write watchdog. With an exhausted SQLite pool, `Initiator.Add` hangs as replay does, and it takes no context. Severity LOW: it happens at configuration time, not on a live session.

## Previous remediation

| Previous item | Fourth-audit disposition |
|---|---|
| T01: malformed tags bypass normal session checks | Resolved for the reported malformed-input paths. Current regressions cover identity/time, BeginString, low/high sequences, partial headers, PossDup, reset mode, unusable GapFill recovery and diagnostic redaction. [Race verification](audit-evidence/production-readiness-4/session/t01-remediation.log). |
| T02: strict group validation scales with unused schema width | Resolved for all three reported costs: indexed positions, clearing touched words and activation from fields present. For 20,000 entries, widths 64/4,096/65,536 measured approximately 1.78/1.53/1.68ms. [Current regressions and diagnostic](audit-evidence/production-readiness-4/parser/t02-regressions.log). |
| Earlier persistence, write receipts, SQLite metadata, resource limits and shutdown fixes | Current broad root/contrib race suites pass; focused reviews found no new regression in those specific fixes. A4-02 concerns cancellation of replay **reads**, which the existing write watchdog does not cover. |

Partial MsgType reflection into outbound RefMsgType remains an intentional, tested compatibility choice. Session-owned diagnostic attributes withhold partial values. It is not counted as a new redaction finding. Existing documented deployment boundaries—authentication, retained-store growth, lossy observers, callback cooperation and macOS power-loss durability—remain applicable.

## Verification

The machine-readable [verification summary](audit-evidence/production-readiness-4/verification-summary.json) records package outcomes, skipped tests and vulnerability-scan metadata. Full JSON test streams and coverage remain in `/private/tmp/fix2-production-readiness-4-87fad5a`; compact outcomes and focused evidence are preserved with this report.

| Check | Result |
|---|---|
| Root normal race suite, uncached test execution | Passed: 2,909 passing test/subtest events, nine skips. |
| Root SIMD race suite | Passed: same 2,909 passing events and nine skips. |
| TCP/TLS and end-to-end paths, normal and SIMD, rerun with loopback permitted | Passed: nine events per run, zero skips. These exercise the six network cases skipped in each sandboxed root run. |
| Root race-disabled allocation checks | Both previously race-skipped allocation tests passed. |
| Contrib race suites | Binance, Coinbase and SQLite passed. |
| `make vet` | Root and all three contrib modules passed. |
| Explicit external QuickFIX conformance, race enabled | All 505 scripts passed; 506 events including the parent test. Fixture revision `236c791cfcbca8d57a36f92927f9e8265349359b`. |
| Leader's 15-second Groups / dictionary Validate / Acceptor fuzz campaigns | Passed: approximately 648,890 / 1,197,231 / 25,588 executions. |
| Independent parser review's 15-second Groups / XML Load / Framer fuzz campaigns | Passed: approximately 359,587 / 66,693 / 452,531 executions. |
| `govulncheck`, root and all three contrib modules | Completed with no findings for the scanned graphs. Scanner v1.7.0; official database snapshot last modified 8 October 2026, 22:31:09 UTC. |
| Staticcheck | No valid completed analysis. Installed v0.6.1 reported no matching packages; a separately built v0.8.1 failed to decode Go 1.27 export-data version 5. These are tool failures, not clean results or source findings. Logs preserved. |
| New regression assertions | A4-01 and A4-03 fail as expected; A4-02 and A4-04 diagnostics pass while demonstrating the behavior and controls. |

Selected statement coverage in the root race run: parser/root 98.6%, dictionary 97.5%, XML loader 99.1%, generator 95.0%, session 92.6%, memstore 91.9%. Network coverage from that sandboxed run is not representative; its network tests ran separately. Coverage supports the review but does not establish failure-mode completeness.

Vulnerability checks initially lacked sandbox network access and were rerun successfully with access to the official database. Staticcheck cache permission problems were worked around with temporary writable caches before the export-format incompatibility was established. No failed or skipped check is represented as passing.

This audit did not execute Linux/amd64/arm64 CI, conduct a long-duration production load/soak, connect to live venues, or inject machine crashes, power loss, disk-full or physical storage failures. The [operations guide](../operations.md) remains necessary deployment guidance. Passing the checks here does not establish those untested operating guarantees.

## Reproduction and handoff

Create an isolated archive of the audited revision and copy the evidence into its relevant packages. The session reproductions reuse existing test helpers; the SQLite reproduction belongs to its separate module:

```sh
audit_tmp=$(mktemp -d)
git archive 87fad5a6c5ce0388f204b5210b4fb6a5abe90672 | tar -x -C "$audit_tmp"
cp docs/audits/audit-evidence/production-readiness-4/session/buffered-arrival-time_test.go.txt "$audit_tmp/session/audit4_arrival_test.go"
cp docs/audits/audit-evidence/production-readiness-4/session/written-receipt-reset_test.go.txt "$audit_tmp/session/audit4_receipt_test.go"
cp docs/audits/audit-evidence/production-readiness-4/runtime/sqlite_replay_cancel_test.go.txt "$audit_tmp/contribs/store/sqlite3/audit4_replay_test.go"
cp docs/audits/audit-evidence/production-readiness-4/parser/profile-fallback-cost_test.go.txt "$audit_tmp/dictionary/xmldict/audit4_profile_test.go"
(cd "$audit_tmp" && go test -race -count=1 -run '^TestAudit4' -v ./session)
(cd "$audit_tmp/contribs/store/sqlite3" && go test -race -count=1 -run '^TestAudit4' -v .)
(cd "$audit_tmp" && go test -count=1 -run '^TestAudit4' -v ./dictionary/xmldict)
```

The first test command intentionally fails before correction. The other two are behavior diagnostics. A writable `GOCACHE` may be needed in constrained environments. For the related stash-draining checks, also copy `session/stale-held-header_test.go.txt` into that package.

Implementation order: repair arrival metadata and the fatal-header drain boundary; propagate replay-read cancellation; preserve receipt evidence by level; then remove the custom-profile allocation amplification. Convert applicable reproductions into permanent regressions and rerun the relevant suites, broad race/SIMD/contrib checks and explicit conformance. Obtain a Go-1.27-compatible static-analysis run before describing that check as complete.
