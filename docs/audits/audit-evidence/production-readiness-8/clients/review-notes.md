# Eighth audit: contrib clients

Revision: `c7e5579227c8e7efc2957ebde0aaf1ce76ba241d`.
Scope: `contribs/binancefix`, `contribs/binancemd`, `contribs/coinbasemd`, `contribs/fixpecho`; SQLite excluded.

## Disposition

**A7-03 remains open (P3). No additional concrete production defect was confirmed in this bounded review.**

The saved seventh-audit timestamp reproduction was copied unchanged into an isolated full `git archive HEAD` at `/private/tmp/fix2-audit8-clients-full`, as `contribs/binancemd/audit8_timestamp_test.go`. Three race-enabled repetitions still reproduce both timestamp extremes. Each sends ordinary trade 1, candidate trade 2 and ordinary trade 3 through a fake exchange. Both `math.MaxInt64` and `math.MinInt64+1` omit trade 2 from JSON while trade 3 appears and cancellation returns nil. Ordinary timestamps and the last second of year 9999 output all three trades. This establishes serialized-handler progress past the omitted event, rather than an observation made before delivery completed.

Sources remain `contribs/binancemd/output.go:221-223` (ignored JSON encoding error), `contribs/binancemd/sbe.go:339` (unchecked conversion to `time.Time`) and `contribs/binancefix/marketdata.go:672-675` (timestamp admission rejects schema null, not JSON's narrower range). Scope and impact remain unchanged: malformed/unsupported upstream timestamps cause silent omission in `binancemd --sbe --json trades`; no live exchange incident, ordinary timestamp loss or trading loss was demonstrated. Explicit rejection or output error propagation remains the acceptance criterion, preserving faithful integer codecs.

Evidence: `sbe-trade-timestamp_test.go.txt`, `timestamp-recheck.log`. The failing exit is expected from the defect assertion; all ordinary/year-9999 controls pass. The first sandbox invocation could not access the default Go cache and was rerun with permission; the retained log is the actual executed diagnostic, not the cache-denied attempt.

## Changes since audit seven and focused controls

Only three files in this scope changed since `8e9a7f4`: `binancefix/conn.go`, `binancefix/client_test.go` and `binancefix/internal/probe/tap.go`.

- `conn.go:286-305` rechecks whether Logon already completed when Stop/context/timer becomes ready, preserving the graceful logout path when Stop comes from OnLogon. `conn.go:363-383` recognizes Logout written when writer completion and Logout notification are both ready, then waits for the peer answer. The new `TestStopFromOnLogon` passes ten race repetitions, fifty connections per repetition (500 total), in `stop-onlogon-remediation.log`.
- Probe `tapConn.Write` records attempted output before delegating the write to preserve request-before-answer observation. Its comment explicitly discloses that failed writes can leave attempted messages recorded as sent. This is probe evidence semantics, not a receipt state change; no concrete erroneous acceptance or cleanup decision was established from it.
- Existing SBE gap recovery, maintenance reconnection, trace redaction, blocked-output shutdown and fatal-end-under-backpressure controls pass three race repetitions in `sbe-failure-controls.log`.

Commands, all from the corresponding module in the isolated archive:

```sh
go test -race -count=3 -run '^TestAudit7SBETradeTimestampOutput$' -v .
go test -race -count=10 -run '^TestStopFromOnLogon$' -v .
go test -race -count=3 -run '^(TestSBEGapReconnects|TestSBENewsReconnects|TestSBETrace|TestSBEShutdownWhileOutputBlocked|TestSBEFatalEndWhileOutputBlocked)$' -v .
```

## Source review boundary

Read-only review covered order receipt states and leg correlation, partial-write classification and reconciliation, queue and subscription ownership, logon/logout ordering and reconnection epochs, market-data snapshot/depth/ticker updates, output encoding/backpressure/error propagation, trace credential redaction, and FIXP echo accounting/authentication/TLS/shutdown. Current docs already qualify account-wide client-ID uniqueness, callback cooperation and book accuracy outside initial snapshot ranges; those documented deployment constraints are not new findings.

Root-level broad contrib suites, vet, dependency scans and other project-wide checks are owned by the audit leader and were not duplicated here. No live venue, load/soak, production credential or external deployment action was taken. No production source or permanent test was edited. This is a bounded risk review, not exhaustive inspection of every generated codec or proof of all possible exchange behavior.
