# FIX session, store and transport review — audit seven

Audited revision: `8e9a7f4`. Isolated archive: `/private/tmp/fix2-audit7-session-iB6ZEB`.
Only audit evidence was written to the working tree. Production source and permanent tests were not changed.

## Confirmed finding: queued replay crosses the final Logout boundary (P3)

`fix/session/replay.go:288–295` releases held frames through the queued replay's end before activating it. `fix/session/conn.go:695` deliberately retains all administrative frames when removing pending messages covered by a replay. The writer sends pending frames first (`conn.go:373–386`). Together, these release a held Logout before an already queued resend whose range includes its sequence number.

The window requires our initiated Logout to be sequenced while a first replay runs, followed by another ResendRequest during the intentional Logout grace period. An infinity-ended request is capped at the sender's current last sequence, which now includes the held Logout. The peer then answers our Logout, starting final closure. When the first replay completes, queued-replay promotion releases that Logout into pending. The writer emits it, then serves the queued replay, including an application retransmission and the Logout's own GapFill.

`queued-replay-logout_test.go.txt` uses valid complete peer frames through a real `mem.Pipe` stream. Its write-recording wrapper delegates every write to that stream. A cooperative memstore wrapper gates only the first Range so the required ordering is deterministic. Session creation, attachment and establishment reuse the existing internal test helpers; it does not perform a Logon handshake. All transport writes in the small diagnostic succeed completely. Peer input is parsed and processed by the actual reader loop.

Three executions under the race detector produce:

- **Crossing Logout, queued infinity range includes Logout:** original order 1; replay order 1; Logout 2; replay order 1; GapFill 2. The assertion that Logout is last fails.
- **Crossing Logout, queued range ends before Logout:** original order 1; replay order 1; replay order 1; Logout 2. Passes.
- **Direct peer Logout, queued infinity range:** same passing ordering. The final answer is sequenced after that request's end snapshot, so it remains held until both replays finish.

No data race, persistent sequence disagreement, application loss or duplicate execution is demonstrated. The output after Logout is a valid PossDup replay and GapFill; it consumes no extra sender sequence beyond the already sequenced Logout. This is P3, consistent with audit five's final-output ordering finding. The existing final-close deadline continues to apply. This is an uncovered queued-replay promotion path, not a regression in audit six's inbound Logout drain fix, and it does not reopen the indefinite-close P2.

Acceptance should preserve earlier Logout grace and already requested resends, but keep a held final Logout behind *every* replay queued before final closure, including requests whose snapshot already covers that Logout. Do not globally release administrative messages after replays: existing ordering intentionally sends other administrative replies before the replay. Keep crossing/excluding-range and peer-initiated controls, plus timeout/cancellation/receipt/retention coverage. A peer may close on Logout before reading the remaining replay; the diagnostic does not establish the peer's business behavior.

Run from an isolated archive after copying the diagnostic to `fix/session/audit7_queued_logout_test.go`:

```sh
go test -race -count=3 -run '^TestAudit7QueuedReplayAfterFinalLogout$' -v ./fix/session
```

Only the crossing/includes-Logout subtest should fail on the audited revision. `queued-replay-logout.log` contains the three repetitions. An initial reproduction compile error and sandbox cache rejection were corrected before the recorded run; neither is counted as evidence.

## Confirmed finding: concurrent TCP Accept ignores its waiter's cancellation (P3)

`transport/tcp/tcp.go:121` acquires an ordinary mutex before `context.AfterFunc` is installed at lines 123–127. A second Accept is therefore unable to observe its own context while an earlier Accept holds that mutex waiting for a connection. This conflicts with the Listener interface's context bound (`transport/transport.go:38`).

`tcp-concurrent-accept_test.go.txt` starts a real first Accept on an empty loopback listener, waits until that call owns its serialization lock (TryLock observes it; the diagnostic never holds it), and launches a second Accept with a 20 ms context deadline. The second call is still blocked 100 ms after its context ends; canceling the first call releases it immediately, with the original deadline error. Both calls are joined. The sole-Accept control returns on its 20 ms deadline. All three race executions reproduce the failure and pass the control; no memory data race occurs. `tcp-concurrent-accept.log` records them.

This is P3 and does not demonstrate failure of the ordinary Acceptor, which runs one accept loop per listener. The exposure is direct users sharing a TCP listener between concurrent Accept calls with different contexts. No connection is required to trigger it. The first Accept may legitimately use a long-lived context and wait indefinitely, so the second call's delay has no bound of its own. This is separate from audit five's stale cancellation callback: the callback join and deadline restoration fix is present and correct once the lock has been acquired.

Acceptance: make acquisition of the Accept serialization guard context-aware while preserving exclusive listener-deadline ownership, callback joining before guard release and deadline restoration. Keep existing cancellation/reuse and TLS tests plus the concurrent-wait and sole-call controls.

```sh
go test -race -count=3 -run '^TestAudit7AcceptCancellationWhileSerialized$' -v ./transport/tcp
```

## Existing remediation and store verification

`remediation.log`: three race executions pass for final-output queue refusal, direct/crossing Logout drain and chunk boundaries, close-deadline retention preservation, replay-bounded Logout including crossing/direct cases, and stalled-handler reset-Logon refusal. The filtered command is the log's following reproduction command:

```sh
go test -race -count=3 -run '^(TestClosingRefusesOutput|TestHeldArrival_InSequenceLogoutStopsHeld|TestHeldArrival_InSequenceLogoutEndsChunk|TestCloseDeadlineKeepsRetention|TestReplayWaitingHoldsBackLogout|TestStallLogoutIgnoresResetLogon)$' -v ./fix/session
```

`sqlite-race.log`: the full SQLite module race suite passes (`go test -race -count=1 ./...` from `contribs/store/sqlite3`). Source review found no additional confirmed SQLite or memstore defect. The suite covers conformance, restart, commit settlement, sticky failures, flush/checkpoint configuration, reset and truncation snapshots/pins, cancellation and real session closing during pool contention.

## Review scope and limits

Inspected session connection/writer/deadline lifecycle, inbound sequence and Logout handling, delivery persistence, replay queue promotion, receipt and retention accounting, acceptor/initiator attachment and shutdown, transport cancellation and in-memory streams, memstore snapshot/truncation, and SQLite writer/sync/checkpoint/range/pinning paths. Existing operations boundaries around callback cooperation, acceptor Route/Resolve deadlines, resource ownership, retention growth and macOS power-loss durability were not treated as new findings.

Audit six's direct/crossing/held Logout drain and malformed-after-Logout gates are present. Focused remediation results and SQLite suite results are recorded separately. These tests do not provide storage-fault, machine-crash, power-loss, live-counterparty or capacity evidence.
