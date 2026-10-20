# Operating fix2

What a deployment has to size, configure and watch: memory,
authentication, transport security, what the logs hold, audit trails, the
SQLite store, and a disk that stops answering. Defaults are those of
`session.Config` (`github.com/sylr/fix2/fix/session`) when a field is left at
zero.

## Memory

Every buffer a session keeps is bounded, in messages and in bytes. The
bounds below are per session; a process holding many sessions needs the sum.
A message received is at most `fix.MaxBodyLength`, 1 MiB; a message sent
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
| Receipts tracked | `MaxTrackedReceipts` (1 048 576), and as many again of the last 1 024 generations a sequence reset ended | A few dozen bytes each: tens of MiB; 8 bytes each of earlier generations: 8 MiB | The oldest are forgotten: their `Receipt` reports `ErrReceiptUntracked`. Earlier generations go whole, the earliest first, once together they pass any bound of one generation (receipts, gap-filled spans, rejections) or 1 024 generations: many rejections or gap-fills can make an earlier generation forget sooner what it wrote or had acknowledged |
| Rejections received, for `Receipt.Rejection` | The last 16 384 messages sent (fewer if `MaxTrackedReceipts` is), and as many again of earlier generations | 2 × 16 384 × the first 1 KiB of their Text: about 40 MiB | Older ones are forgotten |

At the defaults, a session's buffers stay under about 310 MiB in the worst
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
as the messages. With a `Dictionary`, it colors MsgType with an ANSI
escape sequence, for a terminal: set `NoColor` when logging to a file or
through slog's text or JSON handlers, which escape it.

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
passwords, passphrases and signatures, unredacted, and bodies in clear even
when an `Encrypter` seals them on the wire. Protect a store's files,
the SQLite database, its write-ahead log and their backups, as credentials.

## Encrypting message bodies

`Config.Encrypter` seals the body of a FIX 4.x session's messages into
SecureData (91), for an EncryptMethod (98) other than 0. fix2 ships no
cipher: the `Encrypter` is the application's, and so are its keys, agreed
with the counterparty beforehand, out of band. Nothing is exchanged in
RawData (95/96).

- Both Logons declare the `Encrypter`'s `Method`; a Logon declaring another,
  or none, is refused, the connection closed. A session dictionary must
  list `Method` among EncryptMethod's values (FIX 4.4's lists 0 to 6), or
  the Logons fail its validation.
- Logons go out in clear, reset Logons included, and so does a message
  without body. Every other message, once the Logons agreed, has its body
  sealed and its standard header in clear: CompIDs, MsgSeqNum,
  SendingTime, PossDupFlag stay readable on the wire. Encryption is not
  transport security: see [Transport security](#transport-security).
- The `Encrypter` must authenticate what it seals, an AEAD such as
  AES-GCM: a header field found inside SecureData is merged into the
  header, and a body opened is delivered, so a cipher a third party can
  alter undetected lets it forge fields.
- On one session, `Seal` runs on the connection's writer and `Open` on its
  reader: they may run at the same time, never two `Seal`s, nor two
  `Open`s. An `Encrypter` shared by several sessions sees as many of each
  as there are sessions.
- A `Seal` error closes the connection, what it was to write unwritten,
  to be resent on the next. A message that does not open, holds no
  fields, repeats a header field with another value, or carries body
  fields in clear, is rejected with SessionRejectReason 7 and counted: it
  is **lost** to the application, as any message rejected. One that hides
  its MsgSeqNum logs out.
- The store keeps messages in **clear**, and observers see them in clear,
  both ways: protect the store as the messages themselves, and redact
  observers by the tags of the body that matter, `SensitiveTags`' SecureData
  being there only before opening.
- Resends are sealed when written, by the `Encrypter` as it is then. What
  holds across a key change is therefore only what the counterparty can
  open with the key in use on the connection: change keys between
  connections, both sides together, the store untouched. A message sealed
  with the old key and read after the change does not open: it is
  rejected, and lost, as above. The `Encrypter` is the session's for its
  life, across reconnects and sequence resets: one that changes keys does
  so itself, or the session is removed and added again with another.

## The SQLite store

`contribs/store/sqlite3` keeps sessions in a SQLite database file, in WAL
mode, through cgo (`github.com/mattn/go-sqlite3`).

- **One store per session key at a time.** Two processes, or two `Store`s,
  writing the same key make each other's writes fail with
  `store.ErrSeqMismatch`, which halts the session.
- **Connections.** A store keeps one of the pool's connections while open;
  replays (`Range`) and checkpoints take others, a replay only while it
  reads a chunk. A `*sql.DB` shared by n stores must allow n+1 connections
  at least. A replay waiting for a connection the application holds is
  cancelled when its connection ends, and the session reconnects; a
  session created meanwhile fails after `StoreTimeout`, its store unable
  to open.
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
  and each `Sync`. A replay reads the messages 512 (or 1 MiB) at a time,
  each chunk in a short transaction, ended before the chunk is sent: a
  replay holds no read transaction while it waits for the counterparty,
  and checkpoints complete between chunks, where a replay in one
  transaction held them all back until it ended. The log starts over at
  the write after a checkpoint, unless a chunk is being read then, and
  otherwise at the next checkpoint's, 1000 commits later: a replay that
  spends most of its time reading, its counterparty taking messages as
  fast as they are read, can hold it back as long, the log growing
  meanwhile. The log is reused,
  not truncated: its file stays as large as it ever grew, by every commit
  made while a checkpoint runs, which a disk slow to flush stretches.
  The messages a replay is still to read stay on disk past a `Truncate`
  meanwhile, until the next `Truncate` or the store's next `Open`.
- **A failed flush or checkpoint** leaves the store in doubt: every call
  from then on fails with `store.ErrUnknownOutcome`, and the session
  halts (`Session.Err`). It does not reconnect. Opening the database again
  can lose more: on Linux (ext4, for one), a page the disk failed to write
  stays in the page cache, marked clean, and a later flush need not report
  it. A store opened on the database again, by this process or another,
  reads that page and builds on it, every `Sync` succeeding; when SQLite
  next recovers the write-ahead log from the disk, it can ignore the
  transactions from the stale frame on, synced ones included, and their
  sequence numbers are used again. Before opening the database again:
  - stop all that has it open, the halted session's store included (remove
    the session, or stop the process), other sessions and processes on the
    database too, and keep whatever restarts them off;
  - mend what failed the disk, then unmount the database's filesystem,
    wholly, not lazily, check it (`fsck`) and mount it again; or reboot the
    host;
  - check the database (`PRAGMA integrity_check`), which finds damage to
    its structure, not transactions lost whole: reconcile the sessions'
    sequence numbers and messages with the counterparty, and with your own
    records, before adding the sessions again or restarting the process.

