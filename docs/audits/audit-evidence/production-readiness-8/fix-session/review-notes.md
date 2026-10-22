# FIX session, transport and SQLite review — audit eight

Audited revision: `c7e5579227c8e7efc2957ebde0aaf1ce76ba241d`. Baseline: audit seven (`8e9a7f4`). Environment: macOS arm64, Go 1.27.3. Isolated archive: `/private/tmp/fix2-audit8-session-bxvN9M`.

Only this evidence directory was written in the workspace. Production sources and permanent tests are unchanged. Diagnostics were copied as active `_test.go` files only inside the isolated archive; saved `.go.txt` files intentionally fail on this revision. No broad project suite was run in this review lane.

## Retained A7-01 — P3: a queued replay follows final Logout

Three race repetitions of the audit-seven strengthened peer-stream diagnostic still reproduce the defect. The peer stream is:

```text
D@1 dup=; D@1 dup=Y; 5@2 dup=; D@1 dup=Y; 4@2 dup=Y
```

A first Range is gated; our Logout is sequenced behind it; a second infinity-ended ResendRequest includes that Logout's sequence; the peer answers Logout before the first Range is released. Queued replay promotion releases held frames through the replay end (`fix/session/replay.go:298–305`), and removal preserves administrative frames (`fix/session/conn.go:722`). The writer sends pending output before starting replay (`fix/session/conn.go:389`), so Logout precedes the queued application replay and GapFill. Current final-close documentation is at `fix/session/conn.go:356`.

Both controls pass in all repetitions: ending the second range before Logout, and a peer-initiated Logout with the infinity-ended request. The diagnostic reads/parses the actual peer byte stream over `mem.Pipe`. Establishment uses internal helpers rather than a Logon handshake. This remains P3: no duplicate business execution, persistent sequence disagreement or unbounded shutdown was demonstrated. The final-close deadline remains active. Correction should keep the final Logout behind all already accepted replays, preserving other administrative replies' intended priority.

Evidence: `queued-replay-logout_test.go.txt`, `queued-replay-logout.log`.

## Retained A7-02 — P3: cancellation while awaiting TCP Accept serialization

Three permitted-loopback race repetitions still reproduce audit seven's finding. `transport/tcp/tcp.go:121` takes an ordinary mutex before installing cancellation at line 124. A waiter whose 20ms context expires stays blocked for another 100ms, then returns only after cancellation of the first Accept. The sole-Accept deadline control passes every time. Both goroutines are joined; no connection is required. The interface's context bound is `transport/transport.go:38`.

This remains P3: ordinary FIX/FIXP acceptors use one accept loop per listener. Direct concurrent users can accumulate blocked callers without an independent deadline bound. Correction should make guard acquisition context-aware while retaining callback joining and exclusive deadline restoration. The earlier stale-callback repair at `transport/tcp/tcp.go:129–137` remains present.

The initial sandboxed attempt could not bind loopback; it is not correctness evidence. The saved log is the successful permitted execution, which reaches and fails the defect assertion.

Evidence: `tcp-concurrent-accept_test.go.txt`, `tcp-concurrent-accept.log`.

## New P3: in-memory connections ignore expired deadlines on ready I/O

`transport/mem/mem.go:100–105` consumes buffered input without checking `readDeadline`; `transport/mem/mem.go:133–139` appends available output without checking `writeDeadline`. Deadline checks occur only in `wait` (`transport/mem/mem.go:68–76`), reached when input is empty or output is full. Consequently deadlines are conditional on buffer occupancy rather than applying to future I/O.

The diagnostic sets an absolute deadline in the distant past, avoiding timing sensitivity. With one byte buffered, Read returns that byte and nil. With free output capacity, Write returns 1 and nil. Empty-input and full-output controls return `os.ErrDeadlineExceeded`. All three race repetitions produce exactly those outcomes, without sleeps, socket access or races. An additional standard-library `net.Pipe` control returns deadline errors for both operations; the diagnostic also checks that the concrete mem connection satisfies `net.Conn`.

