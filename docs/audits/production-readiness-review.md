# fix2 production readiness review

## Decision and scope

**Recommendation: hold general production rollout until the seven high-priority findings below are resolved.** The parser and tooling have strong test coverage, but the session engine has reproducible durability, recovery, and validation defects. The market-data clients also have a shutdown race.

This document hands the review to another agent. It preserves the findings, evidence, qualifications, and recommended changes, and adds acceptance criteria for remediation.

- **Reviewed revision:** `f85603b7be0613eec3d57ac8b161d49f55b58aac`.
- **Review date:** 12 October 2026.
- **Environment:** macOS arm64, Go 1.27.2; project target Go 1.27.
- **Scope:** parser, builders, dictionaries, code generator, session engine, transports, stores, Binance and Coinbase contrib clients, benchmark Go module, and CI.
- **Review method:** source inspection, existing tests, isolated regression reproductions, race detection, conformance scripts, short fuzz runs, and a root-module vulnerability scan.
- **Repository state:** the reviewed tree was clean. Review reproductions were created outside the repository; no tracked project files were changed during the review.

Source links are pinned to the reviewed commit. Line numbers and findings must be checked against the current tree before implementation.

## How to use this handoff

1. Read the applicable repository instructions and inspect the current revision. Record which findings still apply.
2. Reproduce each applicable defect with a regression test that fails against the reviewed behavior. Temporary evidence paths in the appendix are conveniences; the scenarios below are the portable reference.
3. Fix the relevant cause and preserve the stated compatibility constraints. Treat documented limitations and compatibility choices separately from unintended defects.
4. Verify each finding against its acceptance criteria and run the relevant package and module checks.
5. Report each finding as resolved, still reproducible, or explicitly constrained by the supported deployment contract. Include test commands and results.

The store findings F01–F03 share durability and error-handling concerns and should be designed together. F04–F05 concern incoming protocol validation. F06 concerns logging, and F07 concerns contrib client lifecycle behavior.

## High-priority findings

### F01 Buffered stores are not synchronized before transmission

