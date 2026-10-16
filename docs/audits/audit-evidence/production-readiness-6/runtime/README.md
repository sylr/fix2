# Runtime/store review evidence — audit six

Revision: `7a6734b70c468a797d01cfaf0366c86c5f9fbe81`.
Baseline: audit five at `0da258079503502ece32633a0030bf21e4b2a529`.
Isolated archive: `/private/tmp/fix2-audit6-runtime.uquicT`.

No new defect was confirmed in the assigned runtime/store/replay/receipt/retention review. Production source and existing tests were not changed.

## Additional retention diagnostic

Copy `retention_close_deadline_test.go.txt` to `session/audit6_retention_close_test.go` in an isolated archive. It reuses existing internal test helpers and exercises a cooperative, context-aware store based on memstore. Its connection reports complete writes without forwarding them, as the existing `replaySession` helper does; these are deterministic accounting checks, not a real-network interoperability test.

Three cases cover cancellation before the first replay message, after a complete replay chunk, and a successful Range return at cancellation with a buffered final message. The final-close deadline cancels and joins replay. The retention hold remains, all three originals are still present after a forced retention sweep despite Max=1, only complete replay frames are counted, and prior Written receipts remain stable. An acknowledgement control releases the hold and lets eviction advance. All pass with the race detector, including three repeated executions in `focused-runtime-race.log`.

This is evidence for the retention invariant; it does not deterministically pause the scheduler between the final replay context check and flush. Source review checked the `!c.closed` condition guarding `deliveredLocked` for that narrower ordering.

## Existing remediation checks

`focused-runtime-race.log`: three executions each of the added diagnostic, closing/replay deadlines, the stalled-handler reset boundary, slow Logout writes, final-output gates, already-requested replay ordering, partial replay accounting and bounded receipt history. All pass.

`sqlite-closing-remediation.log`: three executions each of the real SQLite tests for peer-initiated/crossing Logout during pool contention, disconnected replay reconnecting while the spare remains held, Run/Done cancellation, and Open's pool-wait deadline. All pass.

Commands used writable GOCACHE `/private/tmp/fix2-production-readiness-4-gocache`, `go test -race -count=3`, and explicit test-name filters. Broad verification belongs to the leader's separate evidence.
