# Operating fix2

What a deployment has to size, configure and watch: memory,
authentication, transport security, what the logs hold, audit trails, the
SQLite store, and a disk that stops answering. Defaults are those of
`session.Config` when a field is left at zero.

## Memory

Every buffer a session keeps is bounded, in messages and in bytes. The
bounds below are per session; a process holding many sessions needs the sum.
A message received is at most `fix2.MaxBodyLength`, 1 MiB; a message sent
is as large as the application makes it.

| Buffer | Settings (default) | Worst case per session | When it fills |
|---|---|---|---|
| Messages waiting to be written | `MaxPendingMessages` (10 000), `MaxPendingBytes` (16 MiB) | 16 MiB, plus one message's header; one message of any size when nothing else waits; session-level messages may go past | `Send` waits, or fails with `ErrSlowConsumer` if `FailWhenFull` |
| The buffer the writer last wrote, kept for reuse | `MaxPendingBytes` | Twice `MaxPendingBytes`, 32 MiB; a larger one, grown by an oversized message, is not kept | — |
| Messages `Send` holds back during a resend | `MaxHeldMessages` (10 000), `MaxPendingBytes` (16 MiB) | 16 MiB, as above; its buffer kept up to twice that | `Send` fails with `ErrReplayInProgress` |
| Messages being sent, waiting for room | — | Each `Send` waiting holds its encoded message: as many as the application sends at once, of the size it makes them | — |
| Received messages waiting for `FromApp` | `DeliveryQueue` (1 024), `DeliveryQueueBytes` (64 MiB), counting the buffers holding them | 64 MiB plus one message (1 MiB); buffers over 64 KiB are not kept for the next messages | The reader stops reading: TCP slows the counterparty down; `MaxDeliveryStall` disconnects a handler stuck too long |
| Received messages kept ahead of a gap | `MaxStash` (10 000), `MaxStashBytes` (64 MiB) | 64 MiB, or one message of any size | Messages beyond are dropped, and asked for again |
| Messages kept ahead of a gap, once it is filled | — | Moved to the delivery queue at once, past `DeliveryQueue`: up to `MaxStashBytes` more there | `MaxDeliveryStall` may trip while a slow handler works through them |
| Receipts tracked | `MaxTrackedReceipts` (1 048 576) | A few dozen bytes each: tens of MiB | The oldest are forgotten: their `Receipt` reports `ErrReceiptUntracked` |
| Rejections received, for `Receipt.Rejection` | The last 16 384 messages sent (fewer if `MaxTrackedReceipts` is) | 16 384 × the first 1 KiB of their Text: about 20 MiB | Older ones are forgotten |

At the defaults, a session's buffers stay under about 280 MiB in the worst
case, most of it only while a gap is recovered and the handler is slow,
plus the messages the application is sending meanwhile, and any larger
than 1 MiB while it waits. Lower the byte settings for many sessions in one
process, and raise them for counterparties that send large messages in
bursts.

