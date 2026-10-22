# fix2 production readiness review — eighth audit

## Decision

**One new P2 and two new P3 defects are confirmed. All four findings from audit seven remain open. No P0/P1 defect was confirmed.**

The P2 breaks FIXP's documented per-session callback serialization: reconnecting or retiring a session can invoke callbacks while its previous `OnUnbound` still runs. Fix this before approving a FIXP server deployment whose handlers rely on that contract. The other new findings concern generated SBE decoders retaining historical input buffers and in-memory connections ignoring expired deadlines on ready I/O.

Normal and SIMD race suites on macOS, the normal Linux arm64 race suite, contrib/Jepsen Go tests, all 505 external QuickFIX conformance scripts and vet pass. Those results do not cover the newly reproduced cases: the saved diagnostics intentionally fail outside the permanent suite. Staticcheck still reports 21 diagnostics. The Jepsen provisioning scan has affected-function traces, and the benchmark module has additional dependency observations without an affected-function trace. This is not an unconditional production sign-off.

Only this report and audit evidence were added. Production sources, dependency pins and permanent tests are unchanged. Diagnostics are saved as `.go.txt` files and executed in disposable archives.

## Scope and evidence boundary

- **Date:** 22 October 2026.
- **Revision:** `c7e5579227c8e7efc2957ebde0aaf1ce76ba241d`.
- **Baseline:** [audit seven](production-readiness-review-7.md), whose audited source revision was `8e9a7f4726de04cf59afb615a9758ac54d1e071c`. The report commit itself is current HEAD; source changes since the audited revision are included here.
- **Environment:** Go 1.27.3 on macOS arm64; additional root race verification in a Linux arm64 Go 1.27.3 container. All nine Go modules target Go 1.27.
- **Scope:** classic FIX sessions, new SecureData encryption, receipts/replay/retention and SQLite; FIXP lifecycle, authentication, recovery and finishing; framing, parsers, dictionaries and generators; Binance/Coinbase/FIXP clients; operational documentation, CI, Jepsen checker tests and all nine Go dependency graphs.
- **Method:** three bounded independent review lanes, leader inspection and independent execution of every correctness finding, cross-review of the P2 and SBE retention findings, uncached race suites, explicit conformance, generation comparison, vet, Staticcheck, short fuzz campaigns and current vulnerability scans.

This was a focused risk audit, not a line-by-line review of every generated source or a replay of every historical diagnostic. A passing test suite is evidence for its cases, not proof of capacity, cryptographic security or storage durability under every failure.

## New findings

### A8-01 — P2: FIXP re-establishment and retirement overlap the previous connection's callbacks

**Confidence: high; deterministic authenticated lifecycle reproductions, independent race reruns and independent source cross-review.**

