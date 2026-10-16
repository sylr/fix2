# fix2 production readiness review — sixth audit

## Decision

**Audit five's four findings are resolved. No new P0/P1/P2 defect was confirmed. One newly identified P3 remains: a peer Logout received in sequence releases later buffered application messages before final closure, while the equivalent held-Logout path preserves them.**

This low-severity recovery edge case does not establish a general production blocker. The evidence is substantially stronger than at audit five: normal/SIMD/contrib race suites, all 505 external conformance scripts, vet and targeted closing/retention checks pass. Deployment-specific load, platform and storage-fault validation remain outside this audit; this is not an unconditional durability or capacity certification.

Only documentation and evidence were added. Production source and existing tests are unchanged. The deliberately failing regression is saved as `.go.txt` outside the normal suite and executed in isolated archives of the audited revision.

## Scope and evidence boundary

- **Date:** 16 October 2026.
- **Revision:** `7a6734b70c468a797d01cfaf0366c86c5f9fbe81`.
- **Previous audited revision:** `0da258079503502ece32633a0030bf21e4b2a529`.
- **Environment:** macOS arm64, Go 1.27.2; all project modules target Go 1.27.
- **Scope:** audit-five remediation, final Logout/replay deadlines, reset and queue admission, retained replay data, receipts and persistence, TCP cancellation, implicit DATA generation, parser/dictionary/builder paths, contrib clients and output, operational documentation and CI.
- **Method:** focused independent session, runtime/storage and generator/TCP reviews; leader inspection and independent reproduction of the new finding; broad normal/SIMD race suites, explicit conformance, vet, Staticcheck, short fuzzing and dependency vulnerability scans.

The [fifth report](production-readiness-review-5.md), including its review addendum, is the remediation baseline. Earlier fixes were checked through current suites and focused source review; this is not a fresh replay of every historical reproduction or a line-by-line review of every generated file.

## Finding

### A6-01 — P3: direct peer Logout releases later-numbered buffered orders

**Confidence: high for the observed inconsistency; independently reproduced and cross-reviewed.**

