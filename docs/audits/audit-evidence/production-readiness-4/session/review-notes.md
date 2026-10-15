# Session audit at 87fad5a6c5ce0388f204b5210b4fb6a5abe90672

Read-only production source review; deliberately failing regression sources were executed in `/tmp/fix2-audit4-session.2ElaG9`, populated with `git archive HEAD`.

## Confirmed P2: buffered frames lose original arrival time

`session/protocol.go:874` assigns `Inbound.Received = time.Now()` when draining a held frame. It passes that fresh timestamp into normal header checking at lines 885–891, then `onRegularLocked` invokes `checkHeaderLocked` at line 898. SendingTime is therefore compared with drain time rather than the original time the frame arrived. `session/handler.go:19` describes Received as the time its last byte was read.

A valid order at sequence 3 arriving while 2 is missing initially passes the header checks and starts recovery. If filling sequence 2 takes longer than MaxLatency, sequence 3 is rejected as stale and counted as received without application delivery. A reason-10 Reject and Logout are emitted. On reconnection the order's sequence is already below NextTarget and it will not be recovered normally. The reproduction uses a supported 500 ms MaxLatency and a 700 ms recovery delay; the default 120 seconds has the same issue after a longer gap. The ResendRequest provides a synchronization barrier proving that the order passed its initial header check before the delay.

Reproduction: `buffered-arrival-time_test.go.txt`, `TestAudit4BufferedSendingTimeUsesOriginalArrival`. All three executions under `go test -race -count=3` fail as expected, with `apps=[]`, reason 10 and Logout. There is no data-race report. Combined execution log: `stale-held-header-repro.log`.

Fix at the stash boundary: retain original arrival metadata and use it for timestamp checks and delivered Inbound.Received; alternatively avoid rerunning time-relative header validation already completed at arrival, while preserving the documented Received value. Include ordinary fill, GapFill and Reset-mode recovery, and same-time stashed valid/invalid-header cases in regressions.

## Related buffered-drain boundary

When the held sequence-3 heartbeat becomes stale during recovery, its header failure emits Logout but `advanceLocked` continues to sequence 4 because its loop condition at lines 794–799 tests connection identity and closed state, not the phase change. A fresh held sequence-4 application message is delivered while State is `logging out`. The Reset-mode recovery loop at lines 1267–1296 has the same missing phase check.

`checkHeaderLocked` lines 493–494 explicitly promises that a header failure requiring Logout does not process messages held ahead; the nested draining case breaks that promise. `stale-held-header_test.go.txt` reproduces both ordinary gap fill and Reset-mode variants, each three times under race. The ordinary path ends at NextTarget 5 with `[MUST-STAY-BUFFERED]` delivered; the reset variant ends at 6 with the same delivery.

Qualification: normal incoming application messages are generally processed during Logout grace by the current implementation; this is not a general claim that every callback during StateLoggingOut is forbidden. The specific failed invariant is preserving messages already held ahead when an earlier header fails. The valid-arrival-time defect above is the strongest production finding and explains the trigger in these reproductions.

## T01 remediation and boundaries

Targeted existing malformed-tag/header/unusable-GapFill tests pass with race, including CompIDs, timestamp accuracy, high/low sequences, partial-header uncertainty, reset mode, GapFill recovery and logging redaction. See `t01-remediation.log` (13.510 seconds).

Partial MsgType is deliberately echoed into outbound RefMsgType(372), and the existing redaction test explicitly permits observing that value; excluded as an independent finding because it is an intentional protocol compatibility choice. Session-owned diagnostic attributes now withhold partial values.

Reviewed surrounding Logon/reset handling, replay write accounting, retention acknowledgements and receipts. No additional credible high-impact remote sequence overflow defect was established: inbound numeric sequence values are bounded by ParseInt. Public setters and custom stores can still supply uint64 values beyond the signed wire range, but that requires local administrative/custom-store misuse and is not promoted to a production blocker here. Parent auditor owns the separate Written-receipt stability investigation.
