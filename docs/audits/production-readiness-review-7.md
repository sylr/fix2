# fix2 production readiness review — seventh audit

## Decision

**No new P0/P1/P2 defect was confirmed. Three P3 correctness defects remain, plus a P3 infrastructure dependency finding. Audit six's inbound Logout boundary is fixed; a different queued-replay path still permits outbound traffic after final Logout.**

The findings do not establish a general production blocker, but this is not an unconditional sign-off. The normal broad race run failed one FIXP keepalive test under concurrent verification load. The isolated FIXP suite and twenty keepalive repetitions passed; the original failure remains part of the evidence. Staticcheck and the Jepsen infrastructure vulnerability scan are not clean.

Only this report and audit evidence were added. Production sources, dependency pins and permanent tests are unchanged. Deliberately failing diagnostics are saved as `.go.txt` files and were executed in isolated archives.

## Scope and evidence boundary

- **Date:** 20 October 2026.
- **Revision:** `8e9a7f4726de04cf59afb615a9758ac54d1e071c`.
- **Previous audited revision:** `7a6734b70c468a797d01cfaf0366c86c5f9fbe81`.
- **Environment:** macOS arm64, Go 1.27.2; all nine Go modules target Go 1.27. Jepsen checker unit tests additionally ran in an existing Linux control container, from a separate copy of the audited sources.
- **Scope:** FIX session/store/transport lifecycle and prior remediation; new FIXP/SOFH/SBE paths; Binance order and market-data clients; Coinbase and FIXP echo clients; generation, dependencies, CI, operational documentation and Jepsen evidence handling.
- **Method:** three independent bounded source reviews, leader inspection, independent execution of all three correctness reproductions, broad normal/SIMD/contrib race checks, external QuickFIX conformance, generation comparison, vet, Staticcheck, short fuzz campaigns and dependency scans.

The change since audit six includes substantial new functionality and generated code. This was a focused risk review, not a line-by-line inspection of every generated file or a replay of every historical diagnostic. The [sixth report](production-readiness-review-6.md) is the remediation baseline. Package moves place classic FIX sessions under `fix/session` and transport under `transport`.

## Findings

### A7-01 — P3: a queued replay can follow the final Logout

**Confidence: high; deterministic race reproduction, independent peer-stream verification and independent source cross-review.**

