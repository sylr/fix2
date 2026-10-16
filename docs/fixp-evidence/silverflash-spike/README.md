# Silverflash spike

Evidence of the interoperability spike described in
[docs/fixp.md](../../fixp.md#silverflash-spike), run on 2026-10-16 on
macOS, Apple silicon, Go 1.27 and Java 21 (Nix).

- `client.go.txt`: the Go client, encoding the session messages by hand
  from the pinned schema; `SILVERFLASH_SOFH=1` makes it frame as
  Silverflash does, Message_Length without the SOFH header.
- `SpikeServer.java.txt`: the Silverflash server, SOFH framing, the server
  flow given on its command line, answering each application message
  with one of its own.
- `server.sh.txt`, `runall.sh.txt`: how both were built and run.
- `1-standard-sofh.*`: standard SOFH, Recoverable server: no answer to
  Negotiate.
- `2-recoverable-server.*`: Silverflash's SOFH, Recoverable server:
  establishment crashes the server.
- `3-idempotent-server.*`: Silverflash's SOFH, Idempotent server:
  negotiation, establishment, sequencing both ways, NotApplied and
  Terminate work; no server heartbeat in 3.5 s idle; re-establishment
  crashes the server, which the harness explains (a fresh session per
  connection).

Each case has the client's output (`.client.log`), the server's
(`.server.log`) and every frame exchanged, in hex, SOFH header included
(`.wire.log`).

To run it again: Silverflash at commit
`ef066d2ffd4d123763e226c7caca1a976041e9f7`, its parent POM without the
`nexus-staging-maven-plugin`, `javax.annotation:javax.annotation-api:1.3.2`
added to `silverflash-core`'s dependencies, then
`mvn -DskipTests -pl silverflash-core -am package`; JNA 5.14.0 in place of
4.2.2 on the classpath; then `server.sh build` and `runall.sh`, after
adjusting their paths.
