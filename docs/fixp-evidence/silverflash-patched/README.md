# Silverflash, patched: live sessions

fix2's FIXP client against a Silverflash server, run on 2026-10-17 on
macOS, Apple silicon, Go 1.27 and Java 21 (Nix). Partial interoperability
with a patched peer, not conformance of either side.

- Silverflash at commit `ef066d2ffd4d123763e226c7caca1a976041e9f7`, built
  as for [the spike](../silverflash-spike/README.md) (its POM without the
  `nexus-staging-maven-plugin`, `javax.annotation-api` added, JNA 5.14),
  with `sofh.patch` applied: SOFH 1.0's Message_Length includes the
  header, which Silverflash leaves out. Then
  `mvn -DskipTests -pl silverflash-core -am package`.
- The server is the spike's
  ([`SpikeServer.java.txt`](../silverflash-spike/SpikeServer.java.txt),
  [`server.sh.txt`](../silverflash-spike/server.sh.txt)), with an
  Idempotent server flow, answering each application message with one of
  its own: `server.sh run PORT 120000 Idempotent`.
- The client is fix2's `fixp/session`, through
  [`fixp/tests/silverflash`](../../../fixp/tests/silverflash).
  [`interop.sh.txt`](interop.sh.txt) runs each case below against a
  server of its own, started fresh, and fails if a case does, or if the
  server's log lacks the exception a case characterizes: a case
  that fails on Silverflash's side leaves its server unusable. It waits
  for the server's port with `lsof`, not by connecting: a connection
  closed without a frame kills Silverflash's session thread as below.

What works (`run.log`): negotiation and establishment with an Idempotent
and a Recoverable client flow; 2000 orders each, by windows of 50 awaiting
their answers, the answers delivered in order, numbered 1 to 2000; the
Terminate exchange, Silverflash's Terminate observed.

What does not, on Silverflash's side, each case an opt-in test asserting
the failure (`<case>.test.log`, `<case>.server.log`):

- `burst`: 2000 orders sent at once. Its consumer receives 735 of them
  (725 to 738, and 1350, in earlier runs), then no more, without an error;
  their answers come, in order, the rest never. Likely the `fragmented`
  case's cause, a burst filling its reads of 2048 bytes, a frame
  straddling two of them lost: not confirmed, its reads not instrumented.
  Once fix2's writer wrote what its queue held in one write (M9), the
  same run got 962 answers.
- `finish`: our FinishedSending. Silverflash answers a Terminate (code
  Finished) right away, without FinishedReceiving, and closes the
  connection: the session is not finalized. Our client connects again to
  re-establish it; Silverflash logs a `NullPointerException` in its
  `EventReactor` (`topic=null`), its session thread dies, and no answer
  comes.
- `fragmented`: a Negotiate written in two writes, its first 3 bytes,
  then the rest 50ms later. The same `NullPointerException`; no answer.
  Silverflash's read boundaries are not instrumented; its
  `AbstractTcpChannel.readyToRead` clears the read buffer before each
  read, which drops what a read leaves of a frame.
- No heartbeat when idle (the spike's finding), and 4 to 6 zero bytes
  after its session messages: the tests set the client's
  `ToleratePeerWithoutKeepalives` and `TolerateTrailingBytes`. The runs
  recorded here predate both (S4): the client then had a leniency of 30
  keepalive intervals, and ignored the padding.

Not exercised: a Recoverable server flow (it crashes Silverflash, the
spike's finding), re-establishment of a live session, retransmission and
NotApplied (neither side has a gap), frames coalesced in one read
(whether any were is not recorded), fix2's server against Silverflash's
client.
