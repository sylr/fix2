# Operating fix2

What a deployment has to size and configure. Defaults are those of
`session.Config` when a field is left at zero.

## Memory

Every buffer a session keeps is bounded, in messages and in bytes. The
bounds below are per session; a process holding many sessions needs the sum.
A message received is at most `fix2.MaxBodyLength`, 1 MiB; a message sent
is as large as the application makes it.

| Buffer | Settings (default) | Worst case per session | When it fills |
|---|---|---|---|
| Messages waiting to be written | `MaxPendingMessages` (10 000), `MaxPendingBytes` (16 MiB) | 16 MiB, plus one message's header; one message of any size when nothing else waits, whose buffer is not kept once written; session-level messages may go past | `Send` waits, or fails with `ErrSlowConsumer` if `FailWhenFull` |
| Messages `Send` holds back during a resend | `MaxHeldMessages` (10 000), `MaxPendingBytes` (16 MiB) | 16 MiB, as above | `Send` fails with `ErrReplayInProgress` |
| Received messages waiting for `FromApp` | `DeliveryQueue` (1 024), `DeliveryQueueBytes` (64 MiB) | 64 MiB plus one message (1 MiB) | The reader stops reading: TCP slows the counterparty down; `MaxDeliveryStall` disconnects a handler stuck too long |
| Received messages kept ahead of a gap | `MaxStash` (10 000), `MaxStashBytes` (64 MiB) | 64 MiB, or one message of any size | Messages beyond are dropped, and asked for again |
| Messages kept ahead of a gap, once it is filled | — | Moved to the delivery queue at once, past `DeliveryQueue`: up to `MaxStashBytes` more there | `MaxDeliveryStall` may trip while a slow handler works through them |
| Receipts tracked | `MaxTrackedReceipts` (1 048 576) | A few dozen bytes each: tens of MiB | The oldest are forgotten: their `Receipt` reports `ErrReceiptUntracked` |
| Rejections received, for `Receipt.Rejection` | The last 16 384 messages sent (fewer if `MaxTrackedReceipts` is) | 16 384 × the first 1 KiB of their Text: about 20 MiB | Older ones are forgotten |

At the defaults, a session's buffers stay under about 230 MiB in the worst
case, most of it only while a gap is recovered and the handler is slow,
plus any message larger than 1 MiB the application sends, while it waits.
Lower the byte settings for many sessions in one process, and raise them for
counterparties that send large messages in bursts.

The **store** is not among these: `memstore` keeps every message sent in
memory until the sequence numbers are reset, unless `Config.Retention`
evicts them (it needs a store implementing `store.Truncater`, which
`memstore` and the SQLite store do). A session sending all day on
`memstore` without retention grows by every message it sends. Size the
retention against how far back counterparties may ask for resends.

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
