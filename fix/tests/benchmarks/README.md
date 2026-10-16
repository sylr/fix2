# Benchmarks

Compares fix2's session engine with other FIX engines on two scenarios: an
initiator sends orders, which an acceptor acknowledges, or market data
requests, which it answers with its book. Each end is an *agent*, a
process of its own speaking a small line protocol with the driver,
`cmd/fixbench`, so that engines of any language can take part. Every agent
builds and reads its messages with the code its engine's own generator
generates from one dictionary, `spec/FIX44.xml`.

    nix develop                     # Go, the JDK, Maven, Gradle, CMake, QuickFIX C++
    make bench                      # the whole matrix, every run in results.txt
    make bench RUNS=20 WINDOWS=1,100 SCENARIOS=marketdata
    make bench AGENTS="fix2 quickfixj"          # AGENTS holds BASELINE, fix2
    make stats                      # benchstat: every pair against fix2/fix2

The engines: fix2, [QuickFIX/Go](https://github.com/quickfixgo/quickfix)
v0.9.12, [QuickFIX/J](https://github.com/quickfix-j/quickfixj) 3.0.2 and
[QuickFIX C++](https://github.com/quickfix/quickfix) 1.16.0.

This directory is a module of its own: QuickFIX/Go is a dependency of the
benchmarks, not of fix2, and `go test ./...` at the root leaves it out.

## Toolchains

`flake.nix` pins every agent's toolchain, `flake.lock` the nixpkgs they
come from: Go 1.27 (`GOTOOLCHAIN=local`: never one `go.mod` would
download), the JDK 25, the latest LTS, Maven and Gradle, CMake and the C++
compiler of nixpkgs' standard environment (Clang on macOS, GCC on Linux).
It builds QuickFIX C++ 1.16.0, its latest release, from its source (nixpkgs
has 1.15.1, of 2018), its code generated from `spec/FIX44.xml`: a static
library, CMake's Release build, which `QUICKFIX_CPP` points the shell to
(`nix build .#quickfix-cpp` builds it alone). `nix develop` enters a shell
with them; `nix develop -c make bench` runs the matrix in one. Nix only
sees the files the repository tracks: until `flake.nix`, `flake.lock` and
`spec/FIX44.xml` are committed or staged, use `nix develop path:.`; with
`flake.nix` alone, Nix locks nixpkgs afresh, and the toolchains with it.
Without Nix, any Go 1.27, JDK 25, Maven, CMake and QuickFIX C++ 1.16.0
generated from `spec/FIX44.xml` do, but results are only comparable when
built with the same.

## Generated code

`spec/FIX44.xml` is the FIX 4.4 dictionary QuickFIX/Go v0.9.12 ships, the
full one QuickFIX engines derive from. Each engine's generator generates
its code from it, which each agent builds and reads every message with, and
each engine validates every message it receives against it:

| | generator | generated | dispatch |
|---|---|---|---|
| fix2 | `fix2gen tagvalue -dispatch`, a Go tool of the module (`make generate`) | `gen/fix2/fix44`: builders, typed readers, group entries | `fix44.Dispatch` |
| QuickFIX/Go | its `cmd/generate-fix`, a Go tool of the module (`make generate`) | `gen/quickfixgo`: `fix44/<message>`, `field`, `enum`, `tag` | `quickfix.MessageRouter`, the generated `Route`s |
| QuickFIX/J | `quickfixj-codegenerator`, as the agent builds | `quickfix.fix44` and `quickfix.field`, as QuickFIX/J's published messages are generated (`decimal=false`) | the generated `quickfix.fix44.MessageCracker` |
| QuickFIX C++ | its Ruby generator and `MessageCracker.xsl`, in the flake's build of it | the library's `FIX44::` messages, fields and values | the generated `FIX44::MessageCracker` |

`gen/` is generated, not committed. QuickFIX/J's generation drops, as
QuickFIX/J's own build does, the session fields `quickfixj-base` ships, and
the CHAR values of two characters the dictionary defines (MiscFeeType 10
to 12, MassCancelRejectReason 99), which would not compile. QuickFIX C++'s
own FIX44.xml differs from this one (its components, some fields and
enumerations): its FIX 4.4 headers are those of this one.

Values go through each engine's generated types: float64 in fix2, decimals
in QuickFIX/Go (shopspring's, written with the scale they were read with),
doubles in QuickFIX/J and QuickFIX C++. Every engine writes the scenarios'
values the same: the book's prices end with no zero.

## Scenarios

FIX 4.4 over TCP on the loopback. The initiator `INITIATOR` logs on to the
acceptor `ACCEPTOR`, resetting sequence numbers, and sends requests, at
most *window* of them awaiting their answer; the acceptor answers each:

- **orders**: NewOrderSingle (35=D) messages with a Parties group of three
  entries. The acceptor reads the order's fields and its Parties, and
  answers with an ExecutionReport (35=8) acknowledging it (ExecType and
  OrdStatus New), echoing the ClOrdID, Symbol, Side, OrderQty, Price and
  the Parties. The initiator reads the ClOrdID, ExecType, OrdStatus,
  Side, Symbol, OrderQty, Price and the Parties: a New acknowledgement of
  the order it sent.
- **marketdata**: MarketDataRequest (35=V) messages for a snapshot of 10
  levels of the bids and offers of a symbol (two groups: NoMDEntryTypes,
  NoRelatedSym). The acceptor reads them, and answers with a
  MarketDataSnapshotFullRefresh (35=W) of 20 entries (NoMDEntries: type,
  price and size of each level). The initiator reads the MDReqID, the
  Symbol and every entry, which must be the book's.

Every field an agent reads must be there, and an answer must be of the
initiator's scenario: an ExecutionReport answers no MarketDataRequest.

A window of 1 measures latency: one request in flight at a time. Larger
windows measure throughput: the engines batch, queue and contend. Windows
go up to 10000: fix2's acceptor sends its answers from its callback, where
a Send fails rather than wait once 10000 messages, fix2's default, wait to
be written.

Each run starts both agents afresh; the initiator logs on, sends `-warmup`
requests, unmeasured, then the driver measures `-messages` requests. The
warm-up, the same for every engine, is long enough for the JVM to compile
QuickFIX/J's hot paths: on an Apple M4, QuickFIX/J's throughput at window
100 rose by a third from 10000 orders of warm-up to 50000, the default,
and no further at 100000.

- **requests/s**: the measured requests over the time from the first sent
  to the last answered;
- **round trips** (p50 to max): from before the request is built, building
  it through the engine's generated code being part of the engine's cost,
  to the moment its answer is read, for each measured request;
- **CPU/request**: the user and system CPU time of each process while the
  measured requests flow, per request; the acceptor's is read around the
  initiator's measurement, so it also covers the driver's few exchanges
  with the initiator, during which the acceptor idles;
- **max RSS**: of each process over its life, dictionary included; the
  JVM's follows its heap, sized by its defaults from the machine's memory,
  not by what QuickFIX/J needs.

The summary describes the runs: for each scenario and window, the median
of the runs of each pair, and the slowest and fastest runs' throughputs,
its ratios to the baseline those of medians, with no statistical test.
`-o` writes every run in the Go benchmark format (`BenchmarkOrders`,
`BenchmarkMarketData`) for benchstat to compare them: it gives a
confidence interval of a median from 6 runs, and tells whether two pairs
differ significantly. `-runs` is 10 by default. `make stats` runs it with
a row for each scenario and window and a column for each pair, the
baseline's first, which every other is compared to:

    go run golang.org/x/perf/cmd/benchstat@latest -row '.name,/window' \
        -col '/acceptor@(fix2 quickfixgo quickfixj quickfixcpp),/initiator@(fix2 quickfixgo quickfixj quickfixcpp)' \
        results.txt

Its columns follow the order given, not the shuffled order of the runs:
with benchstat's default rows, the full benchmark name, each acceptor
would have a row of its own, and the first pair to have run the baseline.

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
runs every scenario, window and pair once, the pairs in an order of their
own (`-seed`), so that drift over the session (thermal throttling, other
load) spreads over all of them rather than favouring the pairs run first.

## Microbenchmarks

A change to fix2's generated readers costs or saves a few hundred
nanoseconds a message, which round trips of tens of microseconds over TCP
hide. `micro/` measures them alone: the scenarios' messages, parsed once,
read through `gen/fix2/fix44` as the fix2 agent's handlers read them
(`BenchmarkRead`), their group entries without their fields
(`BenchmarkEntries`), snapshots of 2 to 200 entries (`BenchmarkReadSnapshot`).
`make micro` runs them 10 times (`MICRO_COUNT`), for benchstat to compare
two versions of fix2, each with the code its fix2gen generates.

## Fairness

Each engine runs with its defaults but for what the scenarios must set the
same:

| | fix2 | QuickFIX/Go | QuickFIX/J | QuickFIX C++ |
|---|---|---|---|---|
| store | `memstore` | `NewMemoryStoreFactory` | `MemoryStoreFactory` | `MemoryStoreFactory` |
| sent messages stored | all (default) | all (`PersistMessages=Y`, default) | all (`PersistMessages=Y`, default) | all (`PersistMessages=Y`, default) |
| logging | none (`Logger` nil) | `NewNullLogFactory` | a null `LogFactory` (`NullLog`), `slf4j-nop` | no `LogFactory` (`NullLog`) |
| validation | `Dictionaries.Session` from `-dict` | `DataDictionary=` `-dict` | `DataDictionary=` `-dict` | `DataDictionary=` `-dict` |
| SendingTime check | 120s (default) | 120s (default) | 120s (default) | 120s (default) |
| CheckSum of received messages | verified | not verified (it cannot) | verified (`ValidateChecksum=Y`, default) | verified (`ValidateLengthAndChecksum=Y`, default) |
| sequence reset | `ResetOnLogon` | `ResetOnLogon=Y` | `ResetOnLogon=Y` | `ResetOnLogon=Y` |
| heartbeat | `HeartBtInt` 30s | `HeartBtInt=30` | `HeartBtInt=30` | `HeartBtInt=30` |
| schedule | always (default) | always (default) | `NonStopSession=Y`, which it requires | `NonStopSession=Y`, which it requires |
| timestamps | milliseconds (default) | milliseconds (default) | milliseconds (default) | milliseconds (default) |
| TCP_NODELAY | on (Go's default) | on (Go's default) | on (`SocketTcpNoDelay`, default) | on: `SocketNodelay=Y`, off by default |
| runtime | Go 1.27's defaults | Go 1.27's defaults | JDK 25's defaults (G1, heap from memory) | CMake Release (`-O3 -DNDEBUG`) without nixpkgs' hardening, the system allocator |

QuickFIX C++'s TCP_NODELAY is the one default changed: off, Nagle's
algorithm holds back each small write until the previous one is
acknowledged, which, with delayed acknowledgements, would measure the TCP
stack rather than the engine at small windows; every other engine turns
Nagle's algorithm off (TCP_NODELAY on). Its acceptor listens on every
interface, having no setting to listen on one; the initiator connects over
the loopback all the same. QuickFIX/Go verifies no CheckSum: it has no
setting to, in v0.9.12 as in its main branch.

- **The same dictionary**: both ends validate every message they receive
  against `spec/FIX44.xml`, which their code is generated from; the
  QuickFIX engines also need it to parse repeating groups. A run in which
  any end sends or receives a Reject, a BusinessMessageReject, a
  ResendRequest, a SequenceReset or an unexpected message fails: on the
  loopback, recovering a gap means a message was lost, and the round trips
  would include the recovery.
- **The same messages**: every agent writes the same fields, with the same
  values: each message is as long on the wire whoever sends it (224 bytes
  of body for a NewOrderSingle, 251 for its ExecutionReport, 113 for a
  MarketDataRequest, 685 for its snapshot). The order of the body's fields
  is the generated code's: the QuickFIX engines write them in ascending
  tag order, each group at its counter's place; fix2 in the order the
  agent sets them, its groups last. Every engine writes the header in the
  same order, and each group entry's fields in the dictionary's.
- **Idiomatic use**: each agent uses its engine's generated code as its
  documentation does, and dispatches what it receives with the generated
  dispatcher. Each sets every field of every message through the generated
  setters, group entries field by field, values of their generated types:
  what it reads is parsed into them and formatted again when echoed. As
  their designs intend, fix2 reuses its builders (`Reset`) and their entry
  builders; the QuickFIX engines build each message object anew,
  QuickFIX/J and QuickFIX C++ reusing their group objects, which `addGroup`
  copies.
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
  QuickFIX C++ runs its `SocketAcceptor` and `SocketInitiator` too, not
  the threaded ones: one thread polls the sockets, reads up to `BUFSIZ`
  per wakeup (1 KiB on macOS, 8 KiB with glibc) and processes the
  messages; a `send` queues the message, polls the socket and writes the
  oldest queued, its own but for a backlog, with a write of its own, from
  the thread sending it.
- **Separate processes**: the two ends share no runtime, garbage collector
  or scheduler. The Go agents are built alike (`-trimpath`, no race
  detector), with the same toolchain; the C++ agent and QuickFIX C++ with
  the same compiler, as CMake's Release builds, without the hardening
  nixpkgs' compiler wrapper adds by default (stack protector, registers
  zeroed on return, fortify, libc++'s checks), which an upstream build has
  not; the JVM runs with its default options, but for its own output sent
  to stderr, stdout being the protocol's (`FIX2_BENCH_JAVA_OPTS` adds
  some, for experiments: one logging must log to stderr,
  `-Xlog:gc:stderr`).
- **The same measurement**: the initiator only timestamps (monotonic clock)
  and reports raw round trips; the driver computes every figure, for every
  engine alike. The Go agents share the measuring code
  (`internal/scenario`); the Java and C++ agents' are line for line
  translations, reading their CPU time from `getrusage(2)` as Go does: not
  from the JVM's `getProcessCpuTime`, which counts 10ms ticks on Linux.
  The C++ agent's waits sleep until woken as goroutines and Java threads
  do: libc++'s `std::counting_semaphore`, waited on with a timeout, polls,
  spinning and sleeping, which doubled the CPU time the C++ initiator was
  charged at window 1. The JVM's CPU time includes its compiler's and
  collector's threads, part of what running QuickFIX/J costs.

What the matrix does not control: both processes share the machine's cores
(no pinning; run on an otherwise idle machine, plugged in): one end's
spinning takes cores from the other, and on Apple silicon the system
chooses between performance and efficiency cores. For numbers to publish,
run on Linux, each agent pinned to cores of its own (`taskset`), at least
2 for a JVM: on one, it runs the serial collector rather than G1, with
fewer compiler threads. Go schedulers spin while waiting for work, so at
small windows the CPU time per request is mostly that spinning, close to
the round trip itself.

## Agents

An agent is an executable, `bin/<name>` for the Makefile, taking a role and
flags:

    <agent> version
    <agent> acceptor  -port P -dict FIX44.xml
    <agent> initiator -port P -dict FIX44.xml -scenario S -messages N -warmup K -window W

The acceptor answers the requests of every scenario. The agent reads
commands on stdin and answers on stdout, one line each; stderr is passed
through.

- `version` prints the engine's version and exits.
- The **acceptor** listens on 127.0.0.1:P, then prints `READY`. To `USAGE`
  it answers `USAGE <ns>`, the user and system CPU time the process used.
  When stdin closes, it stops and exits.
- The **initiator** connects to 127.0.0.1:P and logs on. It sends requests
  0 to K-1 of scenario S, `orders` or `marketdata`, identified by their
  number (ClOrdID, MDReqID), keeping at most W awaiting their answer,
  waits for all their answers, then prints `WARM` and waits for `GO`. It
  then sends requests K to K+N-1 alike and, once the last is answered,
  prints `RESULT {"elapsed_ns":…,"cpu_ns":…,"rtt_ns":[…]}`: the time from
  before sending request K to the last answer, the CPU time the process
  used meanwhile, and the N round trips in the order sent. When stdin
  closes, it logs out and exits.
- An agent exits with status 0, 1 on error, 3 if it saw a Reject, a
  BusinessMessageReject, a ResendRequest, a SequenceReset, an unexpected
  message or an answer other than the scenario's.
- The driver runs each agent in a process group of its own and kills the
  group on a timeout: an agent may be a script starting a JVM. Such a
  script must `exec` it, so that the CPU time and RSS the driver reads are
  the engine's.

The messages (`internal/scenario`), the fields an agent reads marked †:

| NewOrderSingle | ExecutionReport |
|---|---|
| 11 ClOrdID = the order's number † | 37 OrderID = the acceptor's count of orders, from 1 |
| 21 HandlInst = 1 | 17 ExecID = OrderID |
| 38 OrderQty = 1000000 † | 150 ExecType = 0 † |
| 40 OrdType = 2 | 39 OrdStatus = 0 † |
| 44 Price = 1.08255 † | 54 Side, echoed † |
| 54 Side = 1 † | 151 LeavesQty = OrderQty |
| 55 Symbol = EURUSD † | 14 CumQty = 0 |
| 59 TimeInForce = 0 | 6 AvgPx = 0 |
| 60 TransactTime = now, ms | 11 ClOrdID, echoed † |
| 453 NoPartyIDs = 3, each: 448 PartyID, 447 PartyIDSource, 452 PartyRole † | 38 OrderQty, 44 Price, 55 Symbol, echoed † |
| BROKER-1 D 1, CLIENT-1 D 3, TRADER-1 D 11 | 60 TransactTime = now, ms |
| | 453 NoPartyIDs, echoed † |

| MarketDataRequest | MarketDataSnapshotFullRefresh |
|---|---|
| 262 MDReqID = the request's number † | 262 MDReqID, echoed † |
| 263 SubscriptionRequestType = 0 † | 55 Symbol, echoed † |
| 264 MarketDepth = 10 † | 268 NoMDEntries = 20, each: 269 MDEntryType, 270 MDEntryPx, 271 MDEntrySize † |
| 267 NoMDEntryTypes = 2: 269 MDEntryType 0, 1 † | bids 1.08249 to 1.08241 and 1.08239, |
| 146 NoRelatedSym = 1: 55 Symbol EURUSD † | offers 1.08251 to 1.08259 and 1.08261, |
| | of 1000000 to 10000000 each side |

To add an engine, write its agent under `agents/<name>` to these rules,
with an in-memory store, no logging, TCP_NODELAY and the code its
generator generates from `spec/FIX44.xml`, build it to `bin/<name>`, and
pass `-impl <name>=bin/<name>`. The Makefile builds and passes every
directory of `agents/`: one holding Go files with `go build`, one holding a
`pom.xml` with Maven, which must build `target/<name>-agent.jar`, run by
`agents/<name>/<name>.sh` (see `agents/quickfixj`), one holding a
`CMakeLists.txt` with CMake, with the shell's compiler, in a directory of
`agents/<name>/build` of its own for each compiler and QuickFIX C++, so
that no CMake cache outlives its toolchain, which must build the
executable `<name>` (see `agents/quickfixcpp`). Write to stdout only the
protocol's lines: an engine logging there, as QuickFIX/J's connectors do by
default, fails the run.