The connection implements the complete `net.Conn` method set, including SetDeadline. Local Go 1.27.3 `go doc net.Conn.SetDeadline` states that deadlines apply to all future and pending I/O; the standard library's `net/pipe.go:155` and `:187` check already expired deadlines before proceeding. Package `transport/mem` itself describes buffered TCP-like connections with deadlines, for tests and sessions within one process.

Impact is limited to the in-process transport and checks made using it: callers cannot treat an expired deadline as an I/O boundary while bytes or room remain available. A continuously ready stream can make progress after its deadline indefinitely. This does not establish a TCP/TLS failure, a normal session outage, persistence loss or an infinite blocked call. P3 is appropriate for public transport semantics. Fix by checking current deadlines before consuming input or appending output, retaining deadline-change wakeups, partial-write counts and close behavior. Cover ready/blocked I/O, already expired/future/cleared deadlines and changes during blocked operations.

Evidence: `mem-expired-deadline_test.go.txt`, `mem-expired-deadline.log`.

## Focused verification and additional source review

- `remediation-encryption.log`: three race repetitions pass of final queue refusal, final-close retention, direct/crossing/held Logout drain and chunk boundaries, replay-bounded Logout, stalled-handler reset refusal, and all `TestEncrypter*` cases. New encrypted-message framing, negotiation, clear/inner-header agreement, binary DATA, signatures, malformed input, replay, reset, partial-write receipt mapping, Open/Seal error paths and size guards were reviewed. Bodyless clear messages and plaintext storage/observer output are intentional and documented. Encryption does not replace authenticated transport; that limitation was not reported as a new defect.
- `encrypter-fuzz.log`: existing `FuzzEncrypterOpen` passes 20 seconds, 341,540 executions. Its oracle covers panic safety and successful open/seal/open round trips with clear and sealed fields. It does not establish cryptographic security or counterparty interoperability.
- `sqlite-focused.log`: focused SQLite race checks pass, including checkpoint/log reuse, long Range allowing checkpoints, Range snapshots/chunks/reset/truncation pins, commit settlement, sticky Sync failures and Close behavior. SQLite production source has no change from the audit-seven baseline; checkpoint tests were strengthened. No further store defect was confirmed. This is not a storage-fault, process/power-loss, Linux or long-soak campaign.
- Source review also covered writer/replay cancellation and generation fences, retention holds, connection attach/detach, callback delivery persistence and memstore snapshots. No additional confirmed high-severity defect was found in this bounded slice.

## Commands

After `git archive` extraction, copy each `.go.txt` diagnostic into its package with a `_test.go` suffix. The first three commands intentionally fail on the reported defect assertions, with passing controls.

```sh
GOCACHE=/private/tmp/fix2-audit8-go-cache go test -race -count=3 -run '^TestAudit7QueuedReplayAfterFinalLogout$' -v ./fix/session
GOCACHE=/private/tmp/fix2-audit8-go-cache go test -race -count=3 -run '^TestAudit7AcceptCancellationWhileSerialized$' -v ./transport/tcp
GOCACHE=/private/tmp/fix2-audit8-go-cache go test -race -count=3 -run '^TestAudit8MemExpiredDeadline$' -v ./transport/mem
GOCACHE=/private/tmp/fix2-audit8-go-cache go test -race -count=3 -run '^(TestClosingRefusesOutput|TestHeldArrival_InSequenceLogoutStopsHeld|TestHeldArrival_InSequenceLogoutEndsChunk|TestCloseDeadlineKeepsRetention|TestReplayWaitingHoldsBackLogout|TestStallLogoutIgnoresResetLogon|TestEncrypter.*)$' -v ./fix/session
GOCACHE=/private/tmp/fix2-audit8-go-cache go test -run '^$' -fuzz '^FuzzEncrypterOpen$' -fuzztime=20s ./fix/session
# from contribs/store/sqlite3:
GOCACHE=/private/tmp/fix2-audit8-go-cache go test -race -count=1 -run '^(TestCheckpoint|TestRangeLetsCheckpointsRun|Test.*(Range|Sync|CommitFailure|Close|Truncate|Reset|Replay).*)$' -v .
```