The **store** is not among these: `memstore` keeps every message sent in
memory until the sequence numbers are reset, unless `Config.Retention`
evicts them (it needs a store implementing `store.Truncater`, which
`memstore` and the SQLite store do). A session sending all day on
`memstore` without retention grows by every message it sends. Size the
retention against how far back counterparties may ask for resends, and see
[the SQLite store](#the-sqlite-store) for what retention does not evict.

Before Logon, an acceptor reads at most `AcceptorOptions.MaxPreLogonBytes`
(16 KiB) of a connection's first message.

## Authentication

An acceptor accepts any Logon whose CompIDs name one of its sessions unless
the handler's `Authenticate` refuses it. `BaseHandler`'s accepts them all:
a session that logs on with a Logon it accepted logs, the first time,

    Logon accepted by BaseHandler's Authenticate, which accepts any Logon: implement Authenticate to authenticate counterparties

with the counterparty's address, and `transport_authenticated=false` when
the connection had neither TLS nor a transport principal. Exposed to
counterparties it does not trust, a deployment authenticates them: in
`Authenticate`, from the Logon's Username and Password, the connection's
TLS client certificate (`LogonRequest.TLS`) or the transport's principal
(`LogonRequest.Principal`), or in front of the acceptor. An `Authenticate`
that authenticates returns nil itself: one that ends by calling
`BaseHandler`'s cannot be told from one that does not, and gets the warning.

`AcceptorOptions.Resolve`, which creates a session for a Logon no session
matches, runs **before** `Authenticate`: the session it creates, and the
store it opens, stay even if `Authenticate` then refuses the Logon. With
`Resolve`, check in `Resolve` itself whom a session may be created for.

## Transport security

`tcp.Listen` and `tcp.Dialer` speak TLS when given a `tls.Config`; FIX over
TLS uses a port of its own, never a plain connection upgraded. An
acceptor's TLS handshake runs before the first message is read, bounded by
`AcceptorOptions.LogonTimeout`, never in `Accept`. To authenticate
counterparties by certificate, set `ClientAuth` to
`tls.RequireAndVerifyClientCert` with their CAs in `ClientCAs`, and check
the certificate against the session in `Authenticate`, from
`LogonRequest.TLS`; sessions sharing CompIDs can be told apart by it in
`AcceptorOptions.Route`. Without TLS, the Logon's Username and Password,
which many venues use, cross the network in clear.

`Route` and `Resolve` get the acceptor's context, without the
`LogonTimeout` deadline: one that blocks holds a connection's place among
`MaxPendingConnections`, and keeps `Run` from returning when it stops.
Give them deadlines of their own.

## What the logs hold

Sessions log through `Config.Logger` (nothing if nil) and keep the values
of `SensitiveTags` (Password, NewPassword, the encrypted passwords, RawData,
SecureData) out of their logs and of the Texts of the Rejects they send, as
far as they can tell them: a value received of 4 bytes or more is withheld
wherever its first 4 bytes appear, a shorter one only in its own field,
and an unparsable message is logged without any of its values. A part of a
secret taken from its middle is not recognized. Add to `SensitiveTags` the
tags that carry secrets for your venues.

Not withheld:

- A session's CompIDs, its identity, on every line it logs. A venue using
  an API key as SenderCompID, as Coinbase does, has that key on every line.
  The key alone does not authenticate: the Logon's signature does.
- Username (553), unless added to `SensitiveTags`.
- What handlers return or log themselves.

`SlogObserver` and `AsyncObserver` redact the tags they are given, or
`SensitiveTags` if none: tags given **replace** `SensitiveTags`, they do not
add to it, so `Tags: []int{553}` logs passwords. Extend the defaults,
`append(slices.Clone(session.SensitiveTags), 553)`, as `binancemd` does for
its API key. An observer's tags change only what that observer masks, not
what sessions withhold from their logs. `SlogObserver` logs every frame at
level Debug: leave Debug off in production, or log to a sink as protected
as the messages.

## Observers and audit trails

None of these records everything that crossed the wire:

- A `MessageObserver` sees the frames a session parses and the frames it
  writes, from its reader and writer: a slow one slows the session. Frames
  that do not parse, those its framer drops as garbled, and connections an
  acceptor refuses before a session takes them, never reach it.
- `AsyncObserver` moves an observer's work to a goroutine of its own,
  through a bounded queue (`NewAsyncObserver`'s size, 1024 by default):
  when the queue is full, frames are **dropped**, counted by `Dropped`. It
  is for logs and metrics.
- `FromApp` and `FromAdmin` get the messages the session accepted: not
  those it rejected, failing validation or authentication, nor the
  duplicates it ignored.
- The store keeps the messages a session **sequenced**, for resending,
  stored before they are written: messages never written, because a
  `Sync` or the connection failed, are there too; resends are not stored
  again. It keeps no message received, only how far they were processed.

A record of every byte sent and received belongs at the transport: wrap
the `transport.Conn` the dialer or listener returns, and record what its
`Read` and `Write` pass.

The store holds the messages sent as they were sent: Logons with their
passwords, passphrases and signatures, unredacted. Protect a store's files,
the SQLite database, its write-ahead log and their backups, as credentials.

## The SQLite store

`contribs/store/sqlite3` keeps sessions in a SQLite database file, in WAL
mode, through cgo (`github.com/mattn/go-sqlite3`).

- **One store per session key at a time.** Two processes, or two `Store`s,
  writing the same key make each other's writes fail with
  `store.ErrSeqMismatch`, which halts the session.
- **Connections.** A store keeps one of the pool's connections while open;
  replays (`Range`) and checkpoints take others. A `*sql.DB` shared by n
  stores must allow n+1 connections at least, n+2 if a `Range` callback
  calls `Range`.
- **Durability.** Each write is a commit a process crash does not lose; a
  machine crash may lose those since the last `Sync`, which the session
  calls before writing what it stored. On macOS, a flush reaches the drive,
  not its platters: SQLite only asks for that with `PRAGMA fullfsync`,
  which slows every `Sync` about tenfold. The store does not set it, and
  will not offer it: on macOS, a `Sync` is not promised to survive a
  power loss.
- **Disk.** The database holds every message sent until `Retention`
  evicts it, or the sequence numbers are reset. Retention is not a hard
  bound: messages that may not have been written, those a resend the
  counterparty asked for has not finished with, and those stored before
  the process started, are kept whatever `Retention.Max` says, until
  written or resent. Size the database for a day's traffic, or what
  retention keeps, times the average message, with room for those.
- **Write-ahead log.** It is checkpointed into the database every 1000
  commits, and a commit writes whole pages, the message's and its
  indexes', some 12 KiB in the store's tests, not the message alone: every
  message sent is a commit, and so are the records of messages received
  and each `Sync`. A checkpoint cannot copy past what a reader still
  needs: while a replay reads (a resend the counterparty asked for, a
  `Range` callback the application runs), the log grows by every commit
  meanwhile, and starts over only once the replay is done. Leave room for
  the commits a long resend may see.
- **A failed flush or checkpoint** leaves the store in doubt: every call
  from then on fails with `store.ErrUnknownOutcome`, and the session
  halts (`Session.Err`). It does not reconnect: check the database,
  then remove the session from its `Initiator` or `Acceptor` and add it
  again, or restart the process.

## A disk that stops answering

A store's writes run under the session's lock: `Append`, for every message
sent, and, with the SQLite store, the flush a `Sync` or a checkpoint makes
it wait for. A disk that hangs holds the session, every call into it
(`Send`, `State`, `Stop`, which closes the store) and its timers, until the
disk answers; nothing the session does can interrupt a write the operating
system does not return from.

A `Sync` running longer than `WriteTimeout`, between once and one and a
half times it, is cancelled: the writer stops waiting for it, then, once
it has the session's lock, drops the connection rather than send what may
not be durable, and logs `making messages durable cancelled: not sent`.
The flush itself goes on, and the SQLite store's next write waits for it:
a `Send` storing a message meanwhile holds the session's lock while it
waits, and the writer, waiting for that lock, neither disconnects nor logs
until the disk answers.

Watch for it from outside the session: a watchdog calling a session method
that takes its lock, `State` for one, with a deadline of its own, reporting
when it does not return. The counterparty sees the session stop answering
and disconnects, after a delay its implementation decides: a fix2
counterparty sends a TestRequest after 1.2 heartbeat intervals of silence
and disconnects 1.2 intervals later. Restarting the process is the way
out; with the SQLite store, it resumes from what reached the disk.
