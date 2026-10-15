# fix2 production readiness review — third audit

## Decision

**The second audit's three defects are resolved. One reproducible session-validation defect remains, together with a conditional performance issue for unusually wide custom dictionaries. Correct the session defect before granting general production approval; address or explicitly exclude the wide-schema workload for deployments that support it.**

No P0/P1 defect was confirmed. Both findings below are P2 / medium, with different exposure: T01 affects established sessions processing malformed input; T02 requires an unusually wide configured dictionary. A controlled pilot with known counterparties and dictionaries remains reasonable with the operating controls in [operations.md](../operations.md). Passing this audit's checks does not establish power-loss durability, long-duration capacity, or compatibility with every venue.

A follow-up review of this audit widened both findings; see [Review addendum](#review-addendum). T01 is one case of a malformed-input path that skips every header and sequence check, and T02 has two width-dependent costs besides the linear lookup. Neither finding rises to P0/P1.

This is an audit and implementation handoff. Production source and existing tests were not changed. New reproductions are preserved as `.go.txt` files and were executed in isolated checkouts; the deliberately failing assertions are outside the normal test suite.

## Scope and evidence boundary

- **Date:** 15 October 2026.
- **Audited revision:** `f24332d945009938ebfc92f697cab00bf6f5ab82`; unchanged through completion of validation.
- **Previous audit revision:** `4ced074dcaf057274578c23a3e8d0d93d740bf33`.
- **Environment:** macOS arm64, Go 1.27.2; module targets Go 1.27.
- **Scope:** previous remediation, framing/parsing/building, dictionaries and generation, session protocol/lifecycle/receipts, memory limits, persistence/retention, SQLite chunked replay reads, store timeouts, TCP/TLS, contrib clients and shutdown, operational documentation, and CI configuration.
- **Approach:** independent focused source reviews plus root/contrib race suites, normal/SIMD builds, explicit QuickFIX conformance, targeted regressions and fuzzing, vet, Staticcheck, and dependency vulnerability scans.

The diff from the second audit contains 64 files, 4,462 insertions and 285 deletions, including imported audit reports and evidence. Review emphasis was on the new production behavior. T01 and T02 were found in older paths; neither is attributed to the latest remediation commits. This audit does not claim a line-by-line rereview of every generated fixture.

The previous reports remain preserved: [first audit](production-readiness-review.md), [second audit](production-readiness-review-2.md).

## Findings

### T01 — P2: malformed-tag rejection bypasses CompID and timestamp enforcement

**Confidence:** high; independently reproduced with the race detector.

**Sources:** [malformed-input fast path](../../session/protocol.go#L45), [invalidTagLocked](../../session/protocol.go#L149), [normal header policy](../../session/protocol.go#L370). Line references concern the audited revision.

When parsing fails on a malformed tag number after the header, `handleFrame` calls `invalidTagLocked`. That function checks the expected MsgSeqNum, sends Reject reason 0, and advances the session. It does not apply the normal CompID and SendingTime checks. Normal processing sends Reject and Logout for a wrong CompID or inaccurate SendingTime; `checkHeaderLocked` documents that policy and `TestHeaderProblemCountsTheMessage` covers it.

**Reproduction:** establish a FIX 4.4 session, then send a correctly checksummed sequence-2 order containing `abc=1` after its header. Test two variants: SenderCompID `WRONG`, and the correct SenderCompID with SendingTime one hour old. In both cases the engine sends Reject reason 0, stays logged on, and accepts a valid sequence-3 TestRequest:

```text
wrong SenderCompID: State=logged on NextTarget=4
stale SendingTime:  State=logged on NextTarget=4
```

**Impact:** malformed body fields suppress the expected session termination for invalid identity/time headers, allowing the connection to continue processing messages. This is a protocol-validation bypass on an already established connection. The reproduction does not demonstrate authentication bypass or delivery of the malformed order to the application.

**Correction:** apply the normal header policy before deciding how to reject a malformed body tag, using only header fields that can be trusted from the partial parse. Preserve the normal expected-sequence accounting and the rule that buffered messages are not released through an identity/time failure that requires Logout. Preserve malformed-input secret redaction.

**Acceptance criteria:**

- Wrong SenderCompID, wrong TargetCompID and inaccurate SendingTime cannot avoid the normal Logout policy by adding a malformed body tag.
- Valid headers with malformed tags retain the intended invalid-tag Reject behavior.
- Missing/invalid headers, reset-mode messages, sequence gaps and duplicates retain intentional sequence handling.
- Normal header regressions and malformed-input redaction tests remain green.

**Evidence:** [reproduction source](audit-evidence/production-readiness-3/session_malformed_header_test.go.txt), [independent race execution](audit-evidence/production-readiness-3/session-repro.log). Both assertions fail as expected on the audited revision.

### T02 — P2, conditional: strict group validation scales with configured schema width

**Confidence:** high for the cost behavior; production significance depends on the configured dictionaries.

**Sources:** [linear position lookup](../../dictionary/checks.go#L350), [per-entry clearing and lookup](../../dictionary/checks.go#L412), [first structural walk](../../dictionary/validate.go#L111), [second structural walk](../../dictionary/validate.go#L236).

Strict validation searches `spec.Nested` and `spec.Tags` linearly for each group field and clears the entire schema-sized presence bitset for every entry. `CheckAll` walks group structure twice. Consequently, small entries referencing a field near the end of a wide schema incur work proportional to entries × configured schema width. The indexed lookup used by ordinary group reading does not bound this strict-validation path.

**Public-API reproduction:** load a QuickFIX XML dictionary through `xmldict.Load`, then validate 20,000 two-field entries. The 65,536-field dictionary is 5,609,429 bytes and is accepted by the loader. The message is 300,040 bytes, below the default 1 MiB body limit. An independent run measured:

| Group schema width | Wire bytes | `Validate` | `ValidateWith(CheckAll)` |
|---|---:|---:|---:|
| 64 | 280,040 | 0.682 ms | 2.230 ms |
| 4,096 | 280,040 | 0.599 ms | 44.374 ms |
| 65,536 | 300,040 | 1.090 ms | 659.394 ms |

These are local diagnostic measurements, not stable benchmark thresholds. A separate reviewer reproduced the same scaling. The bundled dictionary fixtures examined had at most 76 direct fields/nested counters in a group, so the extreme case is not representative of those fixtures.

**Impact:** deployments enabling strict validation with unusually wide custom/runtime-loaded dictionaries can spend substantial session CPU on relatively small valid messages. The peer supplies the message; the deployment must already have loaded the wide schema. This is a conditional capacity/denial-of-service concern, not evidence that ordinary bundled FIX dictionaries suffer the measured delay.

**Correction:** index validation positions for wide groups and avoid clearing untouched schema state per entry, or define and enforce a supported schema-width limit. Preserve ordering, required-field and nested-group semantics. Do not disable strict validation merely to hide the cost.

**Acceptance criteria:** after dictionary/index initialization, group validation avoids work proportional to entries × unused schema width; any per-validation schema-sized scratch initialization is bounded and measured. Alternatively, unsupported widths fail explicitly during dictionary loading. Include a diagnostic/benchmark across widths alongside existing group correctness regressions.

**Evidence:** [diagnostic source](audit-evidence/production-readiness-3/parser_wide_group_cost_test.go.txt), [independent execution](audit-evidence/production-readiness-3/wide-group-repro.log). The diagnostic passes while logging timings; it intentionally has no machine-dependent performance assertion.

## Review addendum

A second pass checked T01 and T02 against the source at the audited revision, then had the findings cross-reviewed independently. Every item below was confirmed in the source; items 1 and 2 were also reproduced.

### T01 widened: the malformed-tag path skips every header and sequence check

`invalidTagLocked` checks only that MsgSeqNum is the one expected, then sends a reason-0 Reject and counts the message. Besides CompID and SendingTime, it skips:

1. **MsgSeqNum too low, without PossDup:** the frame is dropped silently. The well-formed path logs out with `MsgSeqNum too low, expecting 2 but received 1`.
2. **MsgSeqNum too high:** the frame is dropped silently, without a ResendRequest. The gap is only noticed when a later message arrives.
3. **BeginString:** a wrong one is not logged out ([normal check](../../session/protocol.go#L296)).
4. **PossDupFlag and OrigSendingTime:** a missing OrigSendingTime is not rejected, nor one later than SendingTime logged out ([normal check](../../session/protocol.go#L489)).
5. **Reset-mode SequenceReset:** a malformed one counts its own MsgSeqNum, which the normal path never does.
6. **Redaction:** the partial parse stops at the bad tag, so a Password (554) after it is never seen. `rejectLocked` logs MsgType through `withhold`, which compares only parsed fields: a credential misplaced in MsgType would be logged. `garbled` refuses to log MsgType for this reason, and the malformed path should follow the same rule. Traced in source, not executed.
7. **Initiator Logon reply:** before the session is established, a malformed frame is dropped without a log line or a `Garbled` count. The session later logs a Logon timeout, which does not say why. An acceptor's first Logon is not affected: [the acceptor](../../session/acceptor.go#L415) logs "first message does not parse" and closes.

**Correction, replacing T01's:** send the malformed path through the normal checks in the normal order: BeginString, then the header (CompIDs, SendingTime), then the sequence and PossDup checks, then the reason-0 Reject for a message in sequence. Check the header before the sequence, as [normal processing](../../session/protocol.go#L710) does: checking the sequence can advance it and release buffered messages. A message received too high must start gap recovery, but its bytes cannot be buffered: [buffered frames that do not parse are discarded](../../session/protocol.go#L675) on replay. Keep the Reset-mode SequenceReset rules. Log what `garbled` logs about such frames, not MsgType.

**Severity:** P2/P3. The malformed message is never delivered, and each well-formed message is still fully checked. When Authenticate accepts every Logon (BaseHandler), a counterparty can log on with valid CompIDs anyway, so skipping the checks on dropped frames gives it no extra access.

**Evidence:** [sequence reproduction](audit-evidence/production-readiness-3/session_malformed_sequence_test.go.txt) and [its race run](audit-evidence/production-readiness-3/session-sequence-repro.log). Its tests pass while logging that the session stays logged on (`state=logged on next=3`); the well-formed control logs out.

### T02 widened: three costs scale with schema width

1. **Linear position lookup.** `position` scans `spec.Tags`, although the root package already indexes groups of 16 or more tags and nested groups, so that ["a crafted dictionary cannot make a wide group cost quadratic time"](../../group.go#L100). [`rulesOf`](../../dictionary/validate.go#L397) already builds a position map; `groupRules` can keep it.
2. **Full bitset clearing per entry.** `clear(e.seen)` costs width ÷ 64 words per entry. The reader [clears only the words it set](../../group.go#L213); validation should do the same. Indexing alone does not meet T02's acceptance criterion.
3. **Activation scan per entry.** [`complete`](../../dictionary/checks.go#L454) calls `anySet` over each optional component's activation positions, for every entry. A wide optional component that is absent costs entries × its width, even with items 1 and 2 fixed.

**Severity:** P3 for the bundled dictionaries (the widest group, NoLegs in FIX 5.0 SP2, has 76 fields), conditional P2 where wide custom schemas are supported. An independent rerun measured 2.2 ms, 42.5 ms and 668 ms for `CheckAll` at widths 64, 4,096 and 65,536.

### Staticcheck

There is no golangci configuration at the root; each contrib module has one. CI runs `make vet`, the tests and fuzzing, not Staticcheck, so the non-clean result is not a failed release gate.

## Disposition of previous findings and limitations

| Second-audit item | Third-audit result | Evidence/qualification |
|---|---|---|
| R01: structural store errors leave sessions running | Resolved | Manual target, retention and replay paths now halt; targeted wrapped/joined structural-error regressions pass. |
| R02: unwritten GapFill terminally skips receipts | Resolved | `conn.wrote` updates receipts/statistics only for completed frames. Zero-byte, partial-frame and earlier-complete/later-failed cases pass. |
| R03: SQLite overwrites unrelated `user_version` | Resolved | Version now lives in `fix2_schema`; shared metadata preservation, legacy migration and future FIX-version rejection are covered and pass. |
| Required group fields/order not validated | Resolved under `CheckAll` and session defaults | New required-field, optional-component, nested-group and order tests pass. Plain `Validate` deliberately remains less strict; venue order exceptions must be selected explicitly. |
| Count-only memory limits and oversized retained buffers | Substantially improved | Pending/held/stash/delivery byte limits, retained-buffer trimming, receipt/rejection bounds and related race regressions pass. Store memory, concurrent callers and configured session counts still require sizing. |
| SQLite replay holds a long read snapshot | Improved | Chunked reads release the connection before callbacks; concurrent append, truncation/reset and checkpoint tests pass. Replay pins can defer deletion until a later truncate/reopen, as documented. |
| Hung storage can hide session state and block Stop | Improved with a defined boundary | Watchdog regressions pass: terminal state/error and connection closure become observable and Stop returns, while the underlying blocked write may persist until the OS returns. |

The first audit's F01–F12 fixes remain covered by the current broad suites and focused reviews. F10 shared delimiters, F11 malformed-frame recovery and F12 implicit LENGTH generation received focused parser/generator checks. This is not a fresh replay of every original first-audit reproduction. Reset announcements retain the explicit QuickFIX/Strict2020 compatibility choice described in the earlier reports.

An additional shutdown hypothesis was rejected after source inspection and testing: blocked initial `ToAdmin` and `SignLogon` callbacks run outside the session mutex. External-context Stop returned at its 50 ms deadline; Done closed after callback release. The [passing regression](audit-evidence/production-readiness-3/stop-blocked-callback-verification.go.txt) and [race output](audit-evidence/production-readiness-3/stop-blocked-callback-verification.log) are preserved so this is not mistaken for an outstanding defect.

## Verification

| Check | Result |
|---|---|
| Root `go test -race -count=1 -json -coverprofile=… ./...` | Passed; 2,824 passing test/subtest events, three test skips. |
| Root `GOEXPERIMENT=simd go test -race -count=1 -json ./...` | Passed; 2,824 passing test/subtest events, same three test skips. |
| `make test-contribs GO_TEST_FLAGS='-race -count=1'` | Passed for Binance, Coinbase and SQLite modules. |
| `make vet` | Passed for root and all three contrib modules. |
| Explicit external QuickFIX conformance, race enabled | All 505 scripts passed; 506 passing events including the parent test. |
| Two race-disabled allocation assertions | Passed separately without race instrumentation. |
| Structural-store, replay-frame, memory-limit, watchdog, routing/authentication and callback-focused race regressions | Passed. |
| Dictionary/XML/generator and parser/framer/builder focused suites | Passed. |
| 15-second dictionary `FuzzValidate` campaign | Passed; 3,707,488 executions. |
| 15-second session `FuzzAcceptor` campaign | Passed; 60,126 executions. |
| 15-second root `FuzzGroups` campaign | Passed; 2,572,146 executions. |
| `govulncheck -json ./...`, root plus three contrib modules | No findings reported in the scanned graphs. |
| Root Staticcheck through installed golangci-lint | Nonzero exit: 13 findings; see below. |
| Same Staticcheck configuration in all three contrib modules | Zero findings. |
| T01 independent regression under race | Failed both expected assertions, confirming the defect; no data race reported. |
| T02 independent diagnostic | Passed and reproduced width-dependent validation cost. |

The broad root skips are two allocation checks intentionally disabled under race and the separately exercised external conformance harness. TCP/TLS tests ran successfully. Conformance used checkout `236c791cfcbca8d57a36f92927f9e8265349359b` of quickfix-go:

```sh
FIX2_QUICKFIX_TESTS=/absolute/path/to/quickfix-go/_test \
  go test -race -count=1 -parallel=4 -run '^TestConformance$' ./session
```

Root statement coverage includes parser/root 98.6%, dictionary 97.5%, XML dictionary 99.1%, generator 95.0%, session 92.1%, memstore 91.9%, and TCP transport 92.1%. Coverage supports the review but does not establish completeness, as T01 demonstrates.

The vulnerability scanner was `govulncheck v1.7.0`; the official database reported a last-modified time of **8 October 2026, 22:31:09 UTC**. Zero findings are bounded by that database snapshot and the analyzed Go symbol graphs. This does not constitute an independent vulnerability analysis of SQLite's bundled C implementation.

Staticcheck ran as `golangci-lint run --no-config --default none --enable staticcheck --timeout 5m ./...`. Root findings comprise eight QF suggestions, two ST style/signature warnings and three SA4000 warnings on fixture singleton-identity assertions (`Dictionary() != Dictionary()`). Source inspection shows those assertions intentionally compare memoized calls. These findings did not establish an additional production defect, but **Staticcheck is not clean**. Its [full output](audit-evidence/production-readiness-3/staticcheck.log) is preserved; this audit did not run or claim the entire default lint set.

A [machine-readable verification summary](audit-evidence/production-readiness-3/verification-summary.json) records package outcomes, coverage, skips, scanner metadata, counts and hashes of bulky raw artifacts. Raw test JSON, coverage and scanner output remain locally under `/private/tmp/fix2-production-readiness-3-f24332d`; compact logs and reproductions are included beside the report.

## Deployment conditions and remaining evidence gaps

These are operating responsibilities or unverified guarantees, separate from T01–T02:

1. **Set memory and retention budgets.** Byte caps now cover substantially more buffers, but oversized outbound messages, concurrent Send callers, all sessions together and the store need separate budgets. Default memstore without retention keeps outgoing messages indefinitely. SQLite retention deliberately protects uncertain/unwritten and pinned replay data, so its Max is not a hard disk ceiling.
2. **Authenticate counterparties and configure TLS.** BaseHandler still accepts Logons by design; the new warning is useful but does not enforce authentication. Dynamic Resolve runs before Authenticate and must gate creation itself. Route/Resolve callbacks need their own deadlines.
3. **Protect logs, credentials and persistence.** Identity fields can contain API-key identifiers; observer tag overrides replace default redaction. Stored Logons contain unredacted credentials. AsyncObserver can drop frames, and the store is not a complete wire audit trail. Use the documented transport boundary for byte-level capture if required.
4. **Accept the storage fault boundary explicitly.** SQLite on macOS deliberately does not enable `fullfsync` or promise power-loss survival. This audit did not perform power cuts, machine crashes, disk-full or physical fsync/checkpoint-failure campaigns. StoreTimeout signals a halt; it does not terminate a blocked kernel operation or reclaim every waiting goroutine.
5. **Gather release-environment evidence.** Runtime verification here was macOS arm64. Linux amd64/arm64 and SIMD CI are configured but remote CI results were not independently inspected. No live Binance/Coinbase session, long-duration soak, production traffic sizing, or exhaustive custom-dictionary/counterparty interoperability campaign was performed.

## Reproduction and implementation handoff

Use an isolated checkout of the audited revision. From the repository root:

```sh
audit_tmp=$(mktemp -d)
git archive f24332d945009938ebfc92f697cab00bf6f5ab82 | tar -x -C "$audit_tmp"
cp docs/audits/audit-evidence/production-readiness-3/session_malformed_header_test.go.txt \
  "$audit_tmp/session/audit3_malformed_header_test.go"
cp docs/audits/audit-evidence/production-readiness-3/parser_wide_group_cost_test.go.txt \
  "$audit_tmp/dictionary/audit3_wide_group_test.go"
(cd "$audit_tmp" && go test -race -count=1 -run '^TestAudit3MalformedTagStillChecksHeader$' -v ./session)
(cd "$audit_tmp" && go test -count=1 -run '^TestThirdAudit' -v ./dictionary)
```

The session command should fail both assertions before correction. The dictionary command logs timings and passes. Adapt the session diagnostic into permanent assertions for the intended header-error precedence when implementing the fix.

Next actions:

1. Fix T01 at the malformed-input/header-validation boundary and sweep adjacent partially parsed message paths.
2. Decide whether wide custom dictionaries are supported; remediate T02 or enforce their supported width.
3. Rerun targeted regressions, root/contrib race suites, vet, normal/SIMD tests and explicit conformance after changes.
4. Resolve or explicitly baseline the Staticcheck findings if lint cleanliness is a release requirement.
5. Validate the operating conditions above against the actual deployment before production approval.