**Sources:** [callback contract](../../fixp/session/handler.go#L28), [early publication of unbound state](../../fixp/session/session.go#L595), [re-establishment admission](../../fixp/session/server.go#L605), [retirement's connection snapshot](../../fixp/session/server.go#L226), [conditional callback join](../../fixp/session/server.go#L251), [retirement notification](../../fixp/session/server.go#L273).

`Session.run` clears `s.conn` and publishes `StateUnbound` before calling `OnUnbound`. It closes the old connection's `unbound` channel only after that callback returns. A new authenticated connection can claim the published state, receive `EstablishmentAck`, and start its reader without waiting for the old callback. An external `Server.Retire` also sees a nil connection and bypasses the existing `c.unbound` join before invoking `OnFinalized`.

The reconnect diagnostic uses the normal server and an in-memory listener: negotiate and establish as `alice`, close the first transport, gate its `OnUnbound`, then authenticate and establish a second connection with the same session ID and send an application message. Both `OnEstablished` and the actual `OnMessage` execute while the first callback is active. A separate diagnostic calls public `Retire` during the same interval and observes overlapping `OnFinalized`. Each reproduces three times under race in both the reviewer's and leader's executions. Releasing `OnUnbound` before reconnecting or retiring makes every control pass. Atomics detect the overlap without deliberately introducing a user-memory data race.

**Impact and limits:** the API promises one callback at a time per session, and [the application guide](../fixp-app.md#L131) recommends reusable per-session decoders on that basis. Compliant handlers can therefore race cleanup against new initialization or delivery, including closing resources a new connection just installed. Ordinary clients can reconnect after their own callback returns while the server's callback remains active; a malicious or unauthenticated peer is not required. The gates widen a scheduling window and do not establish that a long-blocked callback is required. No actual application corruption, duplicate business execution or durable loss was demonstrated. The library's internal locks do not enforce the promised application callback serialization.

**Correction and acceptance:** keep unbinding callback completion visible to re-establishment and retirement, or serialize callback delivery across connection generations. Cover reconnect, application delivery and external retirement while `OnUnbound` is active, then successful progress after it returns. Preserve shutdown and callback reentry behavior; holding the existing session mutex around handlers would introduce deadlocks. The saved diagnostics expose current behavior; permanent regression tests should explicitly release the callback after verifying that subsequent callbacks cannot start early.

**Evidence:** [reconnect diagnostic](audit-evidence/production-readiness-8/fixp-codecs/rebind-callback_test.go.txt), [reconnect output](audit-evidence/production-readiness-8/fixp-codecs/rebind-callback-race.log), [retirement diagnostic](audit-evidence/production-readiness-8/fixp-codecs/retire-callback_test.go.txt), [retirement output](audit-evidence/production-readiness-8/fixp-codecs/retire-callback-race.log), [independent execution](audit-evidence/production-readiness-8/callback-independent.log), [source cross-review](audit-evidence/production-readiness-8/fix-session/fixp-lifecycle-cross-review.md).

### A8-02 — P3: reused generated SBE groups retain references to historical input buffers

**Confidence: high; structural reproduction, bounded heap measurement, version control and independent race execution.**

**Sources:** [zero-entry reslice](../../internal/gen/sbe/gen.go#L1097), [positive-count reslice](../../internal/gen/sbe/gen.go#L1113), [version-absent group](../../internal/gen/sbe/gen.go#L1302).

Generated decoders shorten group slices without clearing removed entries. Those inactive entries can still contain borrowed `[]byte` fields and nested slices, keeping previous input allocations alive even when the public group is empty. This is additional retention beyond keeping a reusable entry array at its high-water capacity.

The diagnostic reuses the unchanged generated `rich.Order` decoder over 513 valid frames, each below 80 KB and 1,024 total group entries, shrinking `Legs` from 512 entries to zero. Each removed entry keeps a reference to a different historical input buffer. After GC, the empty group retains approximately 39.54 MB versus 144–166 KB in the leader's constant-count controls; all 512 stale input slices remain observable in its backing array. Three independent race repetitions reproduce the structural and heap assertions. A second diagnostic decodes a newer message with nested `Fees`, then an older version lacking that group: the public group becomes empty, but old memo and nested tax slices remain. Zeroing the destination before decoding is a passing workaround control.

**Impact and limits:** applications reusing generated message structs across separately allocated input frames can retain substantially more memory than the current message requires. For fixed retained capacities this has a ceiling; unbounded growth and an OOM were not demonstrated. A SOFH reader normally reuses its own buffer, so the measured allocation pattern is not universal. Binance has an affected generated group containing variable data, but its current handlers instantiate fresh decoders; no current exchange-client incident or FIXP session-codec exposure was established. Per-message entry limits do not clear historical references.

**Correction and acceptance:** release references in removed group entries on positive shrink, zero count and version absence, including nested groups, while preserving intended capacity reuse and ownership of active fields. Regenerate affected codecs. Assert removed references are cleared, decoder reuse remains correct and allocations remain bounded; use structural assertions as the primary regression rather than a brittle exact heap threshold.

**Evidence:** [diagnostic](audit-evidence/production-readiness-8/fixp-codecs/group-retention_test.go.txt), [reviewer race output](audit-evidence/production-readiness-8/fixp-codecs/group-retention-race.log), [independent heap reproduction](audit-evidence/production-readiness-8/sbe-retention-independent.log), [version diagnostic](audit-evidence/production-readiness-8/fixp-codecs/group-version-retention_test.go.txt), [independent version output](audit-evidence/production-readiness-8/deadline-version-independent.log), [cross-review and scope limits](audit-evidence/production-readiness-8/fix-session/sbe-retention-cross-review.md).

### A8-03 — P3: in-memory connections ignore expired deadlines when I/O is ready

**Confidence: high; deterministic expired deadlines, independent race execution and standard-library controls.**

**Sources:** [buffered read](../../transport/mem/mem.go#L100), [write with available room](../../transport/mem/mem.go#L133), [deadline checking only while waiting](../../transport/mem/mem.go#L71).

`mem.Conn` checks deadlines only when a read has no data or a write has no room. With input buffered, a read after `SetReadDeadline(time.Unix(1, 0))` still returns a byte and nil; with output capacity available, the equivalent expired write returns 1 and nil. Empty-read and full-write controls correctly return `os.ErrDeadlineExceeded`. This depends on buffer state, not a close race or timer precision.

The diagnostic reproduces both errors in three race repetitions, then independently repeats them. The strengthened version also asserts that the concrete connection implements `net.Conn` and verifies that standard `net.Pipe` rejects both operations with the same expired deadlines. Its controls pass. The local Go documentation says deadlines apply to future and pending I/O; the package advertises TCP-like buffered connections with deadlines.

**Impact and limits:** users of the public in-process transport cannot rely on an expired deadline as an I/O boundary while data or capacity remains available. A continuously ready stream can continue past it. This also weakens deadline testing performed with that transport. No TCP/TLS timeout failure, normal session outage or persistent loss was demonstrated.

**Correction and acceptance:** check current deadlines before consuming or appending bytes, preserving deadline-change wakeups, partial-write accounting and close behavior. Cover ready and blocked I/O, expired/future/cleared deadlines and changes during a blocked operation.

**Evidence:** [diagnostic and standard controls](audit-evidence/production-readiness-8/fix-session/mem-expired-deadline_test.go.txt), [three repetitions](audit-evidence/production-readiness-8/fix-session/mem-expired-deadline.log), [independent strengthened execution](audit-evidence/production-readiness-8/deadline-version-independent.log).

## Findings retained from audit seven

These are existing findings, not newly counted defects. All were rechecked at this revision.

| Finding | Current disposition and evidence |
|---|---|
| **A7-01 — P3: queued replay follows final Logout** | Still open. Promotion at [replay.go:298](../../fix/session/replay.go#L298), retained administrative output at [conn.go:722](../../fix/session/conn.go#L722) and pending-before-replay writing at [conn.go:389](../../fix/session/conn.go#L389) still yield `order; replay; Logout; replay; GapFill`. Three race repetitions and the leader's peer-stream execution fail only the defect case; both controls pass. The final-close deadline remains bounded. [Diagnostic](audit-evidence/production-readiness-8/fix-session/queued-replay-logout_test.go.txt), [reviewer output](audit-evidence/production-readiness-8/fix-session/queued-replay-logout.log), [independent output](audit-evidence/production-readiness-8/replay-independent.log). |
| **A7-02 — P3: concurrent TCP Accept cannot cancel while awaiting serialization** | Still open. [tcp.go:121](../../transport/tcp/tcp.go#L121) acquires a plain mutex before registering cancellation. Three race repetitions and the independent execution still show the expired caller waiting until the first Accept releases it; the sole-Accept control passes. Normal engine acceptors use a single accept loop. [Diagnostic](audit-evidence/production-readiness-8/fix-session/tcp-concurrent-accept_test.go.txt), [reviewer output](audit-evidence/production-readiness-8/fix-session/tcp-concurrent-accept.log), [independent output](audit-evidence/production-readiness-8/transport-independent.log). |
| **A7-03 — P3: unsupported SBE timestamps silently omit trade JSON** | Still open. [output.go:221](../../contribs/binancemd/output.go#L221) discards encoding errors after [sbe.go:339](../../contribs/binancemd/sbe.go#L339) converts an admitted int64 timestamp to `time.Time`. Three race repetitions and the independent fake-exchange execution emit trade IDs 1 and 3, omit invalid trade 2 and return nil; ordinary/year-9999 controls pass. No ordinary timestamp loss or live venue incident is established. [Diagnostic](audit-evidence/production-readiness-8/clients/sbe-trade-timestamp_test.go.txt), [reviewer output](audit-evidence/production-readiness-8/clients/timestamp-recheck.log), [independent output](audit-evidence/production-readiness-8/timestamp-independent.log). |
| **A7-04 — P3: affected Jepsen provisioning dependency** | Still open. [jepsen/infra/go.mod:110](../../jepsen/infra/go.mod#L110) still pins indirect `golang.org/x/net v0.59.0`. A fresh symbol-level scan again reports five HTTP/2 advisories, fixed in `v0.60.0`, plus one module-only OpenPGP advisory in `x/crypto`. Root, five contrib modules and Jepsen node report no findings. The benchmark module has separate observations described below. [Current advisory records and complete finding traces](audit-evidence/production-readiness-8/jepsen_infra-vuln-findings.json). |

The five infrastructure advisories concern [memory exhaustion](https://pkg.go.dev/vuln/GO-2026-6603), [response smuggling](https://pkg.go.dev/vuln/GO-2026-6610), [CPU exhaustion](https://pkg.go.dev/vuln/GO-2026-6611), [flow-control bypass](https://pkg.go.dev/vuln/GO-2026-6612) and [an encoder race/crash](https://pkg.go.dev/vuln/GO-2026-6617). Current official records still identify `v0.60.0` as fixed. The scan contains 108 finding records: six module observations, five package observations and 97 symbol traces, representing six distinct advisory IDs. These are not 108 vulnerabilities. OpenPGP has no affected-package or function trace. JSON-mode exit zero is not proof of a clean scan.

Concrete exploitability in the Pulumi provisioning workflows remains unverified. No project-owned public HTTP/2 server or reverse proxy was identified; generic formatting paths in some static traces do not establish an attack. This dependency finding does not establish a production FIX/FIXP vulnerability. Upgrade to a compatible fixed dependency and rerun infrastructure checks and the scan.

## Additional dependency observation

The benchmark module pins indirect [`golang.org/x/net v0.24.0`](../../fix/tests/benchmarks/go.mod#L19). Its completed scan, after generating required codecs, records **18 distinct advisory IDs: 18 module observations and one affected-package observation, with no affected-function trace**. The package observation is [GO-2025-3503, proxy bypass involving IPv6 zone IDs](https://pkg.go.dev/vuln/GO-2025-3503), fixed in `v0.36.0`; the full observed advisory set includes later fixes through `v0.60.0`. [Current advisory records and findings](audit-evidence/production-readiness-8/benchmark-vuln-findings.json).

This is dependency-maintenance evidence for separate benchmark tooling, not a confirmed reachable vulnerability in its binaries or the production library. No exploitability was established, and it is not counted as another correctness finding. Update to a compatible release covering the observed advisories and repeat generation, Go build/vet and scanning. Neither scanner exit zero nor the absence of a symbol trace makes the dependency observations disappear.

## Remediation and additional review

- Prior direct/crossing/held Logout gates, final-close deadlines, retention accounting and TCP cancellation-callback joining remain present. Focused session/remediation/encryption cases pass three race repetitions; focused SQLite replay/checkpoint/reset/flush/close tests pass. The separate queued-replay issue remains as A7-01.
- New SecureData paths were reviewed for negotiation, clear/inner-header agreement, binary DATA, signatures, malformed bodies, replay, resets, partial-write receipt mapping and size/error handling. No additional concrete encryption defect was confirmed. Plaintext stores/observers and reliance on application-provided authenticated encryption and transport security are documented deployment constraints.
- Binance's Stop-from-OnLogon repair passes 500 connections under race; focused gap, reconnect, trace redaction, blocked-output and shutdown controls pass. No additional client defect was confirmed beyond A7-03. [Client notes](audit-evidence/production-readiness-8/clients/review-notes.md).
- FIXP owner authentication, bounds, recovery, finishing and crash-model tests were inspected alongside lifecycle. A8-01 is the new lifecycle finding; no new durable recovery defect was confirmed. [FIXP/codec notes](audit-evidence/production-readiness-8/fixp-codecs/review-notes.md), [session/storage notes](audit-evidence/production-readiness-8/fix-session/review-notes.md).
- Operational documentation describes authentication hooks, retention sizing, callback cooperation, secret-bearing stores and reconciliation after storage errors. Those remain deployment requirements. This audit ran checker unit tests, not a new Jepsen fault campaign.

## Verification

The [machine-readable summary](audit-evidence/production-readiness-8/verification-summary.json) records outcomes, skips, scanner metadata, generation comparisons and raw-evidence hashes. Full test JSON, coverage and scan streams are in `/private/tmp/fix2-production-readiness-8`; compact evidence is saved beside this report. Test-event counts include subtests and fuzz seeds, whose cached corpus and platform can differ between runs.

| Check | Result |
|---|---|
| macOS root race suite, uncached, with coverage | Passed: 4,035 test/subtest events; no reported race. Aggregate statement coverage 58.1%, including generated packages. |
| macOS root SIMD race suite, uncached | Passed: 4,037 test/subtest events; no reported race. |
| Linux arm64 root race suite, uncached | Passed on retry: 4,028 test/subtest events; no reported race. Go 1.27.3. |
| All contrib and Jepsen Go modules, race | Passed: 794 test/subtest events across 14 tested packages. |
| `make vet` | Passed root and all seven contrib/Jepsen modules. |
| External QuickFIX conformance, race | All 505 scripts passed. Fixture revision `236c791cfcbca8d57a36f92927f9e8265349359b`; checkout clean. |
| Race-skipped allocation checks | Both passed separately without race. |
| FIXP keepalive repetition | Passed 20/20 under race. Audit seven's broad-run timing failure did not recur; this does not prove its underlying timing sensitivity is removed. |
| Root and three generating contrib modules | Regenerated in an isolated archive: all 700 tracked Go files compare byte-for-byte with HEAD. |
| Dependency tidiness | Root and seven contrib/Jepsen modules tidied in archive; no tracked module pin/checksum changed. |
| Benchmark Go module | Required fix2/QuickFIX codecs generated; Go build checks and vet pass; its test-bearing package has no tests to run. Scan completed with the dependency observations above. No Java/C++ build or benchmark campaign. |
| Jepsen checker unit tests, offline Linux container | 20 tests, 154 assertions; zero failures/errors. |
| 20-second leader fuzz campaigns | Encrypter Open 362,349; FIXP server 1,642; XML dictionary load 272,823; rich SBE decode 3,202,415 executions. All passed. Reviewer Encrypter campaign separately passed 341,540. |
| Govulncheck v1.7.0, all nine modules | Seven modules report no findings; Jepsen infrastructure retains A7-04; benchmark tooling has 18 advisory IDs at module/package level, no symbol trace. Database last modified 21 October 2026, 00:27:29 UTC. |
| Staticcheck via installed Go-compatible golangci-lint | 21 diagnostics: root 18, Binance client 1, FIXP echo 1, Jepsen node 1. Other scanned modules zero. Installed linter reports `(devel)`, built with Go 1.27.1; standalone Staticcheck is older and was not used. |
| Correctness diagnostics outside permanent suite | All reported defect assertions reproduce, with passing controls; independently rerun. No production patch was applied. |

The first Linux attempt used an incomplete offline module volume and failed setup for the CLI/tagvalue generator because three dependencies could not be fetched. The retry mounted the host's complete dependency cache read-only, with container networking still disabled, and passed the full root suite. The initial failure remains in the raw evidence and summary and is not classified as a product failure. Initial local sandbox/cache/socket denials likewise are not correctness results.

Broad root runs skip allocation assertions under race, external QuickFIX unless explicitly enabled, and opt-in Silverflash cases. The allocation and QuickFIX checks were enabled separately; Silverflash was not. Contrib tests skip two live Binance testnet cases. The initial direct benchmark scan could not load its ungenerated codecs; generation in the disposable archive resolved that prerequisite and its subsequent scan completed with the benchmark dependency observations above. Skips and setup failures must not be described as passing tests.

## Reproduction and remaining acceptance work

Extract `git archive c7e5579227c8e7efc2957ebde0aaf1ce76ba241d` into a fresh directory. Copy a saved `.go.txt` diagnostic into its indicated package as `_test.go`, then run the corresponding command. These commands intentionally fail the defect assertions on the audited revision; use a writable Go cache and allow loopback for the TCP and fake-exchange tests.

```sh
# FIXP diagnostics -> fixp/session/
go test -race -count=3 -run '^TestAudit8(Rebind|Retire)OverlapsUnbound$' -v ./fixp/session
# Retention diagnostic -> internal/gen/sbe/tests/rich/
go test -race -count=3 -run '^TestAudit8GroupTailRetention$' -v ./internal/gen/sbe/tests/rich
# Version diagnostic -> internal/gen/sbe/tests/newer/
go test -race -count=3 -run '^TestAudit8AbsentGroupRetention$' -v ./internal/gen/sbe/tests/newer
# Deadline diagnostic -> transport/mem/
go test -race -count=3 -run '^TestAudit8MemExpiredDeadline$' -v ./transport/mem
```

Audit seven's retained diagnostic names and reproduction commands remain valid; the current copies and outputs are linked above. New diagnostics are evidence, not final regression-test designs or fixes.

No live exchange, Silverflash interoperability, Linux amd64/SIMD matrix, long-duration capacity/load/soak, new Jepsen fault campaign, real process/power-loss or device-mapper storage-fault campaign was performed. The Linux race and checker-unit runs do not substitute for those. Production acceptance still requires A8-01 remediation and evidence for the actual handlers, persistence implementation, platform, traffic and failure budget.
