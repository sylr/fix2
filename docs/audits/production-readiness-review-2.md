# fix2 production readiness review — second audit

## Decision

**The project is substantially closer to production readiness. The original failure scenarios have been corrected, or, for reset announcements, assigned an explicit compatibility policy. Three reproducible medium-priority defects remain. Hold unrestricted production approval until those defects are resolved and the deployment-specific guarantees below are verified.**

A controlled pilot is a reasonable next stage after remediation, with a dedicated SQLite database, explicit authentication, appropriate memory and retention limits, and application validation for repeating-group requirements. Passing the tests below does not establish power-loss durability, sustained-load capacity, or compatibility with every counterparty.

This is a second, independent verification of the first audit's remediation, plus review of the newly introduced retention and SQLite storage features. It is an implementation handoff for another agent. No production source was changed by the reviewers. The first report remains preserved in [production-readiness-review.md](production-readiness-review.md).

## Scope and revision boundaries

- **Date:** 13 October 2026.
- **First audited revision:** `f85603b7be0613eec3d57ac8b161d49f55b58aac`.
- **Primary second-audit revision:** `c8d99a19ef88c1969d3cf4525baee1159d9a0337`.
- **Final reviewed revision:** `4ced074dcaf057274578c23a3e8d0d93d740bf33`.
- **Environment:** macOS arm64, Go 1.27.2; project targets Go 1.27.
- **Scope:** framing, parsing, dictionaries, generation, session lifecycle and protocol processing, durability and recovery, receipts, retention, transports, logging, Binance/Coinbase clients, the new SQLite module, and CI/build integration.

The remediation diff through the primary revision spans 128 files, approximately 12,842 added lines and 547 deleted lines. During review, HEAD advanced to the final revision. That commit only renamed checksum implementation files and moved the unchanged `findByte` helper into `parse.go`. Its diff was inspected, and root-package race tests were rerun with and without SIMD at the final revision. Session, contrib, and SQLite sources are unchanged between these revisions. Broad validation results primarily concern the primary revision; the final revision received this focused additional verification.

Source links below are pinned to the final revision. Recheck their applicability before editing a later tree.

## Remaining defects

All three findings are **P2 / medium**, with high confidence from source inspection and reproducible behavior. No new P0/P1 defect was confirmed within this review's scope. Their priority does not make the affected guarantees optional: applications relying on terminal receipts, reconciliation after storage faults, or a shared SQLite database can encounter incorrect behavior.

### R01 Structural store errors do not consistently halt the session