**Sources:** [direct Logout advances before closure](../../session/protocol.go#L1209), [nested drain behavior](../../session/protocol.go#L781), [phase-sensitive drain](../../session/protocol.go#L790).

When an in-sequence peer Logout arrives, `onLogoutLocked` marks it and calls `advanceLocked` before sending its response, changing phase or entering final closure. The drain still sees the established phase and releases later stashed messages. When the Logout itself was held, the nested advance only updates the sequence; the enclosing drain observes the phase change and stops. That held-Logout boundary already has a permanent regression test.

**Reproduction:** establish a Strict2020 session expecting sequence 2. Receive order 3 and wait for the ResendRequest, proving the order is held. Fill sequence 2 with a peer Logout. Order 3 reaches `FromApp`, next target becomes 4, and the session disconnects. Three race executions and the leader's independent run reproduce this.

Two controls distinguish the defect from ordinary gap recovery:

- Filling sequence 2 with a Heartbeat correctly delivers held order 3.
- Holding Logout 3 and order 4, then filling sequence 2 with a Heartbeat, delivers no order and stops at target 4. This preserves the existing held-Logout policy.

**Impact and boundary:** the direct path releases application traffic beyond the peer's Logout sequence position before entering final closure, whereas the held path preserves it. Exposure requires ahead-of-sequence traffic followed by a lower-sequence Logout; ordinary correctly sequenced traffic does not trigger it. The finding does not demonstrate message loss, duplicate execution or an authentication problem. It also does not show delivery after the final Logout was written, after disconnect or after `OnLogout`: application delivery precedes `OnLogout`. Normal input handling during the earlier grace period while awaiting our own Logout's answer is intentional and is not the issue.

This is newly identified, not attributed to the latest remediation commits. The missing direct-path boundary predates those changes.

**Correction and acceptance:** apply one peer-Logout sequence boundary before releasing later held messages, consistently for direct and held handling. Preserve recovery of messages before Logout, the earlier Logout grace period, LastMsgSeqNumProcessed, receipt/store accounting and final-response flushing. Keep the Heartbeat and held-Logout controls alongside the direct-path regression; include crossing Logout coverage. Do not set the final queue-admission gate before queuing the required Logout response.

**Evidence:** [regression and controls](audit-evidence/production-readiness-6/session/in-sequence-logout-drain_test.go.txt), [three race executions](audit-evidence/production-readiness-6/session/in-sequence-logout-drain.log), [independent execution](audit-evidence/production-readiness-6/session/logout-drain-independent.log), [review notes](audit-evidence/production-readiness-6/session/review-notes.md). Only the direct-Logout assertion fails; both controls pass. No data race is reported.

## Review addendum

A second pass reran both diagnostics in an isolated archive at the audited revision, and they behaved as reported:
- the direct-Logout case fails, with order 3 delivered and the next target at 4;
- the Heartbeat and held-Logout controls pass;
- all three retention cases pass.

The finding was then confirmed in the source, and this pass agrees with P3.

### A6-01 widened: the answer to our own Logout has the same gap

The drain in [`advanceLocked`](../../session/protocol.go#L792) stops only when the phase changes. Answering our own Logout does not change the phase: [the answer branch](../../session/protocol.go#L1183) is already in `phaseLogoutSent`, and it calls `advanceLocked` and then `closeAfterFlushLocked`, which sets `closing` but leaves the phase as it was.

So with crossing Logouts, an order held ahead of a gap is delivered when the counterparty's in-sequence Logout fills that gap. The reproduction:
- establish a session, start `Logout`, and read our Logout;
- the counterparty sends order 3, then its Logout answer as sequence 2;
- order 3 reaches `FromApp`, and the next target becomes 4.

The held variant does not apply here: an answer received out of sequence ends the connection at once, without recovering the gap. [Diagnostic](audit-evidence/production-readiness-6/session/crossing-logout-drain_test.go.txt), [output](audit-evidence/production-readiness-6/session/crossing-logout-drain.log). The second subtest's EOF is that immediate close, not a defect.

### One boundary for both paths

Two changes would give both Logout paths, and a held Logout, the same boundary:
- **The drain stops once closing:** add `!c.closing` to the loop condition at protocol.go#L792.
- **Each Logout path closes before it advances:** queue the answer and call `closeAfterFlushLocked` before `advanceLocked`, so the nested drain stops at once. `NextTarget` still moves past the Logout.

This pass tried both changes in the scratch archive:
- both direct paths stop releasing the held order;
- the Heartbeat and held-Logout controls still pass;
- the full session race suite passes, apart from the inapplicable held-answer probe, and so do all 505 conformance scripts.

Keep `markLocked` before the answer is queued, so the answer's LastMsgSeqNumProcessed still counts the Logout. This also keeps the rule from audit five: the answer is queued before closing starts. The existing `TestHeldArrival_HeldLogoutStopsHeld` covers the held path. The regressions should cover:
- the direct Logout;
- the direct crossing answer;
- the Heartbeat control.

### Retention diagnostic

`TestAudit6CloseDeadlinePreservesRetention` covers what `2e0f715` recorded as not tested: retention after a resend cut short by the close deadline. It passes at the audited revision. It is worth keeping as a permanent test.

## Audit-five remediation

| Previous item | Sixth-audit disposition |
|---|---|
| A5-01: final Logout waits indefinitely behind replay | Resolved for the reported cases. A separate close deadline disconnects and cancels replay. Real SQLite tests cover peer-initiated/crossing Logout while the spare pool connection remains reserved; repeated race executions pass. [SQLite evidence](audit-evidence/production-readiness-6/runtime/sqlite-closing-remediation.log). |
| A5-02: responses/replays can follow final Logout | Resolved for the reported paths and addendum. Queue admission refuses new output without consuming sequence numbers; reader-triggered rejects/replays are suppressed; reset Logon cannot escape stalled-handler closure. Existing resends requested before Logout and healthy slow response writes remain covered. [Session evidence](audit-evidence/production-readiness-6/session/a5-remediation.log). |
| A5-03: canceled Accept poisons listener reuse | Resolved. Accept joins a started cancellation callback before restoring the deadline, under its serialization lock. Original and current cancellation/reuse cases pass with race and loopback permitted. [TCP evidence](audit-evidence/production-readiness-6/parser/tcp-cancel-permitted.log). |
| A5-04: implicit group DATA partner lacks generated setter | Resolved. Implicit DATA getters/setters are emitted, while explicit merged references avoid duplicate methods. The original implicit/explicit reproduction passes. [Original generator reproduction](audit-evidence/production-readiness-6/parser/generator-original.log). |

Additional shared/nested group controls pass strict validation and binary DATA round trips, contextual ordering, scoped outer/inner getters, computed LENGTH, setter overwrite/copy and missing-value behavior. Generated subprocess checks use plain `go test`; TCP cancellation checks use the race detector. [Generator matrix](audit-evidence/production-readiness-6/parser/generator-shared-nested.log), [diagnostic source](audit-evidence/production-readiness-6/parser/shared-nested-implicit-data_test.go.txt), [review commands and boundaries](audit-evidence/production-readiness-6/parser/generator-tcp-review.log). An initial sandbox-denied TCP run is preserved separately and is not counted as successful verification; the permitted run has no skips.

The final-close retention concern recorded in the remediation commit also received additional evidence. A deterministic, context-aware memstore wrapper and the existing simulated-write helper exercise cancellation before replay, after a complete replay chunk, and alongside successful Range completion with a buffered tail. All requested originals survive a forced `Max=1` retention sweep, only completed replay frames are counted, and prior Written receipts remain stable. An acknowledgement control releases the hold and allows eviction again. The diagnostic passes repeatedly under race and in a leader rerun.

This diagnostic verifies accounting with simulated writes; it is not a real-network interoperability test. It does not force the exact scheduler interval between the final replay context check and flush. Source review confirms that `!c.closed` guards the subsequent retention-delivered update. [Diagnostic source](audit-evidence/production-readiness-6/runtime/retention_close_deadline_test.go.txt), [repeated focused checks](audit-evidence/production-readiness-6/runtime/focused-runtime-race.log), [leader execution](audit-evidence/production-readiness-6/runtime/retention-independent.log), [limitations](audit-evidence/production-readiness-6/runtime/README.md).

No additional confirmed defect emerged from the runtime/store/replay/receipt/retention review. Existing documented deployment boundaries—authentication policy, retained-store growth, observer completeness, callback cooperation and macOS power-loss durability—remain applicable.

## Verification

The [machine-readable summary](audit-evidence/production-readiness-6/verification-summary.json) records commands, package outcomes, skips, scan metadata and static-analysis results. Full JSON test/scanner streams and coverage are retained in `/private/tmp/fix2-production-readiness-6-7a6734b`; compact evidence accompanies this report.

| Check | Result |
|---|---|
| Root normal race suite, uncached test execution, loopback permitted | Passed: 2,974 passing test/subtest events, three skips. TCP/TLS and end-to-end network paths ran. |
| Root SIMD race suite, loopback permitted | Passed: same 2,974 passing events and three skips. |
| Race-disabled allocation checks | Both allocation tests skipped under race passed separately. |
| Contrib race suites | Binance, Coinbase and SQLite passed. |
| `make vet` | Root and all three contrib modules passed. |
| Explicit external QuickFIX conformance with race | All 505 scripts passed; 506 events including the parent. Fixture revision `236c791cfcbca8d57a36f92927f9e8265349359b`. |
| 15-second Builder / dictionary Validate / Acceptor fuzz campaigns | Passed: approximately 567,007 / 1,468,483 / 29,581 executions. |
| Focused closing/replay/retention checks | Passed, including three repeated race executions of the runtime diagnostic and real SQLite remediation tests. |
| `govulncheck`, root and three contrib modules | Completed with no findings for the scanned graphs. Scanner v1.7.0; official database snapshot last modified 8 October 2026 at 22:31:09 UTC. |
| Staticcheck through golangci-lint v2.14.0, built with Go 1.27.1 | Completed. Root exits 1 with the same 16 existing diagnostics: nine quick-fix suggestions, two style diagnostics and five SA4000 diagnostics in fixture singleton tests. Contrib modules report zero issues. |
| A6-01 additional regression | Fails as expected on the audited revision; its two controls pass. |

The three broad-run skips are the two separately exercised allocation checks and the separately enabled conformance harness. No network skip occurred in those runs. Selected statement coverage: parser/root 98.6%, dictionary 97.9%, XML loader 99.1%, generator 95.0%, session 93.6%, TCP transport 93.9%, memstore 91.9%. Coverage supports the review; it does not establish failure-mode completeness.

Staticcheck is not a clean check. Its five SA4000 diagnostics concern `Dictionary() != Dictionary()` in fixture singleton tests; the remaining diagnostics concern style/simplification. None is represented as a new production defect. The current CI uses vet, tests and fuzzing. [Root Staticcheck output](audit-evidence/production-readiness-6/root-golangci-staticcheck.log).

No Linux/amd64/arm64 CI run, long-duration load/soak, live exchange session, benchmark-toolchain execution, machine crash, physical power loss, disk-full or physical storage-fault campaign was performed. The source and local verification do not certify those untested guarantees. Apply the [operations guide](../operations.md) and gather evidence appropriate to the deployment's traffic, retention, storage and supported platforms.

## Reproduction and handoff

The failing session regression and passing retention diagnostic reuse existing package helpers. Run them in an isolated archive:

```sh
audit_tmp=$(mktemp -d)
git archive 7a6734b70c468a797d01cfaf0366c86c5f9fbe81 | tar -x -C "$audit_tmp"
cp docs/audits/audit-evidence/production-readiness-6/session/in-sequence-logout-drain_test.go.txt "$audit_tmp/session/audit6_logout_test.go"
cp docs/audits/audit-evidence/production-readiness-6/runtime/retention_close_deadline_test.go.txt "$audit_tmp/session/audit6_retention_test.go"
(cd "$audit_tmp" && go test -race -count=1 -run '^TestAudit6' -v ./session)
```

This command intentionally fails only the direct-Logout case before correction. A writable GOCACHE may be needed in constrained environments. Correct the narrow boundary in A6-01, keep its controls as permanent regressions, and rerun session race tests plus explicit conformance. The P3 finding does not reopen audit five's resolved general closing-timeout blocker. Broader production acceptance still depends on the deployment evidence listed above.
