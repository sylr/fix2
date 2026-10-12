# Benchmarks

Compares fix2's session engine with other FIX engines on one scenario: an
initiator sends orders, an acceptor acknowledges each. Each end is an
*agent*, a process of its own speaking a small line protocol with the
driver, `cmd/fixbench`, so that engines of any language can take part.

    nix develop                     # Go, the JDK, Maven and Gradle
    make bench                      # the whole matrix, every run in results.txt
    make bench RUNS=10 WINDOWS=1,100 MESSAGES=100000
    make bench AGENTS="fix2 quickfixj"          # AGENTS holds BASELINE, fix2
    go run golang.org/x/perf/cmd/benchstat@latest -col /initiator results.txt

The engines: fix2, [QuickFIX/Go](https://github.com/quickfixgo/quickfix)
v0.9.12 and [QuickFIX/J](https://github.com/quickfix-j/quickfixj) 3.0.2.

This directory is a module of its own: QuickFIX/Go is a dependency of the
benchmarks, not of fix2, and `go test ./...` at the root leaves it out.

## Toolchains

`flake.nix` pins every agent's toolchain, `flake.lock` the nixpkgs they
come from: Go 1.27 (`GOTOOLCHAIN=local`: never one `go.mod` would
download), the JDK 25, the latest LTS, Maven and Gradle. `nix develop`
enters a shell with them; `nix develop -c make bench` runs the matrix in
one. Nix only sees the files the repository tracks: until `flake.nix` and
`flake.lock` are committed or staged, use `nix develop path:.`; with
`flake.nix` alone, Nix locks nixpkgs afresh, and the toolchains with it. Without Nix, any Go 1.27,
JDK 25 and Maven do, but results are only comparable when built with the
same.

## Scenario

FIX 4.4 over TCP on the loopback. The initiator `INITIATOR` logs on to the
acceptor `ACCEPTOR`, resetting sequence numbers, and sends NewOrderSingle
(35=D) messages, at most *window* of them awaiting their acknowledgement.
The acceptor answers each with an ExecutionReport (35=8) acknowledging it
(ExecType and OrdStatus New), echoing the order's ClOrdID, Symbol, Side,
OrderQty and Price.

A window of 1 measures latency: one order in flight at a time. Larger
windows measure throughput: the engines batch, queue and contend. Windows
go up to 10000: fix2's acceptor sends its ExecutionReports from its
callback, where a Send fails rather than wait once 10000 messages,
fix2's default, wait to be written.

Each run starts both agents afresh; the initiator logs on, sends `-warmup`
orders, unmeasured, then the driver measures `-messages` orders. The
warm-up, the same for every engine, is long enough for the JVM to compile
QuickFIX/J's hot paths: on an Apple M4, QuickFIX/J's throughput at window
100 rises by a third from 10000 orders of warm-up to 50000, the default,
and no further at 100000.

- **orders/s**: the measured orders over the time from the first sent to
  the last acknowledged;
- **round trips** (p50 to max): from before the order is built, building
  it through the engine's API being part of the engine's cost, to the
  moment its ExecutionReport is delivered to the application, for each
  measured order;
- **CPU/order**: the user and system CPU time of each process while the
  measured orders flow, per order; the acceptor's is read around the
  initiator's measurement, so it also covers the driver's few exchanges
  with the initiator, during which the acceptor idles;
- **max RSS**: of each process over its life, dictionary included; the
  JVM's follows its heap, sized by its defaults from the machine's memory,
  not by what QuickFIX/J needs.

The summary gives the median of the runs of each pair, and the slowest and
fastest runs' throughputs; `-o` writes every run in the Go benchmark format
for benchstat to compare with confidence intervals.

## Matrix

The baseline, fix2, against itself, then for each other implementation X:

| acceptor | initiator |
|---|---|
| fix2 | fix2 |
| X | X |
| fix2 | X |
| X | fix2 |

`X/X` against `fix2/fix2` compares the engines; the mixed pairs tell which
end makes the difference, and that the engines interoperate. Each round
runs every window and pair once, the pairs in an order of their own
(`-seed`), so that drift over the session (thermal throttling, other load)
spreads over all of them rather than favouring the pairs run first.

## Fairness

Each engine runs with its defaults but for what the scenario must set the
same:

| | fix2 | QuickFIX/Go | QuickFIX/J |
|---|---|---|---|
| store | `memstore` | `NewMemoryStoreFactory` | `MemoryStoreFactory` |
| sent messages stored | all (default) | all (`PersistMessages=Y`, default) | all (`PersistMessages=Y`, default) |
| logging | none (`Logger` nil) | `NewNullLogFactory` | a null `LogFactory` (`NullLog`), `slf4j-nop` |
| validation | `Dictionaries.Session` from `-dict` | `DataDictionary=` `-dict` | `DataDictionary=` `-dict` |
| without `-dict` | checks nothing (default) | `ValidateFieldsHaveValues=N`, `ValidateFieldsOutOfOrder=N` | `UseDataDictionary=N` |
| SendingTime check | 120s (default) | 120s (default) | 120s (default) |
| CheckSum of received messages | verified | not verified (default) | verified (`ValidateChecksum=Y`, default) |
| sequence reset | `ResetOnLogon` | `ResetOnLogon=Y` | `ResetOnLogon=Y` |
| heartbeat | `HeartBtInt` 30s | `HeartBtInt=30` | `HeartBtInt=30` |
| schedule | always (default) | always (default) | `NonStopSession=Y`, which it requires |
| timestamps | milliseconds (default) | milliseconds (default) | milliseconds (default) |
| TCP_NODELAY | on (Go's default) | on (Go's default) | on (`SocketTcpNoDelay`, default) |
| runtime | Go 1.27's defaults | Go 1.27's defaults | JDK 25's defaults (G1, heap from memory) |

- **The same dictionary**: both ends validate every message they receive
  against the same file, by default QuickFIX/Go's `spec/FIX44.xml`, the
  full FIX 4.4 dictionary QuickFIX engines ship. `-validate=false` runs
  without, QuickFIX/Go then told to skip the checks it runs even without a
  dictionary. A run in which any end sends or receives a Reject, a
  BusinessMessageReject, a ResendRequest, a SequenceReset or an unexpected
  message fails: on the loopback, recovering a gap means a message was
  lost, and the round trips would include the recovery.
- **The same messages**: every agent writes the same fields, with the same
  values, in ascending tag order as QuickFIX engines do, so that the
  messages on the wire are byte for byte the same whoever sends them, but
  for their timestamps and checksums: the three engines write the header
  in the same order too.
- **Idiomatic use**: each agent uses its engine as its documentation does,
  through the raw tag API rather than generated message types: fix2 reuses
  a `Builder` and sends with `Session.Send`; QuickFIX/Go builds a `Message`
  and sends with `SendToTarget`, from `FromApp` too; QuickFIX/J builds a
  `quickfix.Message` and sends with `Session.sendToTarget`. QuickFIX/J's
  generated messages (`quickfixj-messages-fix44`), which QuickFIX/J 3
  leaves to applications to add, are left out: its `DefaultMessageFactory`
  then parses every message into a `quickfix.Message`, as the Go agents
  read raw fields.
- **Engine design left as is**: what each engine does by design is what is
  measured, and not tuned: fix2 writes all that is queued in one write and
  delivers messages on a goroutine of their own; QuickFIX/Go writes each
  message with a write of its own, and hands each message read to its
  session goroutine through a channel of capacity 1 (`InChanCapacity`);
  while a write is under way, its session goroutine keeps waking itself to
  send what is queued. QuickFIX/J runs its `SocketAcceptor` and
  `SocketInitiator`, the connectors of its examples: Apache MINA's threads
  read and write, one thread processes every session's messages, handed
  over through a queue of 10000 that holds back MINA's reader when full.
- **Separate processes**: the two ends share no runtime, garbage collector
  or scheduler. The Go agents are built alike (`-trimpath`, no race
  detector), with the same toolchain; the JVM runs with its default
  options, but for its own output sent to stderr, stdout being the
  protocol's (`FIX2_BENCH_JAVA_OPTS` adds some, for experiments: one
  logging must log to stderr, `-Xlog:gc:stderr`).
- **The same measurement**: the initiator only timestamps (monotonic clock)
  and reports raw round trips; the driver computes every figure, for every
  engine alike. The Go agents share the measuring code
  (`internal/scenario`); the Java agent's is a line for line translation,
  reading its CPU time from `getrusage(2)` as Go does, not from the JVM's
  `getProcessCpuTime`, which counts 10ms ticks on Linux. The JVM's CPU
  time includes its compiler's and collector's threads, part of what
  running QuickFIX/J costs.

What the matrix does not control: both processes share the machine's cores
(no pinning; run on an otherwise idle machine, plugged in): one end's
spinning takes cores from the other, and on Apple silicon the system
chooses between performance and efficiency cores. For numbers to publish,
run on Linux, each agent pinned to cores of its own (`taskset`), at least
2 for a JVM: on one, it runs the serial collector rather than G1, with
fewer compiler threads. Go
schedulers spin while waiting for work, so at small windows the CPU time
per order is mostly that spinning, close to the round trip itself.

## Agents

An agent is an executable, `bin/<name>` for the Makefile, taking a role and
flags:

    <agent> version
    <agent> acceptor  -port P [-dict FIX44.xml]
    <agent> initiator -port P [-dict FIX44.xml] -messages N -warmup K -window W

Without `-dict`, the agent validates nothing a dictionary would. The agent
reads commands on stdin and answers on stdout, one line each; stderr is
passed through.

- `version` prints the engine's version and exits.
- The **acceptor** listens on 127.0.0.1:P, then prints `READY`. To `USAGE`
  it answers `USAGE <ns>`, the user and system CPU time the process used.
  When stdin closes, it stops and exits.
- The **initiator** connects to 127.0.0.1:P and logs on. It sends orders
  of ClOrdID 0 to K-1, keeping at most W awaiting acknowledgement, waits
  for all their ExecutionReports, then prints `WARM` and waits for `GO`.
  It then sends orders K to K+N-1 alike and, once the last is
  acknowledged, prints `RESULT {"elapsed_ns":…,"cpu_ns":…,"rtt_ns":[…]}`:
  the time from before sending order K to the last acknowledgement, the
  CPU time the process used meanwhile, and the N round trips in ClOrdID
  order. When stdin closes, it logs out and exits.
- An agent exits with status 0, 1 on error, 3 if it saw a Reject, a
  BusinessMessageReject, a ResendRequest, a SequenceReset or an unexpected
  message.
- The driver runs each agent in a process group of its own and kills the
  group on a timeout: an agent may be a script starting a JVM. Such a
  script must `exec` it, so that the CPU time and RSS the driver reads are
  the engine's.

The messages, in ascending tag order (`internal/scenario`):

| NewOrderSingle | ExecutionReport |
|---|---|
| 11 ClOrdID = the order's number | 6 AvgPx = 0 |
| 21 HandlInst = 1 | 11 ClOrdID, echoed |
| 38 OrderQty = 1000000 | 14 CumQty = 0 |
| 40 OrdType = 2 | 17 ExecID = the acceptor's count of orders, from 1 |
| 44 Price = 1.08255 | 37 OrderID = ExecID |
| 54 Side = 1 | 38 OrderQty, echoed |
| 55 Symbol = EURUSD | 39 OrdStatus = 0 |
| 59 TimeInForce = 0 | 44 Price, echoed |
| 60 TransactTime = now, ms | 54 Side, echoed |
| | 55 Symbol, echoed |
| | 60 TransactTime = now, ms |
| | 150 ExecType = 0 |
| | 151 LeavesQty = OrderQty |

To add an engine, write its agent under `agents/<name>` to these rules,
with an in-memory store, no logging and TCP_NODELAY, build it to
`bin/<name>`, and pass `-impl <name>=bin/<name>`. The Makefile builds and
passes every directory of `agents/`: one holding Go files with `go build`,
one holding a `pom.xml` with Maven, which must build
`target/<name>-agent.jar`, run by `agents/<name>/<name>.sh` (see
`agents/quickfixj`). Write to stdout only the protocol's lines: an engine
logging there, as QuickFIX/J's connectors do by default, fails the run.