**Sources:** [manual target update](https://github.com/sylr/fix2/blob/4ced074dcaf057274578c23a3e8d0d93d740bf33/session/lifecycle.go#L404), [retention truncation](https://github.com/sylr/fix2/blob/4ced074dcaf057274578c23a3e8d0d93d740bf33/session/retention.go#L172), [replay read failure](https://github.com/sylr/fix2/blob/4ced074dcaf057274578c23a3e8d0d93d740bf33/session/replay.go#L207), [store error contract](https://github.com/sylr/fix2/blob/4ced074dcaf057274578c23a3e8d0d93d740bf33/session/store/store.go#L85).

The store contract says structural errors such as `ErrGenerationGone`, `ErrSeqMismatch`, `ErrBackwards`, and `ErrClosed` halt a session until reconciliation. Three paths do not enforce that contract:

| Operation | Injected result | Observed session behavior |
|---|---|---|
| `Session.SetNextTargetSeqNum` | `ErrGenerationGone` | Returns the error, but remains logged on with `Session.Err()==nil`. |
| Background `Truncate` | `ErrGenerationGone` | Logs a warning and remains logged on with `Session.Err()==nil`. |
| Replay `Range` | `ErrGenerationGone` | Starts Logout, but leaves `Session.Err()==nil`; no persistent halt prevents reconnection. |

Each reproduction holds the session's generation unchanged. These failures therefore fall outside the documented exception for an operation concerning a generation the session itself already left.

**Impact:** the session can continue processing or reconnect after discovering invalid storage ownership or generation state. This is an uncommon fault/ownership-change path, rather than a failure during ordinary healthy storage operation. The automatic inbound persistence path addressed by F02 now halts correctly; these are additional callers with the same error-classification requirement.

**Portable reproduction:** wrap `memstore.Store`, override the relevant method to return `store.ErrGenerationGone`, and establish a session. Call the manual setter; separately trigger retention with acknowledged messages eligible for eviction; separately request replay of a stored message. Assert a non-nil terminal session error. All three assertions fail on the reviewed revision.

**Recommended correction:** consistently apply the existing structural-error classification and halt policy. Preserve the distinction between structural errors and ordinary transient failures. Keep the generation-change exception only where the session can establish that it left that generation itself.

**Acceptance criteria:**

- Each of these three operations halts on structural errors for the current generation and prevents reconnection until reconciliation.
- Classification works through wrapped and joined errors; a structural component retains priority over a cancellation or uncertain-outcome component.
- Ordinary transient-error behavior remains intentional and tested.
- Existing reset-generation and recoverable `SetNextTarget` uncertain-outcome behavior remains correct.

### R02 Failed GapFill transmission incorrectly marks receipts skipped

**Sources:** [replay construction/write accounting](https://github.com/sylr/fix2/blob/4ced074dcaf057274578c23a3e8d0d93d740bf33/session/replay.go#L99), [receipt updates before write-error handling](https://github.com/sylr/fix2/blob/4ced074dcaf057274578c23a3e8d0d93d740bf33/session/replay.go#L225), [terminal skipped receipt](https://github.com/sylr/fix2/blob/4ced074dcaf057274578c23a3e8d0d93d740bf33/session/receipt.go#L131).

Replay records every constructed GapFill in `gaps`, then applies those ranges to receipt tracking before inspecting `writeErr`. Consequently, a GapFill that was never written still produces terminal `ErrSkipped` for the affected receipt.

**Confirmed reproduction:** obtain a real receipt from `Session.Send`, successfully complete the original application-message write, veto its resend through `OnResend`, and have the transport return `(0, error)` when replay writes the GapFill. The peer receives no GapFill, but `Receipt.Err()` returns:

```text
session: message gap-filled before the counterparty acknowledged it
```

**Impact:** the application receives terminal evidence that the message was skipped even though the attempted transmission did not establish that result. A counterparty could still request recovery. Applications using receipts for reconciliation or retry decisions can therefore act on false certainty.

**Recommended correction:** associate every GapFill with its ending offset in the output buffer and apply receipt skips only for completely written frames. Retention-loss notification already uses equivalent write-progress accounting; use a consistent approach. Review the construction-time `GapFilled` statistic if its intended meaning is successfully transmitted gap fills.

**Acceptance criteria:**

- Zero-byte and partial-frame failed writes do not mark the affected receipts skipped.
- If an earlier GapFill is completely written before a later frame fails, only the completed GapFill's range becomes skipped.
- Successful GapFill replay retains existing behavior.
- Receipt state and beyond-retention loss notifications agree on which GapFill frames were written.

### R03 SQLite schema version collides with unrelated application metadata

**Sources:** [shared-database contract](https://github.com/sylr/fix2/blob/4ced074dcaf057274578c23a3e8d0d93d740bf33/contribs/store/sqlite3/sqlite3.go#L5), [schema version read/rejection](https://github.com/sylr/fix2/blob/4ced074dcaf057274578c23a3e8d0d93d740bf33/contribs/store/sqlite3/sqlite3.go#L195), [schema version overwrite](https://github.com/sylr/fix2/blob/4ced074dcaf057274578c23a3e8d0d93d740bf33/contribs/store/sqlite3/sqlite3.go#L206).

The store advertises that its `fix2_` tables can coexist with unrelated tables, but uses database-wide `PRAGMA user_version` for its own schema. SQLite defines that integer as application-owned database metadata. [SQLite documentation](https://www.sqlite.org/pragma.html#pragma_user_version).

**Confirmed public-API reproduction:** create an unrelated application table and set `user_version=7`; opening the FIX store fails because its schema version is 1. Set `user_version=0`; opening succeeds and changes the application's version to 1.

```text
Open alongside unrelated schema version 7: sqlite3: loading session "FIX-SESSION": the database's schema is version 7, this store knows up to 1
Open alongside schema version 0: err=<nil>, app user_version now=1
```

**Impact:** shared-database deployments can fail startup or have their migration metadata silently changed. This reproduction does not demonstrate deletion of unrelated tables or FIX message loss. A dedicated FIX database avoids this particular collision.

**Recommended correction:** store the FIX schema version in a namespaced metadata table. Alternatively, explicitly require a dedicated database and remove the sharing promise; that narrows the supported integration contract.

**Acceptance criteria:**

- Under a sharing contract, unrelated `user_version` values survive opening and migrating the FIX store unchanged.
- A genuinely unsupported future FIX schema version is still rejected using FIX-owned metadata.
- Existing databases have an explicit, tested migration path.
- If choosing a dedicated-database contract instead, documentation and opening behavior consistently enforce that restriction.

## Disposition of the first audit

“Resolved” refers to the original scenario and inspected remediation, rather than a guarantee that every adjacent error path is correct. R01 and R02 demonstrate why the new persistence and replay behavior still needs follow-up.

| First finding | Second-audit result | Evidence or remaining qualification |
|---|---|---|
| F01 Missing synchronization before transmission | Resolved | Initial batches, replay, and shutdown synchronize buffered stores. Failed Sync prevents transmission; cancellation and structural errors have dedicated coverage. |
| F02 Failed inbound-prefix persistence | Resolved in original automatic path | Disconnects and withholds subsequent deliveries/loss notifications until persistence reconciles. Structural automatic persistence errors halt. R01 covers other callers. |
| F03 Ignored reset errors / invented post-reset state | Resolved | Failed and uncertain resets are propagated; failure to reopen after reset halts without fabricating state. |
| F04 Active reset skips Logon validation | Resolved | Identity, timestamp, required-field and sequence-1 checks precede reset mutation. |
| F05 Wrong FIXT application DATA profile | Resolved | Header-based application-version selection precedes the definitive parse; selected profiles survive stash and delivery handling. |
| F06 Malformed input leaks passwords | Resolved | Original secret-leak reproduction now reports `secret leaked: false`. Garbled logs carry sanitized errors, counts and offsets; Reject/refusal diagnostics are filtered. Identity logging has a narrower documented policy. |
| F07 Blocked contrib output / shutdown race | Resolved | Single output owner, mutex released before output wait, finalization after session completion, and bounded blocked-output shutdown. Race tests cover stalled pipes, queue pressure, write failures, peer behavior and signals. |
| F08 Received messages dropped when reset passes a gap | Resolved | Held received messages are delivered in sequence; only missing ranges are reported lost. Tests cover persistence failure, reset and Stop interactions. |
| F09 Unannounced acceptor reset | Explicit compatibility policy | `Strict2020` announces applicable resets; QuickFIX compatibility mode deliberately retains its documented behavior. Fresh sessions and both policies are tested. Choose the policy required by the counterparty. |
| F10 Shared group delimiter incorrectly rejected | Resolved | Original delimiter/enclosing-field scenario passes `CheckAll`, returns one group entry, and preserves the enclosing field. Nested boundaries and actual count mismatches are covered. |
| F11 Quadratic malformed-frame recovery | Resolved for reproduced behavior | The old 320 KB one-byte-reader case fell from approximately 602 ms to 2.85 ms in local smoke measurements. Growth tests pass; 50,000 malformed streams gave identical framing outcomes across one-byte/full-buffer reads. These are not sustained-load benchmarks. |
| F12 Missing generated LENGTH field | Resolved | DATA tag 96 without an explicit tag-95 definition now generates `95=3\x0196=a\x01b\x01`; generated-profile parsing round-trips the embedded SOH. Implicit group lengths and unpaired DATA are covered. |

The earlier absence of an included persistent store is also addressed: the SQLite module now supplies persistence, has independent tests, and participates in contrib test/vet targets and CI configuration. Retention is implemented rather than left wholly to external store integrations.

## Deployment constraints still requiring an explicit decision

These are documented limitations, configuration responsibilities, or unverified guarantees. They are separate from R01–R03.

1. **Repeating-group semantic validation is incomplete.** `dictionary/validate.go:68` explicitly permits missing required entry fields and arbitrary entry order, even under `CheckAll`. Both behaviors remain reproducible. Group-only orphan/duplicate fields are better checked, but applications needing complete entry validation must add it or extend the validator.

2. **Memory limits require workload-specific configuration.** `MaxStash` defaults to 10,000 messages; near the default 1 MiB body limit, retained frame bodies alone can approach 10 GiB. Held outbound messages and queued deliveries add memory. `MaxPendingBytes` admission checks the existing pending buffer, so it is not a strict cap including the next message. Configure counts, body sizes and backpressure against a measured memory budget. Filling/resetting a gap can enqueue the stash beyond `DeliveryQueue`, as documented in `session/config.go`.

3. **Retention is optional and has deliberate recovery exceptions.** A zero retention interval keeps all stored messages. Messages held because they may never have been written can exceed the desired retained window. Size disk capacity and choose retention against counterparty replay obligations. Long-running SQLite `Range` callbacks retain read snapshots and can prevent complete checkpoints, allowing WAL growth. [SQLite WAL documentation](https://www.sqlite.org/wal.html).

4. **SQLite durability has a tested boundary.** Race tests and an independent abrupt-process-exit smoke test, without `Close`, preserved synced counters, stored messages, retention holes, skipped ranges, and a subsequent synced reset. Real power loss, machine crashes, disk-full conditions, and physical fsync/checkpoint failures were not tested. NORMAL/WAL commits have weaker machine-failure durability than FULL commits; the store's Sync strategy follows that distinction. macOS `fullfsync` is not enabled. Do not infer universal power-loss durability from process-exit tests. [SQLite synchronous documentation](https://www.sqlite.org/pragma.html#pragma_synchronous).

5. **Storage and shutdown can block together.** SQLite Append runs under the session lock, and package documentation acknowledges that a hung disk can hold both the session and Close. Sync cancellation tests do not establish a hard shutdown deadline under an indefinitely stalled filesystem. Respect the documented connection-pool sizing and one-active-store-per-session-key requirements; the module also requires cgo.

6. **Authentication and observability are deployment responsibilities.** `BaseHandler` accepts authentication by default. Configure appropriate authentication and TLS for the trust boundary. Password/signature redaction is improved, but Coinbase API-key identity values can still appear through CompIDs and Username tracing; the key alone cannot authenticate. `AsyncObserver` is deliberately lossy and cannot be treated as an authoritative durable audit stream.

7. **Operating evidence remains narrower than a release guarantee.** Verification was local macOS arm64. Linux/amd64 CI is configured, but its results were not independently executed here. No long-duration soak, live exchange session, machine-level fault campaign, or production capacity assessment was performed. Conformance requires an external QuickFIX test checkout and was explicitly enabled in this audit; a default root test run skips it.

## Verification performed

| Check | Result |
|---|---|
| Root: `go test -race -count=1 -json -coverprofile=… ./...` | Passed: 2,221 passing test/subtest events, three skips. |
| Root with `GOEXPERIMENT=simd`, race enabled | Passed: 2,221 passing test/subtest events, the same three skips. |
| Final revision root package, race, normal and SIMD | Both passed after inspecting the checksum/helper-only change. |
| `make test-contribs RACE=1 GO_TEST_FLAGS='-race -count=1'` | Passed for Binance, Coinbase and SQLite modules. |
| `make vet` | Passed for root and all three contrib modules. |
| External QuickFIX conformance, race enabled | All 505 scripts passed; 506 passing events including the parent test. |
| Focused session remediation tests | Passed: store failures, active reset, application profiles, held reset and reset-on-Logon. |
| Focused dictionary/framer/generator remediation tests | Passed, including original F10–F12 scenarios. |
| 15-second fuzz run: Groups | Passed; approximately 806,637 executions. |
| 15-second fuzz run: dictionary Validate | Passed; approximately 961,561 executions. |
| 15-second fuzz run: session Acceptor | Passed; approximately 9,450 executions. |
| `govulncheck`, root plus all three contrib modules | No vulnerability findings reported for the scanned graphs. |
| Four additional session regression assertions | All fail as expected, demonstrating R01's three paths and R02. |
| SQLite metadata public-API reproduction | Demonstrates both R03 outcomes. |

The three skips in each broad root run are two allocation checks intentionally disabled under race instrumentation and the separately exercised external conformance harness. TCP/TLS tests ran in the successful unrestricted rerun. Staticcheck was not exercised in this second audit; successful Go vet is the static-analysis evidence reported here.

The vulnerability scanner was `govulncheck v1.9.0`, using a database last modified on 8 October 2026 at 22:31:09 UTC. Its zero-findings result is bounded by that database snapshot and the source graphs analyzed.

Selected statement coverage from the root race run: parser/root **97.0%**, dictionary **97.1%**, generator **95.0%**, session **89.5%**, XML dictionary **96.9%**, TCP transport **92.1%**, and memstore **91.9%**. Coverage is supporting evidence, not proof of protocol or failure-mode completeness. The additional failing regressions show gaps that existing passing suites did not detect.

The external QuickFIX fixture checkout was at `236c791cfcbca8d57a36f92927f9e8265349359b`. To repeat:

```sh
FIX2_QUICKFIX_TESTS=/absolute/path/to/quickfix-go/_test \
  go test -race -count=1 -parallel=4 -run '^TestConformance$' ./session
```

## Reproduction artifacts and implementation handoff

Small reproductions are preserved as `.go.txt` files so they do not alter the production test suite. The session files reuse existing repository test helpers and must be copied into the corresponding package in a temporary checkout.

- [Internal session regressions: R01 retention/replay and R02](audit-evidence/production-readiness-2/reaudit_internal_test.go.txt).
- [Public session regression: R01 manual target update](audit-evidence/production-readiness-2/reaudit_test.go.txt).
- [Public SQLite metadata reproduction: R03](audit-evidence/production-readiness-2/sqlite_metadata_repro.go.txt).

Example commands, executed from the repository root:

```sh
audit_tmp=$(mktemp -d)
git archive 4ced074dcaf057274578c23a3e8d0d93d740bf33 | tar -x -C "$audit_tmp"
cp docs/audits/audit-evidence/production-readiness-2/reaudit_internal_test.go.txt \
  "$audit_tmp/session/reaudit_internal_test.go"
cp docs/audits/audit-evidence/production-readiness-2/reaudit_test.go.txt \
  "$audit_tmp/session/reaudit_test.go"
mkdir -p "$audit_tmp/audit-repro"
cp docs/audits/audit-evidence/production-readiness-2/sqlite_metadata_repro.go.txt \
  "$audit_tmp/audit-repro/main.go"
(cd "$audit_tmp" && go test ./session -run '^TestReaudit' -count=1 -v)
(cd "$audit_tmp/contribs/store/sqlite3" && go run "$audit_tmp/audit-repro/main.go")
```

The session command is expected to fail four assertions before correction. The SQLite program reports the unwanted behavior rather than failing an assertion. Its database is temporary and is removed after use. The scenarios described above remain the reference if test helpers change.

The next agent should:

1. Confirm R01–R03 against its current revision, then convert each applicable reproduction into a permanent regression test.
2. Correct structural-error handling consistently, while preserving documented reset and uncertain-outcome exceptions.
3. Correct receipt updates using completed-frame write accounting, including partial-write coverage.
4. Resolve the SQLite metadata ownership contract and test existing-store migration.
5. Run the relevant package tests, root/contrib race suites, vet, normal/SIMD builds and explicitly enabled conformance scripts.
6. Record deployment decisions for group validation, authentication, memory, retention and storage durability; gather load/fault/platform evidence appropriate to the intended production environment.

Production source remains unchanged by this audit. Review deliverables are this report and its three reproduction artifacts. There is no general production sign-off in this document; the remaining defects and unverified operating guarantees are stated explicitly so the next agent can close them with evidence.