**Sources:** [queued-replay promotion](../../fix/session/replay.go#L288), [administrative frames retained in pending](../../fix/session/conn.go#L695), [pending output precedes replay](../../fix/session/conn.go#L373), [documented final ordering](../../fix/session/conn.go#L342).

While a replay runs, our initiated Logout is sequenced and held. A second ResendRequest received during Logout grace can include that Logout's sequence number, for example with EndSeqNo=0. The peer then answers our Logout, starting final closure. When the first replay completes, promotion of the queued replay releases held frames through its end. Pending-range removal retains administrative frames, so the writer sends the Logout before running the second replay.

The reproduction sends one order, gates the first store Range, queues our Logout and the second resend, receives the peer's Logout answer, then releases Range. The peer receives:

```text
order 1; replay order 1; Logout 2; replay order 1; GapFill 2
```

Three race repetitions reproduce this. Capping the second request before Logout, and a peer-initiated Logout with the same infinity-ended request, both keep Logout last. The leader independently reproduced the final diagnostic and a strengthened variant that reads and parses the peer's actual byte stream after closure. The diagnostic uses the actual reader/writer loops over `mem.Pipe`; establishment is arranged through existing internal test helpers, without a Logon handshake.

**Impact and limits:** a peer closing on Logout can miss an already requested resend that follows it. The retransmission carries PossDupFlag=Y; the reproduction does not demonstrate duplicate business execution, durable loss or sequence reuse. The existing close deadline still bounds the replay. This is consistent with audit five's P3 final-output ordering issue, not a recurrence of its indefinite-close P2 or audit six's inbound-drain defect.

**Correction and acceptance:** keep a held final Logout behind every replay accepted before final closure, including a queued range already covering its sequence. Preserve the intended priority of other administrative replies, Logout grace, cancellation and retention/receipt accounting. Keep both controls alongside the regression.

**Evidence:** [diagnostic](audit-evidence/production-readiness-7/fix-session/queued-replay-logout_test.go.txt), [three repetitions](audit-evidence/production-readiness-7/fix-session/queued-replay-logout.log), [independent execution](audit-evidence/production-readiness-7/fix-session/queued-replay-logout-independent.log), [peer-stream diagnostic](audit-evidence/production-readiness-7/fix-session/queued-replay-logout-peer-stream_test.go.txt), [peer-stream output](audit-evidence/production-readiness-7/fix-session/queued-replay-logout-peer-stream.log), [cross-review](audit-evidence/production-readiness-7/fix-session/cross-review.md).

### A7-02 — P3: a concurrent TCP Accept cannot cancel while awaiting serialization

**Confidence: high; reproduced three times under race and independently rerun.**

**Sources:** [mutex acquisition before cancellation registration](../../transport/tcp/tcp.go#L121), [Listener context contract](../../transport/transport.go#L38).

`tcp.Listener.Accept` serializes callers before installing its context cancellation callback. If one caller is already waiting for a connection, a second caller blocks on the mutex even after its own context expires. It returns only when the first Accept releases that mutex.

The reproduction starts a real loopback listener and one long-lived Accept, observes that it owns the guard, then calls Accept with a 20ms context. The second call remains blocked for another 100ms after expiry and returns only after the first caller is canceled. A sole-Accept deadline control passes. The diagnostic observes the real guard with TryLock; it does not hold or replace it.

**Impact and limits:** concurrent users cannot rely on per-call deadlines and may accumulate blocked goroutines. An indefinitely waiting first call can hold them indefinitely. The normal FIX/FIXP acceptors use a single accept loop per listener, so this does not establish an outage in that ordinary path. Audit five's deadline-restoration race remains fixed.

**Correction and acceptance:** make acquisition of the serialization guard context-aware while preserving joined cancellation callbacks and listener deadline restoration. Keep concurrent/sole cancellation, reuse and listener-close controls. Do not restore the old callback race by allowing independent callers to move one shared deadline concurrently.

**Evidence:** [diagnostic](audit-evidence/production-readiness-7/fix-session/tcp-concurrent-accept_test.go.txt), [three repetitions](audit-evidence/production-readiness-7/fix-session/tcp-concurrent-accept.log), [independent execution](audit-evidence/production-readiness-7/fix-session/tcp-concurrent-accept-independent.log).

### A7-03 — P3: SBE trade JSON output silently drops unsupported timestamps

**Confidence: high; end-to-end fake-exchange reproduction repeated under race and independently rerun.**

**Sources:** [discarded JSON encoding error](../../contribs/binancemd/output.go#L221), [SBE timestamp conversion](../../contribs/binancemd/sbe.go#L339), [non-null timestamp admission](../../contribs/binancefix/marketdata.go#L672).

`binancemd --sbe --json trades` converts signed microsecond TransactTime values directly to `time.Time`. Values other than the schema null can produce years outside JSON time's supported range. `jsonOutput.write` assumes events always encode and discards the encoder's error, silently omitting the trade while later output continues.

A fake endpoint supplies ordinary trade 1, trade 2 with `math.MaxInt64` or `math.MinInt64+1` as its timestamp, then ordinary trade 3. Output contains only IDs 1 and 3, and command cancellation returns nil. Three race repetitions and the leader's independent run reproduce both extremes. Ordinary and year-9999 controls emit all three lines. The final trade proves the serialized handler passed the missing event.

**Impact and limits:** erroneous upstream timestamps cause silent data omission in this CLI mode. No live Binance incident, ordinary timestamp loss, order-entry receipt defect or trading loss was demonstrated. The existing panic-oriented SBE client fuzz target does not assert output preservation.

**Correction and acceptance:** reject unsupported event timestamps explicitly or propagate JSON encoding failure through the command's output error path. Keep the integer codec faithful to its schema; JSON's range need not become a library-wide restriction. Test both extremes, boundary controls and command error propagation.

**Evidence:** [diagnostic](audit-evidence/production-readiness-7/clients/sbe-trade-timestamp_test.go.txt), [three repetitions](audit-evidence/production-readiness-7/clients/sbe-trade-timestamp.log), [independent execution](audit-evidence/production-readiness-7/clients/sbe-trade-timestamp-independent.log), [client review](audit-evidence/production-readiness-7/clients/review-notes.md).

### A7-04 — P3: Jepsen provisioning pins an affected HTTP/2 dependency

**Confidence: high for the dependency and scanner traces; concrete exploitability unverified.**

**Source:** [infrastructure dependency pins](../../jepsen/infra/go.mod#L110).

The separate Jepsen provisioning module pins indirect `golang.org/x/net v0.59.0`. Govulncheck reports symbol-level paths through Pulumi/gRPC for five advisories, all listing `v0.60.0` as fixed. The official advisories describe HTTP/2 memory exhaustion ([GO-2026-6603](https://pkg.go.dev/vuln/GO-2026-6603)), response smuggling in a reverse-proxy configuration ([GO-2026-6610](https://pkg.go.dev/vuln/GO-2026-6610)), CPU exhaustion ([GO-2026-6611](https://pkg.go.dev/vuln/GO-2026-6611)), flow-control bypass ([GO-2026-6612](https://pkg.go.dev/vuln/GO-2026-6612)) and an encoder race/crash ([GO-2026-6617](https://pkg.go.dev/vuln/GO-2026-6617)).

The 108 finding records are six distinct advisories: six module observations, five package observations and 97 symbol traces. The sixth advisory concerns unmaintained OpenPGP in `x/crypto`; it is module-only, with no affected-package or function trace. Finding records must not be counted as separate vulnerabilities, and JSON-mode process exit status must not be used as proof of a clean scan.

**Impact and limits:** affected code is statically reachable in provisioning tools, which need dependency maintenance. No exploit was attempted or established. The project-owned infrastructure entry points are Pulumi programs; no project reverse proxy or public HTTP/2 server was found. Some traces traverse generic Error/String formatting and are conservative evidence. These limits particularly matter for server-only and reverse-proxy attack prerequisites. This finding does not establish a FIX/FIXP protocol vulnerability. Root, five contrib modules and the Jepsen node scan report no findings in their scanned graphs.

**Correction and acceptance:** raise the infrastructure's indirect `x/net` requirement to a compatible fixed release, then rerun its build, tests, vet and vulnerability scan. Separately assess removal of the unused OpenPGP dependency path when supported upstream. No dependency update was made during the audit.

**Evidence:** [scan triage](audit-evidence/production-readiness-7/clients/scan-triage.md), [counts](audit-evidence/production-readiness-7/clients/infra-scan-summary.json), [advisory metadata and complete finding traces](audit-evidence/production-readiness-7/infrastructure-vulnerability-evidence.json).

## Prior remediation and new functionality

| Area | Disposition |
|---|---|
| A6-01 direct/crossing Logout drains and malformed input after peer Logout | Fixed. Current peer-Logout gates stop delivery; focused regression checks pass three race repetitions. A7-01 concerns outbound queued replay promotion instead. |
| A5 close deadline, final queue gate and stalled reset Logon | Reported cases pass, including retention after forced close and real SQLite replay cancellation. No unbounded-close recurrence confirmed. |
| TCP cancellation/reuse | Prior callback/deadline restoration fix remains present. A7-02 concerns waiting to acquire its guard. |
| Implicit DATA generation and package moves | Current generation/test checks pass. Root and all four generating contrib modules reproduce tracked Go sources exactly. |
| FIXP/SOFH/SBE | No additional concrete defect confirmed in the focused handshake, ownership, bounds, recovery, finish, decode or crash-model review. Default in-memory persistence and documented delivery semantics remain deployment constraints. |
| Jepsen | Current checker unit tests pass; infrastructure/node Go tests pass. This audit ran no fault campaign and does not treat the harness's existence or historical results as new fault evidence for HEAD. |

[Session/store review and remediation evidence](audit-evidence/production-readiness-7/fix-session/review-notes.md), [focused remediation output](audit-evidence/production-readiness-7/fix-session/remediation.log), [SQLite race output](audit-evidence/production-readiness-7/fix-session/sqlite-race.log), [FIXP review](audit-evidence/production-readiness-7/fixp/review-notes.md).

## Verification

The [machine-readable summary](audit-evidence/production-readiness-7/verification-summary.json) records commands, package outcomes, skips, coverage, scanner metadata and raw-file hashes. Full JSON streams and coverage remain in `/private/tmp/fix2-production-readiness-7-8e9a7f4`; compact evidence accompanies this report.

| Check | Result |
|---|---|
| Root normal race suite, uncached, with coverage | **Failed one test:** `fixp/session.TestKeepalive`; 3,883 passing test/subtest events, one failing event. No reported race. All other package tests passed. |
| Root SIMD race suite, uncached | Passed: 3,882 passing test/subtest events. |
| Isolated FIXP/SOFH/SBE race suite | Passed all 12 tested packages. Isolated keepalive additionally passed 20/20 repetitions. |
| All contrib and Jepsen Go modules, race | Passed: 791 passing test/subtest events across 14 tested packages. |
| `make vet` | Passed root and all seven contrib/Jepsen modules. |
| External QuickFIX conformance, race | All 505 scripts passed (506 events including parent). Fixture revision `236c791cfcbca8d57a36f92927f9e8265349359b`; checkout clean. |
| Non-race allocation assertions | Both checks skipped under race passed separately. |
| Regeneration in isolated archive | Root and four generating contrib modules passed; no tracked Go file differed. |
| Jepsen checker unit tests | 20 tests, 154 assertions; zero failures/errors, from isolated HEAD sources in Linux container. |
| 15-second fuzz campaigns | Builder 220,903; dictionary validation 898,120; acceptor 36,649; SOFH reader 997,512; SBE client 201,105; wide SBE decode 2,692,172 executions. All passed. |
| Govulncheck v1.7.0 | Root, five contrib modules and Jepsen node: no findings. Jepsen infrastructure: A7-04. Database last modified 20 October 2026, 00:20:56 UTC. |
| Staticcheck via installed golangci-lint, built with Go 1.27.1 | Completed with **21 diagnostics**: root 18; Binance client 1; FIXP echo 1; Jepsen node 1. Other modules zero. Installed lint version reports `(devel)`. |
| New correctness diagnostics | Fail as expected on HEAD; controls pass; independently rerun. These failures are outside the permanent suite. |

The normal broad-run keepalive failure was `unexpected event unbound ... mem: write to a closed connection`. Its test config uses 50ms keepalive intervals with the default 1.5 leniency: 75ms of silence can expire it. Concurrent normal/SIMD/contrib/vet work creates scheduling pressure, making timing sensitivity a credible explanation, not a proven root cause. The isolated passes do not erase the original failure. [Original output](audit-evidence/production-readiness-7/root-keepalive-failure.log), [twenty repetitions](audit-evidence/production-readiness-7/fixp/keepalive-repeat.log), [isolated suite](audit-evidence/production-readiness-7/fixp/focused-race.log).

Both broad root runs skip two allocation tests, the separately enabled QuickFIX harness, and six opt-in Silverflash test cases. The contrib run skips two live Binance testnet tests. Packages with no test files are listed separately in the summary; those are not unexecuted network cases. No ordinary TCP/TLS test was skipped for sandbox denial in the permitted runs.

Staticcheck's diagnostics concern style/simplification and test expressions, including identical singleton calls, modulo-one and a repeated echo assertion. They are not represented as additional production defects. Static analysis is nevertheless not a passing check. [Root output](audit-evidence/production-readiness-7/root-staticcheck.log); other module logs are beside it.

Selected local statement coverage: FIX parser 98.5%, dictionary 97.4%, XML loader 99.1%, tag-value generator 96.8%, SBE generator 75.0%, FIX session 93.9%, FIXP session 90.0%, SOFH 98.0%, TCP 93.9%. Coverage is per package, without `-coverpkg`; FIXP's measurement comes from the broad run with the keepalive failure. It does not establish failure-mode completeness or compare directly with differently instrumented earlier audits.

## Reproduction and remaining acceptance work

Copy the saved diagnostic into its package in a fresh `git archive` of the audited revision, then run the corresponding command:

```sh
# A7-01: copy queued-replay-logout_test.go.txt into fix/session/audit7_test.go
go test -race -count=1 -run '^TestAudit7QueuedReplayAfterFinalLogout$' -v ./fix/session

# A7-02: copy tcp-concurrent-accept_test.go.txt into transport/tcp/audit7_test.go
go test -race -count=1 -run '^TestAudit7AcceptCancellationWhileSerialized$' -v ./transport/tcp

# A7-03: copy sbe-trade-timestamp_test.go.txt into contribs/binancemd/audit7_test.go
cd contribs/binancemd
go test -race -count=1 -run '^TestAudit7SBETradeTimestampOutput$' -v .
```

These commands intentionally fail the defect assertions on HEAD. The evidence includes the expected output and passing controls. Use a writable Go cache and permit loopback listeners.

No Linux production engine matrix, standalone benchmark-module verification, long-duration load/soak, live exchange or Silverflash campaign, real process/power-loss campaign, device-mapper disk faults or deployed-environment change was performed in this audit. The Linux Jepsen checker unit run does not substitute for those. Follow the [operations guide](../operations.md), including authentication, callback cooperation, retention sizing, store protection and reconciliation after failed storage writes. Production acceptance still needs evidence for the actual platform, persistence implementation, traffic and failure budget.
