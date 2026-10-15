# Fifth audit: session protocol review

Audited revision: `0da258079503502ece32633a0030bf21e4b2a529`. Production source and existing tests were not edited. New regression source was executed in the isolated `git archive HEAD` checkout `/private/tmp/fix2-audit5-session.oy5BUg`.

## Confirmed completion-boundary defect: reader responses after final Logout

Suggested severity: P2/P3, depending on required counterparty compatibility. This is a confirmed protocol-completion defect, not evidence of authentication bypass or lost application orders. It is separate evidence for the closing/replay lifecycle class also investigated by the runtime reviewer.

The new closing behavior suppresses timer traffic and acknowledgement probes, but the reader still handles ordinary messages and can queue responses. `session/protocol.go:952` unconditionally answers a TestRequest with Heartbeat. `session/session.go:532–536` accepts that queued frame whenever `s.c` is non-nil; it does not check `c.closing`. `session/conn.go:371` closes only once pending is empty, allowing reader responses queued during the final Logout's write to follow it.

Deterministic public-API reproduction: log on, gate a cooperative Store.Sync, send the counterparty's sequence-2 Logout, and wait until its answer reaches Sync. While that final answer is pending, send sequence-3 TestRequest and wait until the session has counted it. Release Sync. The engine writes sequence-2 Logout, then sequence-3 Heartbeat carrying the TestReqID, then closes. Its next sender is 4. The no-extra-input control writes only Logout and closes with next sender 3.

`closing-input_test.go.txt` preserves the failing regression and passing control. `closing-input.log` records three race executions; all controls pass and all TestRequest cases fail, with no race report. The gate avoids relying on socket speed or sleep timing.

This is explicitly admitted in `0da2580`'s `Not-tested` trailer and remains present at HEAD. It defeats the broader stated intent that the answer to a peer's Logout is the final frame; a counterparty that closes after the Logout can miss the extra sequence number, requiring recovery on reconnect. No persistent disagreement or order duplication was demonstrated. Ordinary input during the grace period after an engine-initiated Logout remains intentional; the reproduction specifically waits until the counterparty Logout is accepted and `closeAfterFlushLocked` has begun final closure.

Suggested correction: make the final-close boundary apply to reader-triggered work as well as timer/probe work. Preserve intentional recovery during the earlier Logout grace period, and preserve replay/receipt/retention accounting. Existing blocked-replay behavior needs its own bounded completion/cancellation correction; merely dropping new TestRequest responses does not repair an already running replay.

## A4-01 remediation verified

The stash now retains `keptMsg.received` and supplies it to replayed application and admin handlers. Both recovery loops stop when their captured phase changes, including the final `resetPastLocked` drain. Existing regressions cover normal missing-message recovery, GapFill, Reset-mode recovery, delayed held GapFill and Logout, held Logout stopping later application delivery, chunk boundaries, synthetic fatal headers, and slow Logout-answer writes.

Command: `GOCACHE=/private/tmp/fix2-production-readiness-4-gocache go test -race -count=1 -run '^(TestHeldArrival_|TestHeldLogoutStopsDrain|TestLogoutAnsweredOnSlowWrite)' -v ./session`.

All selected tests passed in 9.375 seconds. See `a4-01-verification.log`. The old valid-at-arrival failure is not carried forward as an outstanding defect.

## Independent TCP Accept cross-review

Leader's reproduction `TestAudit5CanceledAcceptDoesNotPoisonListener` was independently rerun once under race, with loopback access. It fails on attempt 0: the fresh second Accept returns `i/o timeout`. See `tcp-accept-crossreview.log`.

Source: `session/transport/tcp/tcp.go:123–130`. `context.AfterFunc`'s stop function returning false does not join the callback. After a queued connection lets Accept return before the scheduled cancellation callback runs, clearing the deadline at line 126 can happen first, then the callback sets the expired deadline again. The listener remains poisoned for later contexts.

Exposure boundary: this affects standalone/reused TCP listeners across canceled Accept contexts. Default `Acceptor.Run` closes its listeners on cancellation (`session/acceptor.go:301–302`), so it is not evidence of an ordinary active-Acceptor outage. Suggested P3 unless a deployment reuses listener contexts this way. The initial sandboxed loopback attempt failed to bind; the subsequent permitted loopback execution established the source defect.

## Scope boundary

Reviewed session diff since audit-four revision `87fad5a`, with emphasis on protocol, stashed arrivals, gap/Reset-mode drain invariants, final Logout closure, queue admission and related timers. Broad suites, cancellation/SQLite replay and receipt-history verification are owned by the leader/runtime review. No additional independently confirmed sequence-integrity defect was identified in this slice.
