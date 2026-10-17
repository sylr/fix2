# fixpecho

A FIXP echo server and its load client, on fix2's
[`fixp/session`](../../fixp/session) ([docs/fixp.md](../../docs/fixp.md)):
the server answers each application message with itself; the client
sends messages, at most a window of them awaiting their echo, measures
their round trips, then finishes its flow, and returns once the session
is finalized. Both keep their sessions in memory (`memstore`): a restart
negotiates new ones.

```sh
go run . server -listen 127.0.0.1:7760 -token s3cret
go run . client -connect 127.0.0.1:7760 -token s3cret -count 100000 -window 100
```

```
sent 100000, echoed 100000, duplicates 0, not applied 0, connections 1, in 130.391ms: 766926 echoes/s
round trip: min 17µs, p50 85µs, p90 120µs, p99 206µs, p99.9 778µs, max 1.497ms
```

(Apple M4, loopback, both ends on one machine. With `-window 1`, one
message at a time: 42,897 echoes/s, round trip p50 20µs, p99 36µs.)

The message is an SBE message of schema 100, template 1: its number and
send time, padded to `-size` bytes. A round trip runs from its send to its
echo's last byte read.

## Flows

The server's flow is `-flow` (Recoverable by default), and it accepts the
client flows of `-client-flows`. The client's is `-flow` (Idempotent by
default, the pair FIXP recommends); `-server-flows` limits the server
flows it accepts. None is refused on either side: there would be nothing
to echo.

## Backpressure

An echo is sent from the server session's callback, which never waits
for room in the session's queue (`-max-pending` messages). When the queue
is full, the echo waits for room, at most `-echo-wait`, holding back the
connection's reads, and so the client, through TCP. Past it, the server
terminates the connection: on a client's Recoverable flow the message is
asked for again on the next one, and echoed then; on an Idempotent or
Unsequenced one it was taken, and its echo never comes: the client's run
ends with its `-timeout`. A client's messages the server never got, it
reports with NotApplied on an Idempotent flow, and the client counts them.

## TLS

The server takes `-tls-cert` and `-tls-key` (PEM). The client takes
`-tls`, and verifies the server against `-tls-ca` if given, the system's
roots otherwise, for `-tls-server-name`, the address's host by default.

## Credentials

With `-token`, the server requires its value as the Negotiate's and
Establish's credentials, and the client sends it. Without, the server
accepts any client, and says so. The token is compared in constant time,
and never logged; it is no substitute for TLS.

## Stopping

SIGINT or SIGTERM stops either: the server terminates its sessions
(Terminate Finished), the client its connection. The client's `-timeout`
bounds its run; stopping then terminates its connection, the Terminate's
answer awaited a keepalive interval at most. A session ending otherwise,
such as one the server no longer knows (`Unnegotiated`), ends the run at
once, with its error.