**Fixed in:** [`97c3129`](https://github.com/sylr/fix2/commit/97c31298944a8f668049e6be881a71caed2006a6) (sync before sending), [`17f269d`](https://github.com/sylr/fix2/commit/17f269d81cce08c661fd9ef4d9abf41556bc5658) (a cancelled Sync halts on a store that moved on).

**Priority:** high. Applies to stores whose `Append` buffers writes until `Sync`.

**Sources:** [session/session.go:326](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/session.go#L326), [session/conn.go:323](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/conn.go#L323), [session/store/store.go:92](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/store/store.go#L92).

The store contract permits `Append` to buffer writes until `Sync`, but the engine never calls `Syncer.Sync`. A compliant buffered store can lose messages and sender sequence state after the peer receives them.

**Reproduction:** wrap a store with a `Sync` method that records calls and returns an error. Send an application message and wait for `Written`. The reviewed engine returns success with zero `Sync` calls; the failing durability barrier is never consulted.

**Recommended change:** establish durability before transmission, preferably for each writer batch. Propagate synchronization failures and halt on uncertain outcomes. Define replay and shutdown durability semantics. Preserve the meaning of receipts: `Written` describes transport progress, not application processing by the peer.

**Acceptance criteria:**

- A buffered store is synchronized through every sequence number before that message is transmitted.
- A synchronization failure prevents the affected batch from being transmitted.
- Uncertain outcomes cannot permit reuse of an uncertain sequence number.
- Tests cover successful synchronization, synchronization failure, batching, replay, and shutdown.

### F02 Inbound persistence failures leave the session running

**Fixed in:** [`97c3129`](https://github.com/sylr/fix2/commit/97c31298944a8f668049e6be881a71caed2006a6) (a failed record disconnects), [`17f269d`](https://github.com/sylr/fix2/commit/17f269d81cce08c661fd9ef4d9abf41556bc5658) (queued deliveries stop), [`a01bf77`](https://github.com/sylr/fix2/commit/a01bf77a90a81884cb291da301ade071261b48b0) (held, never dropped).

**Priority:** high. Applies when the processed receive prefix cannot be persisted.

**Source:** [session/delivery.go:340](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/delivery.go#L340).

Failures from `SetNextTarget`, including `ErrUnknownOutcome`, are logged while processing continues. A crash can replay arbitrarily many already processed business messages. Normal batching already permits bounded replay; repeated persistence failures remove that bound.

**Reproduction:** make `SetNextTarget` return `ErrUnknownOutcome`. In the isolated test, persisted `NextTarget` remained **1**, while the session advanced to **101**, stayed connected, and had no halted error.

**Recommended change:** disconnect or halt when the processed prefix cannot be persisted, expose the failed state, and document the normal at-least-once delivery contract. Application deduplication remains necessary; this finding does not establish an exactly-once guarantee or prove inbound message loss.

**Acceptance criteria:**

- Persistence failure stops further uncontrolled progress and exposes an actionable failure state.
- An uncertain outcome requires reconciliation before normal processing resumes.
- Tests establish the normal batching behavior and prevent unbounded divergence between processed and persisted prefixes.

### F03 Reset failures are ignored by their callers

**Fixed in:** [`97c3129`](https://github.com/sylr/fix2/commit/97c31298944a8f668049e6be881a71caed2006a6).

**Priority:** high. Applies to reset failure and state reload failure.

**Sources:** [session/logon.go:183](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/logon.go#L183), [session/logon.go:304](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/logon.go#L304), [session/logon.go:316](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/logon.go#L316).

A failed reset disconnects the connection, but Logon processing continues and publishes an established phase. Reconnection can use stale generation state. Additionally, if `Open` fails after a successful reset, the engine invents replacement state rather than propagating the reload failure.

**Reproduction:** make `Reset` return `ErrUnknownOutcome`. The test observed `phaseEstablished`, `c.closed=true`, `s.c=nil`, and `s.halted=nil`.

**Recommended change:** return immediately on every reset error. Halt and reload or reconcile uncertain outcomes. Treat state reload failure after `Reset` as an error instead of assuming the persisted state.

**Acceptance criteria:**

- Failed reset paths do not publish `OnLogon` or an established state.
- Counters and generations do not advance using assumed store state after failure.
- Uncertain reset outcomes block reconnection until reconciled.
- Tests cover initial Logon resets, active resets, schedule and disconnect resets, and `Open` failure after reset.

### F04 Active reset Logons bypass normal validation

**Fixed in:** [`e5ea15b`](https://github.com/sylr/fix2/commit/e5ea15b6891e3be0e564d73ec0f8296c4497c634), [`17f269d`](https://github.com/sylr/fix2/commit/17f269d81cce08c661fd9ef4d9abf41556bc5658) (EncryptMethod required), [`a01bf77`](https://github.com/sylr/fix2/commit/a01bf77a90a81884cb291da301ade071261b48b0) (EncryptMethod rule per Logon).

**Priority:** high. Applies to reset Logons on established connections.

**Source:** [session/logon.go:118](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/logon.go#L118).

On an established connection, `141=Y` enters the reset path before checking CompIDs, SendingTime, heartbeat, application version, or reset sequence constraints. Authentication still runs, but authentication policies need not duplicate protocol validation. The reset clears pending traffic and recovery state.

**Reproduction:** send a checksum-valid active reset Logon with SenderCompID `WRONG`. The reviewed engine reset both sequence numbers to **2**.

**Recommended change:** validate the complete reset request before clearing queues or touching the store. Enforce reset sequence requirements and retain negotiated session constraints.

**Acceptance criteria:**

- Wrong identities, invalid timestamps, invalid reset sequence numbers, and invalid required Logon fields cannot trigger a reset.
- Refused resets preserve the applicable pending traffic, generation, and recovery state.
- Valid active resets continue to work across supported versions and compatibility modes.

### F05 FIXT application profiles are selected after body parsing

**Fixed in:** [`e5ea15b`](https://github.com/sylr/fix2/commit/e5ea15b6891e3be0e564d73ec0f8296c4497c634), [`17f269d`](https://github.com/sylr/fix2/commit/17f269d81cce08c661fd9ef4d9abf41556bc5658) (ApplVerID from the header without dictionaries).

**Priority:** high for integrations using application dictionaries with differing DATA pairs.

**Sources:** [session/protocol.go:36](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/protocol.go#L36), [session/protocol.go:50](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/protocol.go#L50).

The engine parses the entire message using its default application profile before `ApplVerID` selects an alternate profile. Alternate dictionaries with additional LENGTH/DATA pairs cannot reliably carry embedded SOH bytes.

**Reproduction:** use public `Config`, a FIXT transport dictionary, a default application dictionary, and an alternate dictionary containing LENGTH/DATA tags `9000/9001`. Send an alternate-version message with payload `x\x01y`. It parses successfully under its declared profile, but the session rejects it and consumes its sequence number. The test observed `RejectsSent=1` and `MessagesReceived=0`.

**Recommended change:** select the application profile from the standard header before parsing the body. When tag `1128` is absent, use the negotiated peer default.

**Acceptance criteria:**

- A valid alternate-version message with embedded SOH reaches the application without a parse rejection.
- Explicit `ApplVerID` and negotiated default selection both work.
- Unknown versions and malformed headers are handled according to the selected compatibility policy.
- Profile selection respects field boundaries rather than matching bytes inside DATA payloads.

### F06 Malformed message logs expose credentials

**Fixed in:** [`9b5be1f`](https://github.com/sylr/fix2/commit/9b5be1f54b92703475de582ec0e2498b4641098c), [`17f269d`](https://github.com/sylr/fix2/commit/17f269d81cce08c661fd9ef4d9abf41556bc5658) (values withheld where errors are built), [`a01bf77`](https://github.com/sylr/fix2/commit/a01bf77a90a81884cb291da301ade071261b48b0) (no wire numbers in errors), [`f551679`](https://github.com/sylr/fix2/commit/f551679d605d2614fdf7c18fb9d407264fa6ff3a) (received numbers and acceptor refusals), [`b766b7b`](https://github.com/sylr/fix2/commit/b766b7b161b109c55056c2ea5c9cf9665b2aa634) (refusal attributes as handlers print them).

**Priority:** high when session logging is enabled.

**Source:** [session/protocol.go:155](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/protocol.go#L155).

Parse failures log the first 256 raw bytes at WARN, bypassing the redacting observer.

**Reproduction:** configure an acceptor with a `Logger` and `SlogObserver`, log on normally, then send a framed message containing `554=PASSWORD-SHOULD-BE-REDACTED` and invalid checksum `10=999`. The password appears in the `garbled message ignored` log. DEBUG tracing need not be enabled.

**Recommended change:** omit raw payloads for unparsable messages or apply redaction that fails closed. Audit diagnostic error strings alongside payload logging.

**Acceptance criteria:**

- Malformed messages cannot expose sensitive field values through payload or error logging.
- Tests cover invalid checksums, malformed fields, and sensitive DATA values.
- Diagnostics retain useful error classification and counts without exposing credentials.

### F07 Blocked output causes unsafe contrib shutdown

**Fixed in:** [`eca7e79`](https://github.com/sylr/fix2/commit/eca7e79f49053c5e4f93666a49d368b09caf333a), [`87d1be5`](https://github.com/sylr/fix2/commit/87d1be5023d0a8307c9d51a7c2799f72c706c0b4) (stop on a failed output write), [`4384988`](https://github.com/sylr/fix2/commit/4384988f58c97942c2eabc291de71b770f90932d) (a failed write told once).

**Priority:** high for unattended market-data pipelines.

**Sources:** [contribs/coinbasemd/client.go:167](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/contribs/coinbasemd/client.go#L167), [contribs/binancemd/client.go:136](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/contribs/binancemd/client.go#L136).

Application callbacks hold the client mutex while writing and flushing output. If stdout stalls, a Logout response can block `FromAdmin` behind that mutex and prevent shutdown. If the peer closes instead, `Session.Stop` can return after its deadline while the callback remains blocked. `streamWith` then calls `out.end()` and flushes the same `bufio.Writer` concurrently. Output can be duplicated or corrupted.

**Reproduction:** block the output writer during a Coinbase snapshot callback, cancel the stream, and have the peer close after the client's Logout. Shutdown starts a second buffer flush while the callback's flush is still blocked. Release the writer: the race detector reports concurrent access from `out.end()` and `FromApp`. Coinbase was reproduced; Binance has the same implementation pattern.

**Recommended change:** give output a single owner with explicit cancellation and bounded shutdown. Keep blocking writes outside the client-state mutex. Final flushing must not overlap an unfinished callback.

**Acceptance criteria:**

- Blocked output cannot prevent administrative callbacks from making progress.
- Shutdown has defined behavior when output remains blocked.
- Finalization and application callbacks never access the output buffer concurrently.
- Both contrib modules have race-tested regressions for peer Logout and peer-close paths.

## Additional correctness and robustness findings

### F08 Held messages are dropped by a reset past their sequence numbers

**Fixed in:** [`bca5279`](https://github.com/sylr/fix2/commit/bca5279fb63c8e9fbaf158475acfa918c209e626), [`2735d20`](https://github.com/sylr/fix2/commit/2735d20690d04dc03fe22b19b1f83c576810ab0f) (loss reported before the record).

**Source:** [session/protocol.go:906](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/protocol.go#L906).

Enabling `TestIssue407_HeldMessagesDeliveredAfterResetPastThem` fails. A snapshot and update already received and held during gap recovery never reach `FromApp` after the peer resets past them. `OnRecoveryLoss` reports the loss.

This is a disclosed compatibility limitation. Decide and enforce the intended delivery behavior, or require explicit resnapshot or reconciliation by the application. Acceptance requires a test of the selected behavior and a defined application response to recovery loss.

### F09 ResetOnLogon does not always announce the reset

**Fixed in:** [`ac80a45`](https://github.com/sylr/fix2/commit/ac80a452ade44d53c518e83ca6c8d3385f020bff), [`17f269d`](https://github.com/sylr/fix2/commit/17f269d81cce08c661fd9ef4d9abf41556bc5658) (announced to a counterparty that moved on).

**Source:** [session/logon.go:228](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/session/logon.go#L228).

An acceptor resetting because of `ResetOnLogon` omits tag `141` unless the initiator requested a reset. `TestIssue762_AcceptorResetOnLogonAnswersResetSeqNumFlag` fails.

This is a documented QuickFIX compatibility choice, but counterparties can retain inconsistent sequence expectations. Make the behavior explicit and test it against supported counterparties. Acceptance requires an intentional compatibility policy and coverage of both peer-requested and locally configured resets.

F08 and F09 are skipped by default and disclosed in the README. A green default suite does not establish suitability for a particular integration. Preserve compatibility intentionally rather than automatically treating the tests' desired behavior as the only valid policy.

### F10 Group validation misidentifies a following top-level field

**Fixed in:** [`aa175f1`](https://github.com/sylr/fix2/commit/aa175f1287c3f368adb5a533e85e525a347661cc), [`3fb9fb9`](https://github.com/sylr/fix2/commit/3fb9fb949b85d0c66dbdd7e1f2f3b511d151ea06) (nested group delimiters).

**Source:** [dictionary/checks.go:269](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/dictionary/checks.go#L269).

When a field after a group shares the group delimiter tag, the validator counts past the declared group count and rejects a valid message. In the reproduction, `NoG=1` was followed by one entry and then a top-level delimiter value `TOP`. `CheckAll` reported two entries, while `GroupsOf` returned one entry and `GetTopLevel` recovered `TOP`.

Align validation boundaries with the count-aware reader. Acceptance requires agreement between validation and extraction for shared delimiter tags, nested groups, zero counts, and actual count mismatches.

### F11 Malformed frame recovery repeatedly rescans input

**Fixed in:** [`cbe2299`](https://github.com/sylr/fix2/commit/cbe2299184fed6c8faea623e96afcd5a9a89f740), [`fd7dd77`](https://github.com/sylr/fix2/commit/fd7dd77a8233e914fbbdb1426615829f8d40f2d1) (grow rather than copy).

**Source:** [framer.go:285](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/framer.go#L285).

`skipToCheckSum` searches buffered input repeatedly as new bytes arrive. With one-byte reads, a malformed frame at 40 KB took approximately **10 ms**, while 320 KB took approximately **540 ms**. The measured growth is quadratic. Existing message-size and connection limits bound exposure; this is not a claim of unbounded allocation or unrestricted pre-authentication denial of service.

Maintain a search cursor and scan only newly received bytes. Acceptance requires bounded work as input grows, correct handling of a checksum marker split across reads, and preservation of resynchronization behavior. Use a meaningful complexity check or benchmark rather than a fragile machine-specific timing threshold.

### F12 Generated DATA setters can omit their required LENGTH field

**Fixed in:** [`bf927df`](https://github.com/sylr/fix2/commit/bf927df7dbf798c36fbcd3b870a237848a6b5627), [`9917ed0`](https://github.com/sylr/fix2/commit/9917ed002ad919687d46fa9e93547d8f367843dd) (LENGTH names as FIX gives them).

**Source:** [cmd/fixgen/generate.go:873](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/cmd/fixgen/generate.go#L873).

A dictionary defining RawData `96` without LENGTH `95` is accepted. Its generated `SetRawData` setter emits only tag `96`, although the generated dictionary profile requires tag `95` immediately before it. The generated output fails parsing with `DATA tag 96 without its LENGTH field right before it`.

Reject the incomplete dictionary or generate the required paired setter. Acceptance requires either a clear generation-time error or generated output that round-trips under its own profile, including embedded SOH values and group-entry setters.

## Production deployment requirements

These are deployment requirements or documented limitations, not all unintended implementation defects.

### Durable restart recovery

The repository supplies only an in-memory store. Its documented behavior is to start again at sequence 1 when the process restarts. This can suit feeds that explicitly reset on reconnect. Order sessions requiring restart recovery need a persistent implementation and crash-recovery testing. Resolve F01–F03 before relying on such an implementation.

### Retention and memory budgets

The default store retains outgoing messages without automatic truncation. Gap stashes and replay-held queues are limited mainly by message count. With the default 10,000-message stash and roughly 1 MiB frame limit, potential payload memory approaches **10 GiB per session**. This is a capacity estimate, not a measured allocation during the review.

Establish retention policies and byte budgets for every queue and recovery buffer. Verify sustained traffic and gap recovery stay within the intended process memory budget.

### Repeating-group validation

Required entry fields, duplicate entry fields, entry ordering, and group-only fields without a counter are not fully enforced. Each scenario passed `CheckAll` in the reproduction. This is an intentional limitation: [dictionary/xmldict/xmldict.go:526](https://github.com/sylr/fix2/blob/f85603b7be0613eec3d57ac8b161d49f55b58aac/dictionary/xmldict/xmldict.go#L526) explicitly identifies group instance validation as out of scope.

Applications must supply the missing checks before acting on messages, or the library must implement them before claiming complete dictionary validation.

### Authentication and audit storage

`BaseHandler` deliberately accepts every Logon. Production deployments need an explicit authentication policy, TLS configuration, and client-certificate policy where applicable. TLS client verification, handshake bounds, pre-logon message limits, and pending-connection caps are implemented.

Async observers deliberately drop frames when their bounded queue is full. They should not serve as lossless audit storage. Monitor drops or provide an audit path with the required delivery guarantees.

### Consistent credential redaction

Coinbase API keys appear in INFO session attributes and trace fields `49`, `56`, and `553`, despite the CLI promising credential redaction. The key alone cannot authenticate, and password and signature fields are redacted, but the API key should still be masked consistently. Masking `553` alone does not remove the key from `SessionID.String()`.

### Monitoring and deployment testing

Establish alerts for persistence failures, recovery loss, stalled handlers, queue pressure, and dropped observations. Exercise process termination, disk failures, slow consumers, blocked output, and sustained traffic against the actual deployment configuration. Define the application's `OnRecoveryLoss` response and deduplication behavior.

## Verification completed during the review

| Check | Result |
| --- | --- |
| Root `go test -race ./...`, including TCP and mutual TLS | Passed |
| Root SIMD race suite on arm64 | Passed |
| Root and both contrib modules `go vet` | Passed |
| Both contrib modules' existing race suites | Passed |
| All 505 QuickFIX conformance scripts | Passed with bounded parallelism |
| Known-issue suite enabled | Two failures: issues 407 and 762 |
| Five isolated session regression tests | All reproduced F01–F05 |
| Isolated blocked-output regression | Race detector confirmed F07 |
| Parser and acceptor fuzz smoke tests | Passed, 10 seconds each |
| Root `govulncheck` | No findings reported |
| Benchmark Go module after codec generation | Race tests and vet passed |
| Staticcheck | Could not complete: incompatible with Go 1.27 export data |

### Coverage

| Package | Statement coverage |
| --- | ---: |
| Parser root package | 96.5% |
| Dictionary | 97.6% |
| Code generator | 94.7% |
| Session engine | 85.2% |

These measurements support the breadth of the existing tests. The reproductions demonstrate important semantic gaps despite that coverage.

### Conformance timing and verification limits

The first conformance run had one heartbeat timing failure in `server/fix40/4a_NoDataSentDuringHeartBtInt`. That case passed three targeted repetitions, and all 505 scripts passed when the complete suite was rerun with bounded parallelism. Track this as test reliability evidence rather than a confirmed protocol defect.

Linux and amd64 execution, long-duration soak testing, and live exchange interoperability were not independently exercised. Fuzzing was a short smoke test, not exhaustive. `govulncheck` reported no findings for the root module on Go 1.27.2 using scanner v1.9.0 and a vulnerability database last modified on 8 October 2026. Separate contrib dependency graphs were not scanned.

Staticcheck could not complete because its supported export-data format was incompatible with Go 1.27. This was a tooling limitation, not a passing analysis result.

## Commands for a remediation agent

Run these from the repository root, using the current project's toolchain. Root `./...` excludes the nested contrib and benchmark modules.

```sh
go test -race ./...
go vet ./...
GOEXPERIMENT=simd go test -race ./...
make test-contribs RACE=1
```

Enable the disclosed compatibility regressions explicitly:

```sh
FIX2_QUICKFIX_ISSUES=1 go test -race ./tests/quickfix-go
```

F08 and F09 fail at the reviewed revision. Their outcomes must be interpreted against the selected compatibility policy.

For conformance, set the path to a quickfix-go checkout containing `_test/definitions` and the sibling `spec` directory:

```sh
FIX2_QUICKFIX_TESTS=/path/to/quickfix-go/_test \
  go test -race -parallel=4 -run '^TestConformance$' ./session
```

For each contrib module, run `go test -race ./...` and `go vet ./...` from its directory. The benchmark module requires `make generate` before its Go checks. Do not mistake missing generated packages before that prerequisite for a library defect.

Use targeted fault-injection tests for persistence and reset paths. A green broad suite without those tests does not resolve F01–F03.

## Local reproduction evidence

These paths were created in the review environment and may be absent on another machine. Recreate the described scenarios if they are unavailable. Preserve useful reproductions as repository regression tests during remediation.

| Evidence | Location |
| --- | --- |
| Five session defect tests | `/private/tmp/fix2-session-review.fc8Gvt/session/production_review_test.go` |
| Session defect output | `/private/tmp/fix2-review-session-defects.log` |
| Coinbase shutdown race test | `/private/tmp/fix2-review-coinbasemd/review_test.go`, `TestReviewShutdownOutputRace` |
| Shutdown race report | `/private/tmp/fix2-review-contrib-shutdown.log` |
| Dictionary validation reproduction | `/private/tmp/fix2-parser-audit.go` |
| Fragmented malformed-frame reproduction | `/private/tmp/fix2-framer-audit.go` |
| Generated DATA setter reproduction | `/private/tmp/fix2genaudit/` |
| Credential logging reproduction | `/private/tmp/fix2-security-repro/main.go` |
| Full root race test results | `/private/tmp/fix2-review-full-tests.json` |
| Conformance results with bounded parallelism | `/private/tmp/fix2-review-conformance-bounded.json` |
| Enabled known-issue results | `/private/tmp/fix2-review-known-issues.json` |
| Root vulnerability scan | `/private/tmp/fix2-review-vulncheck.json` |

To run the isolated session tests:

```sh
cd /private/tmp/fix2-session-review.fc8Gvt
go test ./session -run '^TestReview' -count=1 -v
```

To run the isolated Coinbase race test:

```sh
cd /private/tmp/fix2-review-coinbasemd
go test -race -count=1 -run '^TestReviewShutdownOutputRace$' -v .
```

## Conditions for production approval

- F01–F07 are resolved with regression evidence, or their affected use cases are explicitly excluded from the supported production contract.
- F08–F12 and the repeating-group validation limitation have an intentional, tested disposition for supported dictionaries and counterparties.
- Durable restart recovery is tested wherever persistence is required.
- Retention and queue byte budgets are validated under sustained traffic and gap recovery.
- Authentication, credential redaction, audit guarantees, recovery-loss handling, and duplicate-message handling are defined for the deployment.
- Relevant race tests, module tests, conformance checks, and supported static analysis pass; any remaining limitations are reported explicitly.
- The readiness claim names the supported dictionaries, counterparties, store implementation, platforms, and recovery guarantees.

The follow-up report should identify the final revision, changes for each finding, verification results, accepted compatibility constraints, and remaining production risks.
