# Jepsen suite for the FIX session layer

How fix2's FIX session layer (`fix/session`, with the SQLite store of
`contribs/store/sqlite3`) is tested under process kills, power loss, disk
faults, partitions and clock skew, for
[issue #1](https://github.com/sylr/fix2/issues/1). The suite is built in the
[increments](#increments) at the end, of which the table says which are
done.

FIXP (`fixp/session`) is out of scope here; the topology, node binary and
nemeses are meant to serve it later (see [J12](#increments)).

## What the suite checks against

The checkers hold the session layer to what it documents, no more. Where
issue #1 asks for more than that, the documentation wins:

- **Received messages are processed at least once, not exactly once.**
  `FromApp` runs before the session records how far it processed, every 64
  messages at most and whenever its queue goes idle (`config.go`, "processed
  at least once"). After a process kill, up to 64 messages delivered before
  it are delivered again; after a power loss, those since the last `Sync`
  too. "No message delivered twice" holds within one process's life, not
  across restarts.
- **`Acknowledged` is reached only through probes.** A TestRequest written
  after the message, answered on the same connection, proves the
  counterparty's engine read it, not that it processed it. A message last
  written on an earlier connection is not probed again: its receipt waits
  until a resend rewrites it, a reset, a halt, or the caller gives up.
- **Receipts live in memory.** A process restart forgets them; the suite
  never asks a restarted node about a receipt of an earlier process.
- **Outbound messages are stored, then synced, then written.** A
  `kill -9` loses no committed write. A power loss may lose what was
  committed after the last successful `Sync`, and nothing before it, whether
  it was written or not: the writer syncs a batch before writing it, and a
  batch synced but never written, its connection gone, is as durable as the
  rest. A sequence number the counterparty saw is never reused for another
  message.
- **`ErrSkipped` means a GapFill covering the message was written** before
  the counterparty acknowledged it. It does not mean the counterparty never
  read the message: a counterparty that restarts behind may ask again for
  what it had read.
- **`OnRecoveryLoss`** reports messages that may never be delivered: ours
  evicted by retention or missing from the store, and the counterparty's that
  a SequenceReset-Reset skipped during a recovery. A GapFill received, or one
  sent because `OnResend` returned false, is not reported. A retention
  GapFill is written before its report is queued: a kill between the two
  loses the report.
- **A halted session (`Session.Err`) neither sends nor connects again**
  until it is removed and added again, or its process restarts: the
  documented reconciliation. A `Send` failing because the store's outcome
  is unknown halts the session, but the message may be stored all the same.
  Halting does not stop everything at once: writes under way finish,
  delivery goes on recording how far it processed, a store write the
  watchdog gave up on may still commit, and a receipt that reached
  `Acknowledged` before stays so.
- **A message the session finds invalid** is rejected and counted as
  received without reaching `FromApp`. The workload sends only valid
  messages: such a Reject of one of them is an error of its own.
- **The SQLite store fails in two ways.** A commit that fails returns
  `ErrUnknownOutcome` once: the store reads its state back and carries on
  from whichever outcome it was. A flush, a checkpoint or that read-back
  failing puts it in doubt for good: every call fails from then on.
- **Administrative messages use sequence numbers** but never reach
  `FromApp`, and a resend gap-fills them: delivered MsgSeqNums are not
  contiguous, application messages are.
- **A SendingTime beyond `MaxLatency`** in a message received in sequence
  is rejected, counted as received, and the session logs out: the message
  is never delivered nor asked for again, and `OnRecoveryLoss` does not
  report it. A message received ahead of a gap is not counted, and is asked
  for again. A first Logon with such a SendingTime is refused by
  disconnecting, without a Reject; a reset Logon on an established
  connection is rejected and may be counted. A resend whose OrigSendingTime
  is later than its
  SendingTime, once a clock went back, is rejected without being counted,
  until the clocks catch up.
- **While a store write hangs**, `Send`, `Receipt.Wait` and the sequence
  number getters wait for the session's lock, whatever their context says,
  and `Send` then stores with a context of its own. `State` and `Err` wait
  for it too, until the write passes `StoreTimeout` and the session is found
  stuck.
- **Generations are numbered by each store on its own.** The base runs
  never reset, both sides stay at generation 1, and the checks key on it.
  Runs that reset (J11) key on each side's resets as its store records them.

## Topology

Three containers on one Docker network, each Debian 13 (trixie),
privileged:

| Node | Role |
|---|---|
| `control` | Jepsen (Clojure, JDK 21+, Leiningen, gnuplot, graphviz); runs the test, the clients and the checkers |
| `n1` | fix2 initiator: one node binary, its SQLite store, sshd |
| `n2` | fix2 acceptor: the same binary, its SQLite store, sshd |

The two fix2 nodes speak FIX 4.4 over TCP to each other; the control node
reaches each one's control API over HTTP. Partitions cut `n1` from `n2` and
leave the control node's traffic alone, as Jepsen's `iptables` partitions do.

Jepsen's own Docker environment was removed from its repository in March
2026 and Docker is no longer supported there; the community
[jepsen-docker](https://github.com/nurturenature/jepsen-docker) publishes
amd64 images only. The suite keeps its own small environment under
`jepsen/docker`, built locally, on the community one's model: privileged
nodes, SSH keys generated by a setup container, LazyFS built into the node
image.

What Docker Desktop's VM offers, checked on arm64 (kernel 7.0.14-linuxkit):
FUSE, netem, iptables (nf_tables), loop devices; device-mapper without its
`delay`, `flakey` and `dust` targets. The containers share one kernel clock.

## The node binary

`jepsen/node`, a Go module of its own (cgo, for SQLite), built for the
nodes' architecture in a `golang` container and uploaded by the test at
setup. One binary plays either role:

    fix2-jepsen-node --role initiator|acceptor --listen :9876 --connect n2:9876
                     --http :8080 --store sqlite:/var/lib/fix2/store/fix.db
                     --events /var/lib/fix2/events --incarnation <id>

- **Sessions.** One session per node pair to begin with, more in J11. The
  handler accepts every Logon and every resend (`OnResend` returns true).
  Retention and sequence resets are off unless a variant turns them on.
- **Store wrapper.** The session's store is wrapped, keeping the optional
  interfaces (`Syncer`, `Truncater`) the store has, to log each call's
  outcome: what was appended, each `Sync` that returned nil with the last
  sequence number appended before it began, resets, truncations, errors.
  What reached the disk is read from there, not inferred from the wire.
- **Control API.**
  - `POST /send` sends a NewOrderSingle with the ClOrdID it is given, waits
    for `Acknowledged` up to a timeout, and answers with the generation,
    sequence number, the level reached and the error, if any. A hung disk
    blocks `Send` past its context, so the request is answered at its
    timeout while the call goes on in a worker; workers are capped, and one
    not returned is reported unresolved, an `:info`.
  - `GET /status` answers from a snapshot a background goroutine refreshes,
    with its age: `State`, `Err`, the counters the node keeps from its
    events, the number of unresolved workers. A snapshot too old says the
    session is unreachable rather than block.
  - `POST /drain` refuses new sends and waits for the workers to return.
  - `POST /barrier` sends a marker message, of a ClOrdID of its own.
  - `POST /clock` sets the offset of `Config.Now` (J10).
- **Event logs.** JSON lines, one file per process, on a disk no nemesis
  touches, each line written with `write(2)` before the callback returns, so
  a `kill -9` loses none:

  | Event | From |
  |---|---|
  | `start` | the process, with its incarnation and the store state `Open` loaded |
  | `deliver` | `FromApp`: generation, MsgSeqNum, ClOrdID, PossDup, the message's digest (below) |
  | `recovery-loss` | `OnRecoveryLoss`: begin, end, cause |
  | `logon`, `logout` | `OnLogon`, `OnLogout` |
  | `in`, `out` | a `MessageObserver`: generation, MsgSeqNum, MsgType, PossDup, ClOrdID, digest, SendingTime and the node's `Now`, GapFill range, ResetSeqNumFlag, a Reject's RefSeqNum and reason |
  | `store` | the store wrapper: each call's start and end, generation, sequence numbers, outcome; for a `Sync`, its `upToSeq` and the last `Append` of the generation completed when it began |
  | `halt` | `Session.Err` turning non-nil, seen by a watcher, late; whether the watchdog or a store error halted it |

  A message's **digest** covers its application fields only, not the
  standard header and trailer: a resend changes BodyLength, CheckSum,
  PossDupFlag, PossResend, SendingTime and OrigSendingTime, and is the same
  message.

  `OnInbound` sees a frame before the session validates it: ignored,
  rejected and early frames too. `OnOutbound` runs once a frame is
  completely written: a kill between the write and the log line loses the
  line, not the frame, so the checkers take the counterparty's `in` events
  as the proof that a message was written. The observer is synchronous
  (`AsyncObserver` drops frames when it falls behind) and runs on the
  session's reader and writer: the log's lock is never held across a call
  into the session.
- **Mutants.** Flags that break the node on purpose, to show the checkers
  catch what they should: `--mutant no-sync` (the wrapper's `Sync` does
  nothing), `--mutant drop-deliver` (some messages are taken from the
  delivery log as if never delivered), `--mutant redeliver` (some delivered
  twice in one process).

## The test

`jepsen/` holds a Leiningen project on `jepsen` 0.3.15 or later (0.3.12 to
0.3.14 have a `grepkill!` that kills nothing).

- **DB.** `setup!` uploads the binary, mounts the store's disk (plain,
  LazyFS or a small loop filesystem, as the nemeses need), and starts the
  node with `start-daemon!`. It implements `db/Kill` (`kill -9`), `db/Pause`
  (`SIGSTOP`, `SIGCONT`) and `db/LogFiles` (the event logs, the node's log,
  the SQLite files).
- **Client.** One `:send` operation per order, with a unique ClOrdID, from
  the node the worker is bound to:

  | Outcome | Op |
  |---|---|
  | `Acknowledged` | `:ok` |
  | `Send` failed without touching the store (halted or stopped already, queue full, node down before the request) | `:fail` |
  | `Send` failed on the store (`ErrUnknownOutcome`, `StoreTimeout`): perhaps stored | `:info` |
  | `Accepted` or `Written` and timed out, `ErrSkipped`, `ErrUnknownDelivery`, `ErrReceiptUntracked`, unresolved, a request lost mid-flight | `:info`, with the level and error |

- **Phases.**
  1. The workload under the nemesis, for the time limit.
  2. The nemesis's final generator: heal the network, resume paused
     processes and LazyFS, free the disk, reset clocks; only then start what
     is down, and restart every node that halted or whose disk was faulted.
  3. Drain: `/drain` on both nodes; a node whose workers do not return is
     restarted.
  4. Barrier: `/barrier` on both nodes, then poll until each has delivered
     the other's marker. `FromApp` runs in sequence order, so the marker
     closes the sequence numbers before it in its generation: each was
     delivered, gap-filled, rejected or reset past. Which of the sends before
     it still had to be delivered is for the checks to say: one never
     written that a power loss took is not. A test whose
     markers are not delivered in time fails as not live; the timeout
     exceeds `MaxLatency` plus the largest skew, for resends refused after a
     clock went back.
  5. The event logs are downloaded.
- **Nemeses.** Composed from `jepsen.nemesis.combined`'s packages one by
  one, never through `nemesis-package`, which always includes the clock
  package and would step the clock of the whole Docker VM.

## Checks

Per direction, over the Jepsen history and the event logs of every
incarnation, sequence numbers taken within a generation, messages below the
barrier.

- **Seen** means in an `in`, `out` or `deliver` event: a sequence number a
  `/send` returned, for a message a power loss then took, is not.
- **Durable** means, with the SQLite store, an `Append` completed before a
  `Sync` that returned nil began, in the sender's `store` events: SQLite's
  `Sync` flushes every commit before it, past its `upToSeq`. This is what
  the checks may demand, not all that reached the disk. A `memstore` `Sync`
  makes nothing durable across a restart.
- **Reported lost** means covered by an `OnRecoveryLoss` on either side (the
  sender reports its messages evicted or missing, the receiver those a
  SequenceReset-Reset skipped), or rejected for its SendingTime: a Reject
  (35=3) with SessionRejectReason 10 and its RefSeqNum, among the receiver's
  `out` events or the sender's `in` events. A message the receiver saw ahead
  of a gap is asked for again whatever its SendingTime. A message with no
  such Reject, its receiver killed before writing one, leaves the check
  `:unknown`, not failed. With retention on (J11), a GapFill over an
  application message counts as reported only if the sender's `store`
  events show it truncated first, the report itself perhaps lost to a kill.
- **The same message** means the same ClOrdID and digest. A GapFill is not
  a message: it covers the sequence numbers it fills.

1. **Order and duplicates.** Within an incarnation, deliveries rise
   strictly by MsgSeqNum and no ClOrdID is delivered twice.
2. **Redelivery after a restart.** An incarnation delivers, in order, a run
   of application messages beginning at or before the first one no earlier
   incarnation of the generation delivered, with only reported losses
   skipped. Those it delivers again are the same messages as before. It
   need not reach the end of what was delivered before: another
   kill may cut it short. Under process kills alone, the run begins no more
   than 64 MsgSeqNums, administrative ones included, before the highest one
   any earlier incarnation delivered.
3. **One message per sequence number.** No (generation, MsgSeqNum) of
   either direction carries two different messages, across all
   incarnations of both sides: two orders, or an order and a session
   message, or two session messages of different MsgTypes. Resends and
   GapFills do not count as different.
4. **Acknowledged ⇒ delivered.** Every `:ok` send is delivered by the
   counterparty, unless reported lost.
5. **Nothing lost silently.** Every application message the counterparty
   received, every durable one, and every send that reached `Accepted` in a
   run without power loss, is delivered, unless reported lost.
6. **`ErrSkipped` only when gap-filled.** A send that failed with
   `ErrSkipped` has a GapFill covering it among the sender's `out` events,
   or the counterparty's `in` events.
7. **Sequence numbers survive.** After every restart, the store state
   `Open` loads puts the next sender sequence number past every durable
   message and every message the counterparty saw, from earlier
   incarnations of the generation; in a run without power loss, past every
   message appended, synced or not, since a kill loses no commit.
8. **Halts.** Once a session halted, in that incarnation: no `Append`
   begins, no Logon is sent or accepted, and no `Send` called after the halt
   succeeds. What was under way may finish: writes, `SetNextTarget` as
   delivery goes on, a store write the watchdog gave up on, a receipt
   already acknowledged. A halt in a run whose nemeses do not touch the
   disk is an error.
9. **No phantoms.** Every delivered ClOrdID was sent by a `:send` from the
   other side, or is a marker.

Alongside: `checker/perf` with the nemesis windows shaded,
`timeline/html`, `checker/stats`, `unhandled-exceptions`, and the node
logs searched for `panic:`.

The checkers have tests of their own, fed histories that break each check,
and each mutant is run against them: `no-sync` fails checks 5 and 7 once a
power loss takes a message it claimed synced, before the counterparty read
it and before SQLite flushed it on its own; `drop-deliver`, taking every
delivery of an `:ok` send, fails 4 and 5; `redeliver`, delivering twice in
one process, fails 1.

## Nemeses

| Fault | How | Expected of fix2 |
|---|---|---|
| Kill | `kill -9` either node, both, then start them | Checks 1–9; redelivery bounded by 64 |
| Pause | `SIGSTOP`, `SIGCONT` either node, long enough for heartbeats to time out | Disconnects; checks 1–9 |
| Partition | `iptables` between `n1` and `n2` | Reconnects and resends after healing |
| Packet | netem delay, loss, duplication, reordering, rate between `n1` and `n2` | As partition; TCP hides duplication and reordering from fix2 |
| Power loss | Store on LazyFS; on kill, `lose-unfsynced-writes`, then start | Checks 5 and 7: nothing durable forgotten; redelivery unbounded, and a replay may be cut short |
| Disk full | Store on a small loop-mounted ext4, filled with `fallocate`, then freed | A failed commit fails its call once, then the store carries on: a `Send` fails, an uncertain `Append` halts, `SetNextTarget` disconnects until the disk is freed. A failed flush or checkpoint puts the store in doubt for good: the next `Append` or `Sync` halts the session, and a session only receiving stays disconnected instead (see [Found while planning](#found-while-planning)). The final phase restarts it either way. The run records which of these it went through |
| Hung disk | `SIGSTOP` the LazyFS process, `SIGCONT` it | A `Sync` is given up between 1× and 1.5× `WriteTimeout`, and the connection dropped once the lock is free; a write under the lock halts the session between 1× and 1.5× `StoreTimeout` after it began, `StoreTimeout` shortened in these runs only; the final phase resumes LazyFS, then restarts the node |
| Clock skew | Offset of `Config.Now` on one node, through `POST /clock` | A message received in sequence with a SendingTime beyond `MaxLatency` (shortened) rejected, counted as received, Logout, both ways; one ahead of a gap asked for again; a Logon refused; resends refused for a while once the clock goes back; timers unaffected |

Not available on Docker Desktop: `dm-delay`, `dm-flakey` (no targets in its
kernel), Jepsen's clock nemesis (one shared clock), Jepsen's bitflip (an
x86-64 binary). Torn writes need LazyFS 0.3.1; `jepsen.lazyfs` pins 0.3.0.

### `Config.Now`

Clock skew between two containers cannot come from the kernel, and Go reads
the clock through the vDSO, out of `libfaketime`'s reach. `session.Config`
gains `Now func() time.Time`, `time.Now` when nil, used for the wall clock
only:

- the SendingTime of every message written, and the SendingTime and
  OrigSendingTime of a GapFill; a resent application message keeps the
  OrigSendingTime it was stored with;
- the `MaxLatency` check of a SendingTime received, `Inbound.Received`, and
  the stash's receive times;
- the `CreatedAt` a reset stores (the first one is the store's own);
- `Schedule` windows, their start and end: a wall time ahead is turned into
  a duration for the timer that waits for it, on either of its branches.

Timeouts, heartbeats, read and write ages, write deadlines, `MaxPendingAge`,
probe intervals, retention ages and the store watchdog keep the real
monotonic clock: a skewed clock changes what the counterparty is told, not
how long the session waits. Two reads of `time.Now` serve both today and
are split: the one stamping a message sent also dates it for
`MaxPendingAge` (`session.go`), and the one taken when a frame arrives also
feeds the read timer (`protocol.go`). The tests use a `Now` without a
monotonic reading, so that a timer left on it shows.

## Running it

    make -C jepsen images      # control and node images, built locally
    make -C jepsen up          # control, n1, n2
    make -C jepsen check-env   # the node binary built, the environment checked
    make -C jepsen test NEMESIS=kill,partition TIME_LIMIT=300
    make -C jepsen web         # the results browser, on localhost:8080
    make -C jepsen down

Results go to `jepsen/store`, ignored by git.

## Increments

| # | Content | Done |
|---|---|---|
| J0 | This document; Docker Desktop checked for FUSE, netem, iptables, loop devices and device-mapper targets; LazyFS 0.3.0 checked to lose SQLite's unsynced commits and keep its synced ones | yes |
| J1 | `Config.Now`, with tests: SendingTime of messages, resends and GapFills written skewed, a resend's stored OrigSendingTime kept, `MaxLatency` against a skewed counterparty, a reset's `CreatedAt`, schedule windows opening and closing on the skewed clock, timers and retention ages unaffected | yes; each wall-clock read put back on `time.Now`, and each timer put on `Now`, fails a test; retention ages, which never read `Now`, have no test of their own |
| J2 | `jepsen/node`: both roles, SQLite and `memstore`, the store wrapper, control API with capped workers, drain and barrier, event logs, mutants; tested in-process against a fix2 counterparty | yes; two nodes over TCP on SQLite: orders both ways delivered once and in order, each synced before written; a restart carrying on; the HTTP API; the mutants; a store write held (send unresolved, then resolved; drain waiting for it) or in doubt (send `info`, session halted); the wrapper's optional interfaces; the digest across a resend. Not yet: a crash (J6) |
| J3 | `jepsen/docker`: control and node images (LazyFS prebuilt), compose file, setup of SSH keys, the binary built in a container; `jepsen/Makefile` | yes; `make check-env` on Docker Desktop (arm64): SSH to both nodes, the binary and the nemeses' tools there, LazyFS mounted as `jepsen.lazyfs` mounts it, losing an unsynced file and keeping a synced one. `jepsen.lazyfs/install!` fetches and rebuilds LazyFS: the suite calls `mount!` alone |
| J4 | The Clojure test: DB, client, generator, drain, barrier, log download; a baseline run without faults, live and green on checks 4 and 9 | yes; 20s and 30s runs on Docker Desktop, about a thousand orders each acknowledged and delivered before the closing marker; `--mutant drop-deliver` fails check 4. Check 4 does not yet excuse reported losses: they come with the faults (J5) |
| J5 | Checks 1–9, their tests on histories that break them, the mutants caught; perf and timeline | yes; `make -C jepsen unit`: a clean run valid, each check failed by a run made to break it (55 assertions). An order never delivered is excused only once the receiver processed past it (a later delivery, a SetNextTarget begun): by a recovery loss; by a Reject for its SendingTime, or a stale SendingTime with no Reject, only `:unknown`. On Docker Desktop, 30s clean; `redeliver` fails check 1 alone, `drop-deliver` checks 2, 4 and 5 alone (`make -C jepsen verdicts`). `no-sync` needs a power loss: J8 |
| J6 | Kill and pause nemeses | yes; `--nemesis kill,pause`, one node or both, a nemesis operation (fault, start or resume) every 3s on average. Valid on Docker Desktop: 3 minutes of kills (29 kills, 45 processes), 2 of pauses (19, disconnecting each side 6 times), 3 of both at 2000 orders/s with FromApp taking 2ms (34 faults, some 540 messages delivered again after restarts, spans up to 59 of the 64 allowed). At 50 orders/s nothing is delivered again: the queue is idle at each message, and the session records it at once; `--fromapp-delay` keeps it full |
| J7 | Partition and packet nemeses | yes; `--nemesis partition,packet`: `iptables` isolating the two nodes from each other, netem in both directions between them (delay, jitter, 20% and 60% loss, 10% corruption, duplication, reordering, 100 kbit/s, a mix), on `eth0` named: `jepsen.net` shapes the first interface `ip link` lists, a tunnel device on Docker Desktop, where the first packet runs shaped nothing. The network healed before the nodes start again, `eth0` back to `noqueue`. Valid on Docker Desktop: 2 minutes of partitions (18, ten disconnections each side, 199 retransmissions and 20 ResendRequests after them); 2 minutes of packet faults, acknowledgements taking a median 1.4 ms outside them and 200 ms to 3.2 s within (50 ms delays, 200 ms delays, 100 kbit/s, 60% loss), one disconnection each side; 3 minutes of all four faults at 2000 orders/s (80 messages delivered again, spans up to 58) |
| J8 | Power loss on LazyFS | yes; `--nemesis power-loss`: the store on LazyFS, mounted with `jepsen.lazyfs/mount!` alone, each kill losing what was not fsynced, the node started only once LazyFS's log says its cache is cleared (`jepsen.lazyfs` returns once the command is sent). Check 7 also holds the processed prefix an Open loads to the last one a Sync made durable (without power loss, committed). Valid on Docker Desktop over 2 minutes at 2000 orders/s: 20 kills losing 27 nodes' unsynced writes, stores reopened below what they had appended 8 times and below the processed prefix recorded twice, messages delivered again over spans up to 1147. `--mutant no-sync` fails checks 3, 4, 5, 7 and liveness. LazyFS 0.3.0 loses unsynced data and sizes, not files created, renamed or removed, which reach its backing directory at once: losing those, and torn writes, are not simulated |
| J9 | Disk full, first checking which store failure a full disk causes (SQLite reuses its write-ahead log in place), and driving both kinds; hung disk, first checking that stopping LazyFS hangs the store's I/O | yes; `--nemesis hang-disk` stops LazyFS (`StoreTimeout` 2s): the session halts by its watchdog 3s after; `--nemesis disk-full` fills a 64 MiB ext4 under the store: an `Append` failing "database or disk is full" halts, and a `SetNextTarget` whose failed commit cannot be read back leaves the store in doubt for good, both seen. Check 8 now also asserts a store write running past 1.5 `StoreTimeout` halts the session by then, and no Logon goes out after a halt. Releasing a disk restarts the halted nodes, the documented reconciliation, and the quiesce restarts every node whose disk was faulted; a kill while the disk hangs is left pending, completed once it is released; the filesystem has no reserved blocks, and a fill is checked to leave nothing. Valid on Docker Desktop: 90 s of hung disks (12 halts, 2.2 to 3.0 s after their write began), 90 s of full disks, 90 s of both with kills. A full disk fails few writes: SQLite's files grow rarely once the log is reused. Not yet: a session that only receives, to show the `SetNextTarget` wedge; `power-loss` with `hang-disk` (refused) |
| J10 | Clock skew through `Config.Now`. First, evidence the checkers lack: whether a Reject for SendingTime consumed its message (received in sequence) or not (ahead of a gap), and the session's own receive time rather than the observer's reading of `Now`; until then such Rejects excuse a loss as `:unknown` only | yes; fix2 gains `RejectObserver`, a `MessageObserver` told of each Reject the session sends of its own accord (not those a Handler's error asks for) and whether that rejection counted the message as received (`Seq` 0 if it had no valid MsgSeqNum); the node logs it, and a loss is excused by a counted Reject, not by one uncounted, `:unknown` only when a kill left no word: fewer notifications than Rejects written or read, or a process ending on a frame stale by any clock offset it had, at any time since the frame before. The receive time stays the observer's: the node logs each clock change before making it, and the session's reading lies between the frame before and the observer's, a pause in between perhaps, rather than fix2 handing the session's reading to observers. `--nemesis clock` sets a node's `Config.Now` ±3, 8 or 30 s against a `MaxLatency` of 5 s. Valid on Docker Desktop: 2 minutes of skew (12 skews, 18 Rejects, all counted); 2 minutes of skew, kills and partitions at 2000 orders/s (88 Rejects, 20 counted, 68 ahead of a gap and received again) |
| J11a | Variants needing no new check: `AckProbeEager`; a durability comparison without resets, `memstore` against SQLite under kills, `memstore` expected to fail checks 4, 5 and 7 | yes; `--ack-probe eager` and `--session-store memstore` (`:store` is Jepsen's). `AckProbeEager` valid under 90 s of kills and partitions at 2000 orders/s. `memstore` under 90 s of kills fails checks 3 and 7, and 2: a process started again forgets the processed prefix (7, 9 times) and reuses sequence numbers for other orders (3). Checks 2, 4 and 5 key on generation and sequence number, which memstore reuses: their verdicts do not hold there. Check 2's 28 skips include orders a later process sent, projected back onto an earlier one, and check 5 passed over a real loss (an order appended and written at sequence 18, never delivered, past the final marker's sequence 8); every acknowledged order was delivered, as check 4 says. Checks fit for memstore need the store lifetimes of J11d. Not `AckProbeNever`, the node's `--ack-probe never` aside: a send waits to be acknowledged, which only a probe tells, and every one ends `:info` |
| J11b | Retention on, `ErrBeyondRetention` reported | yes; `--retention-keep` and `--retention-max`, evicting every 500 ms, and `--last-processed`, the sessions sending LastMsgSeqNumProcessed (369): without it (or 789, not sent) retention learns of acknowledgements from ResendRequests only, and mostly `Max` evicts. Check 10: a loss told `ErrBeyondRetention` is of messages the sender's store was asked to truncate past, before the loss, failed or not, in any generation (`OnRecoveryLoss` gives none). A GapFill over messages truncated before it excuses their loss as such a report does: the callback is queued after the GapFill is written, and a kill may forestall it. 369 is the processed prefix, not the durable one: a receiver started again may ask for messages acknowledged and evicted, which `Keep` is the margin for. Valid on Docker Desktop, 90 s at 2000 orders/s each: power losses with Keep 16 and Max 512 (78 truncations, no loss told; with 369, 144 and none); partitions with Max 256 (none); partitions and kills with Keep 4 and Max 16 (109 truncations, 4 losses told, no send `ErrSkipped`; with 369, 104 and 2) |
| J11c | Several sessions per node pair, the checks keyed on the session ID | |
| J11d | Resets: checks keyed on logical epochs, mapped from each side's recorded resets across store lifetimes (`memstore` starts at generation 1 every time), with resets' abandoned messages accounted for; `OnRecoveryLoss` gives no generation, so a report queued before a reset and delivered after it needs one first (a field of the callback, or evidence from the store wrapper). Variants: `ResetOnDisconnect` mid-run, `memstore` with `ResetOnLogon` | |
| J12 | Results: long runs of each nemesis on Linux, recorded here, defects filed apart; a short run in CI; FIXP sessions considered | |

## Found while planning

- `AckProbeNever`'s comment says receipts are then "acknowledged only by
  sequence progress evidence, if AckEvidence allows it". No `AckEvidence`
  exists: under `AckProbeNever`, no receipt ever reaches `Acknowledged`.
- A session whose store is in doubt for good does not halt from
  `SetNextTarget`: `ErrUnknownOutcome` there only disconnects, as the store
  contract intends of a counter set again next time (`store.go`), and every
  connection after is refused while recording the processed messages is
  retried (`conn.go`). With the SQLite store in doubt after a failed flush,
  checkpoint or read-back, that retry never succeeds, and only an `Append`
  or a `Sync` would halt the session: one that only receives, a market data
  client, stays neither halted nor connected. `docs/operations.md` says it
  halts.
- Fixed (4575f8e), found writing J1's tests: `MaxPendingAge` was enforced
  only when the connection's timer fired for another reason, up to a
  heartbeat interval late, since queueing a message never armed it.
- `Receipt.Wait` is documented as bounded by its context, but takes the
  session's lock before looking at it, which a hung store write holds: it
  waits for the disk. `docs/operations.md` says so of "the other calls that
  take" the lock, without naming it.