## A disk that stops answering

A store's writes run under the session's lock: `Append`, for every message
sent, `SetNextTarget`, `SetNextSender`, `Reset` and `Truncate`, and, with the
SQLite store, the flush a `Sync` or a checkpoint makes them wait for.
Nothing the session does can interrupt a write the operating system does
not return from, nor release the lock it holds.

`Config.StoreTimeout` (30s by default) bounds how long such a write may
take before the session gives up on it. A write running longer, between
once and one and a half times `StoreTimeout`, halts the session without
waiting for it:

- it logs `session halted: a store write has run longer than
  StoreTimeout`, `Err` returns an error wrapping `session: a store write
  took longer than StoreTimeout` at once, and `State` reports the session
  stopped;
- the connection is closed, so that the counterparty sees the session go;
- `Stop` returns that error, its own Logout or the store's `Close` hanging
  included; the session stops once the write returns, `Done` closing then,
  and an `Acceptor` or `Initiator` frees its ID.

The goroutines waiting for the lock stay blocked until the disk answers:
`Send` and the other calls that take it, timers, the reader and writer.
Once the write returns, the session stays halted, as for any store failure
that leaves its state in doubt (see `Session.Err`): check the store, then
remove the session and add it again, or restart the process; with the
SQLite store, it resumes from what reached the disk, and a write that
failed requires dropping the page cache first (see [the SQLite
store](#the-sqlite-store)). A `StoreTimeout` too
short halts sessions on writes that were only slow: leave it well above
what a `Reset` or `Truncate` of your largest sessions takes.

A `Sync` running longer than `WriteTimeout`, between once and one and a
half times it, is cancelled: the writer stops waiting for it, then, once
it has the session's lock, drops the connection rather than send what may
not be durable, and logs `making messages durable cancelled: not sent`.
The flush itself goes on, and the SQLite store's next write waits for it,
under the session's lock: `StoreTimeout` then halts the session.

Reads are not watched: a replay's `Range` runs without the session's lock,
and leaves the store's state as it was. It is cancelled when its
connection ends, as on a disconnection, a Logout timing out or `Stop`, or
when our last Logout has waited `LogoutTimeout` behind it; a store that
honors its ctx then returns, as the SQLite store does, and the
session reconnects or stops. One that does not return holds back the end
of that connection, and so the next connection and `Done`, until it does;
`Stop` still returns when its ctx ends.

Once a connection only waits to write our last Logout (the answer to the
counterparty's, or ours once it has answered), nothing more is sent on it:
what the counterparty sends meanwhile goes unanswered (a TestRequest, a
message to reject, a ResendRequest, which it will ask again on the next
connection), `Send` returns `ErrNotLoggedOn`, and a Reject the handler
returns is logged, not sent. A resend asked for before still goes first.
What the counterparty sends after its own Logout, or held after it, is not
handled at all: it is asked for again on the next connection.
