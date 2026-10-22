# Independent source and diagnostic review of queued replay / final Logout

Reviewed the current `queued-replay-logout_test.go.txt`, its three race-run
outputs, `fix/session/replay.go`, `conn.go`, the shared test helpers and
`transport/mem/mem.go` at `8e9a7f4`. No additional broad test was run.

The finding is valid and P3 is proportionate. `runReplay` promotes a queued
request by releasing held frames through its end (`replay.go:288-295`). The
pending-range removal deliberately keeps administrative frames (`conn.go:695`),
and the writer prioritizes pending frames before starting the next replay
(`conn.go:373-386`). When a queued request's end includes our held Logout,
this promotes the Logout ahead of that replay. There is no later final-Logout
write guard preventing the resulting application retransmission and GapFill.

This contradicts the explicit ordering in `closeAfterFlushLocked`'s contract
(`conn.go:342-347`) and `docs/operations.md:251-256`: earlier requested resends
go first and our final Logout is last. It is not merely traffic during the
earlier, intentional Logout grace period: the reproduction waits for both
`closing` and `peerLoggedOut` before releasing the store gate.

The setup is sound for this bounded claim. It reuses a session attached to a
real `mem.Pipe`, changes only establishment state through established internal
test seams, and supplies syntactically valid complete ResendRequest/Logout
frames through the actual reader loop. The first Range gate is outside store
locks, cooperates with cancellation, and changes scheduling rather than replay
content or writer policy. The current recording wrapper calls the real stream's
Write; the original heldConn alone would only simulate a successful transport.
Here writes are tiny compared with the stream's 1 MiB buffer and the peer stays
open, so the current diagnostic does not depend on a blocked or failed write.
It records writes before delegating; reading and parsing the peer-side stream
would make transport-success evidence more explicit, but the source ordering
and controls already support the finding. An older independent execution using
only the simulated heldConn should not be described as a second real-stream
execution of the updated source.

Both controls are meaningful. Capping the second resend at sequence 1 keeps
Logout 2 held until all replays complete. A peer-initiated final Logout is
sequenced after the second request's range snapshot, likewise keeping it held.
Only the crossing Logout with a queued range covering its sequence fails; the
three runs agree.

The observed post-Logout order carries PossDupFlag=Y and the GapFill covers the
administrative Logout. This establishes an outbound protocol/ordering defect,
not duplicate business execution, loss of durable data or a reused sender
sequence. A peer may close on Logout and miss the following replay, but can
request it again on the next connection. No unbounded wait is shown, and the
existing close deadline remains active. Those limits support P3 rather than
P2. The correction should retain the final Logout behind all already accepted
replays without discarding the intentional priority of other administrative
messages, and preserve retention accounting for anything not actually resent.

## Peer-stream follow-up

The leader subsequently saved `queued-replay-logout-peer-stream_test.go.txt`
and its output. This variant calls `io.ReadAll` on the peer endpoint after
connection closure and parses those actual bytes. Its failing includes-Logout
case and passing controls reproduce the same ordering. This closes the
record-before-delegated-Write evidence qualification above: the post-Logout
retransmission and GapFill demonstrably reached the peer-side stream.
