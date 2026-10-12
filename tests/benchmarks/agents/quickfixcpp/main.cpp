// quickfixcpp is the benchmark agent of QuickFIX C++ (quickfix/quickfix):
// the acceptor or the initiator of the scenarios, with QuickFIX C++'s
// defaults but for what the scenarios set. It implements what the Go agents
// share in internal/scenario, the README describes it: the same arguments,
// line protocol, messages and measurement. It builds and reads every
// message with the classes QuickFIX C++'s generator generates from the
// dictionary, in the flake's build of it, and dispatches those it receives
// with its generated FIX44::MessageCracker.

#include <quickfix/Application.h>
#include <quickfix/MessageStore.h>
#include <quickfix/Session.h>
#include <quickfix/SessionSettings.h>
#include <quickfix/SocketAcceptor.h>
#include <quickfix/SocketInitiator.h>
#include <quickfix/fix44/ExecutionReport.h>
#include <quickfix/fix44/MarketDataRequest.h>
#include <quickfix/fix44/MarketDataSnapshotFullRefresh.h>
#include <quickfix/fix44/MessageCracker.h>
#include <quickfix/fix44/NewOrderSingle.h>

#include <sys/resource.h>

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <deque>
#include <iostream>
#include <mutex>
#include <optional>
#include <sstream>
#include <stdexcept>
#include <string>
#include <thread>
#include <vector>

namespace {

// The session: FIX 4.4, the initiator INITIATOR logs on to the acceptor
// ACCEPTOR on 127.0.0.1, resetting sequence numbers.
const std::string BEGIN_STRING = "FIX.4.4";
const std::string INITIATOR = "INITIATOR";
const std::string ACCEPTOR = "ACCEPTOR";
const std::string HOST = "127.0.0.1";
const int HEART_BT_INT = 30;

// The scenarios and their messages: see internal/scenario.
const std::string ORDERS = "orders";
const std::string MARKET_DATA = "marketdata";

const std::string SYMBOL = "EURUSD";
const char SIDE = '1';
const double ORDER_QTY = 1000000;
const char ORD_TYPE = '2';
const double PRICE = 1.08255;
const char TIME_IN_FORCE = '0';
const char HANDL_INST = '1';
const char EXEC_TYPE_NEW = '0';
const char ORD_STATUS_NEW = '0';

// Party is an entry of the Parties group: PartyIDSource is a CHAR,
// PartyRole an INT, in QuickFIX C++'s generated code.
struct Party {
  std::string id;
  char source;
  int role;
};

const Party PARTIES[] = {{"BROKER-1", 'D', 1}, {"CLIENT-1", 'D', 3}, {"TRADER-1", 'D', 11}};
const int N_PARTIES = 3;

const char SUBSCRIPTION_SNAPSHOT = '0';
const int MARKET_DEPTH = 10;
const char ENTRY_TYPES[] = {'0', '1'}; // bids, offers

struct Level {
  double px, size;
};

// BOOK is the book the acceptor answers with, by entry type, best first.
const Level BOOK[2][MARKET_DEPTH] = {
    {{1.08249, 1000000}, {1.08248, 2000000}, {1.08247, 3000000}, {1.08246, 4000000}, {1.08245, 5000000},
     {1.08244, 6000000}, {1.08243, 7000000}, {1.08242, 8000000}, {1.08241, 9000000}, {1.08239, 10000000}},
    {{1.08251, 1000000}, {1.08252, 2000000}, {1.08253, 3000000}, {1.08254, 4000000}, {1.08255, 5000000},
     {1.08256, 6000000}, {1.08257, 7000000}, {1.08258, 8000000}, {1.08259, 9000000}, {1.08261, 10000000}},
};

// Params are an agent's arguments: see internal/scenario.Params.
struct Params {
  std::string role;
  int port = 0;
  std::string dict;
  std::string scenario = ORDERS;
  int messages = 0;
  int warmup = 0;
  int window = 1;
};

Params parse(int argc, char **argv) {
  if (argc < 2) {
    throw std::invalid_argument("usage: agent acceptor|initiator|version [flags]");
  }
  Params p;
  p.role = argv[1];
  for (int i = 2; i < argc; i++) {
    std::string name = argv[i], value;
    if (auto eq = name.find('='); eq != std::string::npos) {
      value = name.substr(eq + 1);
      name = name.substr(0, eq);
    } else if (i + 1 < argc) {
      value = argv[++i];
    } else {
      throw std::invalid_argument("flag needs an argument: " + name);
    }
    name.erase(0, name.find_first_not_of('-'));
    if (name == "port") {
      p.port = std::stoi(value);
    } else if (name == "dict") {
      p.dict = value;
    } else if (name == "scenario") {
      p.scenario = value;
    } else if (name == "messages") {
      p.messages = std::stoi(value);
    } else if (name == "warmup") {
      p.warmup = std::stoi(value);
    } else if (name == "window") {
      p.window = std::stoi(value);
    } else {
      throw std::invalid_argument("unknown flag " + name);
    }
  }
  if (p.role == "version") {
    return p;
  }
  if (p.role != "acceptor" && p.role != "initiator") {
    throw std::invalid_argument("unknown role " + p.role);
  }
  if (p.port <= 0) {
    throw std::invalid_argument("-port is required");
  }
  // QuickFIX C++ parses repeating groups only with a dictionary.
  if (p.dict.empty()) {
    throw std::invalid_argument("-dict is required");
  }
  if (p.role == "initiator" && (p.messages <= 0 || p.warmup < 0 || p.window <= 0)) {
    throw std::invalid_argument("-messages and -window must be > 0, -warmup >= 0");
  }
  if (p.scenario != ORDERS && p.scenario != MARKET_DATA) {
    throw std::invalid_argument("unknown scenario " + p.scenario);
  }
  return p;
}

// settings returns the session's settings: those of the scenario, the
// dictionary of p; the store (memory) and the log (none) are given to the
// connector.
FIX::SessionSettings settings(const Params &p, const std::string &sender, const std::string &target,
                              const std::vector<std::string> &extra) {
  std::ostringstream s;
  s << "[DEFAULT]\n"
    << "BeginString=" << BEGIN_STRING << "\n"
    << "HeartBtInt=" << HEART_BT_INT << "\n"
    << "ResetOnLogon=Y\n"
    // QuickFIX C++ requires a schedule: always on, as the other engines are
    // by default.
    << "NonStopSession=Y\n"
    // Off by default: Nagle's algorithm, which no other engine runs with.
    << "SocketNodelay=Y\n";
  for (const auto &l : extra) {
    s << l << "\n";
  }
  s << "[SESSION]\n"
    << "SenderCompID=" << sender << "\n"
    << "TargetCompID=" << target << "\n"
    << "DataDictionary=" << p.dict << "\n";
  std::istringstream in(s.str());
  return FIX::SessionSettings(in);
}

// -- the driver's line protocol -------------------------------------------

// Control reads the driver's commands, one per line on stdin.
class Control {
public:
  Control() {
    std::thread([this] {
      for (std::string l; std::getline(std::cin, l);) {
        std::lock_guard g(m_);
        lines_.push_back(l);
        cv_.notify_one();
      }
      // A read error ends the commands as the end of stdin does.
      std::lock_guard g(m_);
      eof_ = true;
      cv_.notify_one();
    }).detach();
  }

  // next returns the next command, nothing once stdin is closed.
  std::optional<std::string> next() {
    std::unique_lock g(m_);
    cv_.wait(g, [this] { return !lines_.empty() || eof_; });
    if (lines_.empty()) {
      return std::nullopt;
    }
    std::string l = lines_.front();
    lines_.pop_front();
    return l;
  }

private:
  std::mutex m_;
  std::condition_variable cv_;
  std::deque<std::string> lines_;
  bool eof_ = false;
};

// control returns the process's Control, never destroyed: its thread may
// still read stdin while an error unwinds.
Control &control() {
  static Control *c = new Control;
  return *c;
}

std::mutex sayMutex;

void say(const std::string &line) {
  std::lock_guard g(sayMutex);
  std::fwrite(line.data(), 1, line.size(), stdout);
  std::fputc('\n', stdout);
  std::fflush(stdout);
}

// cpuTime returns the user and system CPU time the process used, in ns,
// from getrusage(2) as the Go agents do.
int64_t cpuTime() {
  rusage ru{};
  if (getrusage(RUSAGE_SELF, &ru) != 0) {
    throw std::runtime_error("getrusage failed");
  }
  auto us = [](const timeval &tv) { return int64_t(tv.tv_sec) * 1000000 + tv.tv_usec; };
  return (us(ru.ru_utime) + us(ru.ru_stime)) * 1000;
}

void serveAcceptor(Control &ctl) {
  say("READY");
  while (auto cmd = ctl.next()) {
    if (*cmd != "USAGE") {
      throw std::invalid_argument("unknown command " + *cmd);
    }
    say("USAGE " + std::to_string(cpuTime()));
  }
}

// What invalidates a run: Rejects, sequence recovery, unexpected messages.
std::atomic<int64_t> anomalies{0};

struct AnomalyError : std::runtime_error {
  AnomalyError() : std::runtime_error("anomaly: the run is invalid") {}
};

// anomaly reports something invalidating the run: the agent will exit with
// status 3.
void anomaly(const std::string &message) {
  anomalies++;
  std::fprintf(stderr, "anomaly: %s\n", message.c_str());
}

// -- the initiator's measurement ------------------------------------------

// Semaphore is a counting semaphore whose waiters sleep until released, as
// a blocked goroutine or Java thread does: libc++'s std::counting_semaphore
// polls, spinning and sleeping, when acquired with a timeout.
class Semaphore {
public:
  explicit Semaphore(int64_t count) : count_(count) {}

  void release() {
    {
      std::lock_guard g(m_);
      count_++;
    }
    cv_.notify_one();
  }

  // acquire waits up to timeout for a unit, and tells whether it got one.
  bool acquire(std::chrono::milliseconds timeout) {
    std::unique_lock g(m_);
    if (!cv_.wait_for(g, timeout, [this] { return count_ > 0; })) {
      return false;
    }
    count_--;
    return true;
  }

private:
  std::mutex m_;
  std::condition_variable cv_;
  int64_t count_;
};

// Tracker runs and measures the initiator's side, as internal/scenario's
// does: it sends requests keeping at most window of them unanswered, and
// times each round trip. The handler calls ack with the identifier of each
// answer, its ClOrdID or MDReqID.
class Tracker {
public:
  explicit Tracker(const Params &p)
      : p_(p), start_(std::chrono::steady_clock::now()), sent_(p.warmup + p.messages), rtt_(p.messages),
        slots_(p.window) {}

  // ack tells order clOrdID is acknowledged.
  void ack(const std::string &clOrdID) {
    int64_t now = this->now();
    long i = -1;
    try {
      size_t end;
      i = std::stol(clOrdID, &end);
      if (end != clOrdID.size()) {
        i = -1;
      }
    } catch (const std::exception &) {
    }
    if (i < 0 || i >= long(sent_.size())) {
      fail("answer to unknown request " + clOrdID);
      return;
    }
    int64_t s = sent_[i].exchange(0);
    if (s == 0) {
      fail("answer to request " + std::to_string(i) + " not awaiting one");
      return;
    }
    if (long j = i - p_.warmup; j >= 0) {
      rtt_[j] = now - s;
    }
    for (int64_t e = end_.load(); now > e && !end_.compare_exchange_weak(e, now);) {
    }
    slots_.release();
    if (acked_.fetch_add(1) + 1 == target_.load()) {
      done_.release();
    }
  }

  // run sends the warm-up requests, tells the driver WARM, waits for its GO,
  // sends and measures the requests, and tells the driver its RESULT. send
  // sends the order of ClOrdID clOrdID.
  template <typename Send> void run(Control &ctl, Send send) {
    phase(0, p_.warmup, send);
    say("WARM");
    auto cmd = ctl.next();
    if (!cmd || *cmd != "GO") {
      throw std::runtime_error("expected GO, got " + cmd.value_or("the end of stdin"));
    }
    int64_t cpu = cpuTime();
    int64_t begin = now();
    phase(p_.warmup, p_.warmup + p_.messages, send);
    int64_t elapsed = end_.load() - begin;
    cpu = cpuTime() - cpu;
    std::string b = "RESULT {\"elapsed_ns\":" + std::to_string(elapsed) + ",\"cpu_ns\":" + std::to_string(cpu) +
                    ",\"rtt_ns\":[";
    b.reserve(b.size() + 8 * rtt_.size() + 2);
    for (size_t k = 0; k < rtt_.size(); k++) {
      if (k > 0) {
        b += ',';
      }
      b += std::to_string(rtt_[k]);
    }
    say(b + "]}");
  }

private:
  int64_t now() const {
    return std::chrono::duration_cast<std::chrono::nanoseconds>(std::chrono::steady_clock::now() - start_).count() +
           1;
  }

  // fail fails the run, even once measured: the agent exits with status 3.
  void fail(const std::string &message) {
    anomaly(message);
    failed_ = true;
  }

  void check() const {
    if (failed_ || anomalies.load() > 0) {
      throw AnomalyError();
    }
  }

  // phase sends requests from to to, and waits for their answers.
  template <typename Send> void phase(int from, int to, Send &send) {
    if (from == to) {
      return;
    }
    target_.store(to);
    for (int i = from; i < to; i++) {
      while (!slots_.acquire(std::chrono::milliseconds(10))) {
        check();
      }
      std::string id = std::to_string(i);
      sent_[i].store(now());
      if (!send(id)) {
        throw std::runtime_error("sending request " + std::to_string(i) + ": not logged on");
      }
    }
    while (!done_.acquire(std::chrono::milliseconds(10))) {
      check();
    }
    check();
  }

  const Params &p_;
  const std::chrono::steady_clock::time_point start_;
  std::vector<std::atomic<int64_t>> sent_; // when order i was sent, since start, +1; 0 once acknowledged
  std::vector<int64_t> rtt_;               // round trips of the measured requests
  Semaphore slots_;                        // one per order awaiting its acknowledgement
  Semaphore done_{0};
  std::atomic<int64_t> acked_{0};
  std::atomic<int64_t> target_{0}; // acked count ending the current phase
  std::atomic<int64_t> end_{0};    // the latest acknowledgement
  std::atomic<bool> failed_{false};
};

// -- the engine -----------------------------------------------------------

std::string msgType(const FIX::Message &m) {
  return m.getHeader().isSetField(35) ? m.getHeader().getField(35) : "";
}

// Watcher reports the Rejects, sequence recovery and unexpected messages.
class Watcher : public FIX::Application {
public:
  std::atomic<bool> stopping{false}; // the agent logs out: a logout is expected
  Semaphore logons{0};

  void onCreate(const FIX::SessionID &) override {}

  void onLogon(const FIX::SessionID &) override { logons.release(); }

  void onLogout(const FIX::SessionID &id) override {
    if (!stopping) {
      anomaly(id.toString() + " logged out");
    }
  }

  void toAdmin(FIX::Message &m, const FIX::SessionID &id) override {
    auto t = msgType(m);
    if (t == "3") {
      anomaly(id.toString() + " sent a Reject: " + m.toString());
    } else if (t == "2" || t == "4") {
      // On the loopback, recovering a gap means a message was lost.
      anomaly(id.toString() + " sent a sequence recovery: " + m.toString());
    }
  }

  void fromAdmin(const FIX::Message &m, const FIX::SessionID &id)
      EXCEPT(FIX::FieldNotFound, FIX::IncorrectDataFormat, FIX::IncorrectTagValue, FIX::RejectLogon) override {
    auto t = msgType(m);
    if (t == "3") {
      anomaly(id.toString() + " received a Reject: " + m.toString());
    } else if (t == "2" || t == "4") {
      anomaly(id.toString() + " received a sequence recovery: " + m.toString());
    }
  }

  // toApp runs for every message sent: it checks nothing, as the other
  // agents do not. The counterparty reports a BusinessMessageReject.
  void toApp(FIX::Message &, const FIX::SessionID &) EXCEPT(FIX::DoNotSend) override {}

protected:
  // dispatch cracks m to cracker's onMessage, reporting a message of a type
  // it does not handle. The cast is FIX::MessageCracker's: it does not copy
  // the message, as cracking a FIX::Message would.
  void dispatch(FIX44::MessageCracker &cracker, const FIX::Message &m, const FIX::SessionID &id) {
    try {
      cracker.crack(static_cast<const FIX44::Message &>(m), id);
    } catch (const FIX::UnsupportedMessageType &) {
      anomaly(id.toString() + " received " + m.toString());
      throw;
    }
  }
};

void send(FIX::Message &m, const FIX::SessionID &id, const std::string &what) {
  try {
    if (!FIX::Session::sendToTarget(m, id)) {
      anomaly(id.toString() + ": " + what + " not sent");
    }
  } catch (const std::exception &e) {
    anomaly(id.toString() + ": sending " + what + ": " + e.what());
  }
}

// Exchange answers each NewOrderSingle with an ExecutionReport, each
// MarketDataRequest with a MarketDataSnapshotFullRefresh. Its handlers run
// on the connector's one thread: the group entries it reuses are its own.
class Exchange : public Watcher, public FIX44::MessageCracker {
public:
  void onLogout(const FIX::SessionID &) override {}

  void fromApp(const FIX::Message &m, const FIX::SessionID &id)
      EXCEPT(FIX::FieldNotFound, FIX::IncorrectDataFormat, FIX::IncorrectTagValue,
             FIX::UnsupportedMessageType) override {
    dispatch(*this, m, id);
  }

  void onMessage(const FIX44::NewOrderSingle &m, const FIX::SessionID &id) override {
    FIX::ClOrdID clOrdID;
    FIX::Symbol symbol;
    FIX::Side side;
    FIX::OrderQty qty;
    FIX::Price px;
    m.get(clOrdID);
    m.get(symbol);
    m.get(side);
    m.get(qty);
    m.get(px);
    FIX::OrderID orderID(std::to_string(++n_));
    FIX44::ExecutionReport er(orderID, FIX::ExecID(orderID.getValue()), FIX::ExecType(EXEC_TYPE_NEW),
                              FIX::OrdStatus(ORD_STATUS_NEW), side, FIX::LeavesQty(qty.getValue()), FIX::CumQty(0),
                              FIX::AvgPx(0));
    er.set(clOrdID);
    er.set(FIX::OrderQty(qty.getValue()));
    er.set(FIX::Price(px.getValue()));
    er.set(symbol);
    er.set(FIX::TransactTime(3));
    FIX::PartyID pid;
    FIX::PartyIDSource src;
    FIX::PartyRole role;
    for (int i = 1, n = int(m.groupCount(FIX::FIELD::NoPartyIDs)); i <= n; i++) {
      m.getGroup(i, partyIn_);
      partyIn_.get(pid);
      partyIn_.get(src);
      partyIn_.get(role);
      partyOut_.set(pid);
      partyOut_.set(FIX::PartyIDSource(src.getValue()));
      partyOut_.set(FIX::PartyRole(role.getValue()));
      er.addGroup(partyOut_);
    }
    send(er, id, "ExecutionReport " + orderID.getValue());
  }

  void onMessage(const FIX44::MarketDataRequest &m, const FIX::SessionID &id) override {
    FIX::MDReqID reqID;
    FIX::SubscriptionRequestType sub;
    FIX::MarketDepth depth;
    m.get(reqID);
    m.get(sub);
    m.get(depth);
    int nTypes = int(m.groupCount(FIX::FIELD::NoMDEntryTypes));
    int nSyms = int(m.groupCount(FIX::FIELD::NoRelatedSym));
    const int levels = depth.getValue();
    if (sub.getValue() != SUBSCRIPTION_SNAPSHOT || levels != MARKET_DEPTH || nTypes != 2 || nSyms != 1) {
      anomaly("MarketDataRequest " + reqID.getValue() + ": unexpected request");
      return;
    }
    FIX::Symbol symbol;
    m.getGroup(1, sym_);
    sym_.get(symbol);
    FIX44::MarketDataSnapshotFullRefresh w;
    w.set(reqID);
    w.set(symbol);
    FIX::MDEntryType typ;
    for (int k = 0; k < nTypes; k++) {
      m.getGroup(k + 1, type_);
      type_.get(typ);
      if (typ.getValue() != ENTRY_TYPES[k]) {
        anomaly("MarketDataRequest " + reqID.getValue() + ": MDEntryType " + typ.getString());
      }
      for (int lvl = 0; lvl < levels; lvl++) {
        const Level &l = BOOK[k][lvl];
        entry_.set(typ);
        entry_.set(FIX::MDEntryPx(l.px));
        entry_.set(FIX::MDEntrySize(l.size));
        w.addGroup(entry_);
      }
    }
    send(w, id, "MarketDataSnapshotFullRefresh " + reqID.getValue());
  }

private:
  long n_ = 0;
  FIX44::NewOrderSingle::NoPartyIDs partyIn_;
  FIX44::ExecutionReport::NoPartyIDs partyOut_;
  FIX44::MarketDataRequest::NoMDEntryTypes type_;
  FIX44::MarketDataRequest::NoRelatedSym sym_;
  FIX44::MarketDataSnapshotFullRefresh::NoMDEntries entry_;
};

void acceptor(const Params &p) {
  Exchange x;
  FIX::MemoryStoreFactory store;
  auto st = settings(p, ACCEPTOR, INITIATOR, {"ConnectionType=acceptor", "SocketAcceptPort=" + std::to_string(p.port)});
  // No LogFactory: each session logs to a NullLog.
  FIX::SocketAcceptor a(x, store, st);
  a.start();
  Control &ctl = control();
  try {
    serveAcceptor(ctl);
  } catch (...) {
    a.stop();
    throw;
  }
  a.stop();
}

// Client reads the answers it receives and passes their identifiers to the
// tracker.
class Client : public Watcher, public FIX44::MessageCracker {
public:
  explicit Client(Tracker &tr) : tr_(tr) {}

  void fromApp(const FIX::Message &m, const FIX::SessionID &id)
      EXCEPT(FIX::FieldNotFound, FIX::IncorrectDataFormat, FIX::IncorrectTagValue,
             FIX::UnsupportedMessageType) override {
    dispatch(*this, m, id);
  }

  void onMessage(const FIX44::ExecutionReport &m, const FIX::SessionID &) override {
    FIX::ClOrdID clOrdID;
    m.get(clOrdID);
    int n = int(m.groupCount(FIX::FIELD::NoPartyIDs));
    if (n != N_PARTIES) {
      anomaly(std::to_string(n) + " Parties entries, want " + std::to_string(N_PARTIES));
    }
    FIX::PartyID pid;
    FIX::PartyIDSource src;
    FIX::PartyRole role;
    for (int i = 1; i <= n; i++) {
      m.getGroup(i, party_);
      party_.get(pid);
      party_.get(src);
      party_.get(role);
      if (i > N_PARTIES) {
        continue;
      }
      const Party &want = PARTIES[i - 1];
      if (pid.getValue() != want.id || src.getValue() != want.source || role.getValue() != want.role) {
        anomaly("Parties entry " + std::to_string(i - 1) + ": " + pid.getValue() + " " + src.getString() + " " +
                role.getString());
      }
    }
    tr_.ack(clOrdID.getValue());
  }

  void onMessage(const FIX44::MarketDataSnapshotFullRefresh &m, const FIX::SessionID &) override {
    FIX::MDReqID reqID;
    m.get(reqID);
    int n = int(m.groupCount(FIX::FIELD::NoMDEntries));
    if (n != 2 * MARKET_DEPTH) {
      anomaly(std::to_string(n) + " NoMDEntries entries, want " + std::to_string(2 * MARKET_DEPTH));
    }
    FIX::MDEntryType typ;
    FIX::MDEntryPx px;
    FIX::MDEntrySize size;
    for (int i = 1; i <= n; i++) {
      m.getGroup(i, entry_);
      entry_.get(typ);
      entry_.get(px);
      entry_.get(size);
      if (i > 2 * MARKET_DEPTH) {
        continue;
      }
      int k = (i - 1) / MARKET_DEPTH, lvl = (i - 1) % MARKET_DEPTH;
      const Level &l = BOOK[k][lvl];
      if (typ.getValue() != ENTRY_TYPES[k] || px.getValue() != l.px || size.getValue() != l.size) {
        anomaly("NoMDEntries entry " + std::to_string(i - 1) + ": " + typ.getString() + " " + size.getString() + "@" +
                px.getString());
      }
    }
    tr_.ack(reqID.getValue());
  }

private:
  Tracker &tr_;
  FIX44::ExecutionReport::NoPartyIDs party_;
  FIX44::MarketDataSnapshotFullRefresh::NoMDEntries entry_;
};

void initiator(const Params &p) {
  Tracker tr(p);
  Client c(tr);
  FIX::MemoryStoreFactory store;
  auto st = settings(p, INITIATOR, ACCEPTOR,
                     {"ConnectionType=initiator", "SocketConnectHost=" + HOST,
                      "SocketConnectPort=" + std::to_string(p.port)});
  // No LogFactory: each session logs to a NullLog.
  FIX::SocketInitiator i(c, store, st);
  i.start();
  auto stop = [&] {
    c.stopping = true;
    i.stop();
  };
  try {
    if (!c.logons.acquire(std::chrono::seconds(10))) {
      throw std::runtime_error("timed out waiting for the logon");
    }
    Control &ctl = control();
    FIX::SessionID id(BEGIN_STRING, INITIATOR, ACCEPTOR);
    if (p.scenario == ORDERS) {
      FIX44::NewOrderSingle::NoPartyIDs party;
      tr.run(ctl, [&](const std::string &clOrdID) {
        FIX44::NewOrderSingle nos(FIX::ClOrdID(clOrdID), FIX::Side(SIDE), FIX::TransactTime(3), FIX::OrdType(ORD_TYPE));
        nos.set(FIX::HandlInst(HANDL_INST));
        nos.set(FIX::OrderQty(ORDER_QTY));
        nos.set(FIX::Price(PRICE));
        nos.set(FIX::Symbol(SYMBOL));
        nos.set(FIX::TimeInForce(TIME_IN_FORCE));
        for (const Party &pt : PARTIES) {
          party.set(FIX::PartyID(pt.id));
          party.set(FIX::PartyIDSource(pt.source));
          party.set(FIX::PartyRole(pt.role));
          nos.addGroup(party);
        }
        return FIX::Session::sendToTarget(nos, id);
      });
    } else {
      FIX44::MarketDataRequest::NoMDEntryTypes type;
      FIX44::MarketDataRequest::NoRelatedSym sym;
      tr.run(ctl, [&](const std::string &reqID) {
        FIX44::MarketDataRequest req{FIX::MDReqID(reqID), FIX::SubscriptionRequestType(SUBSCRIPTION_SNAPSHOT),
                                     FIX::MarketDepth(MARKET_DEPTH)};
        for (char t : ENTRY_TYPES) {
          type.set(FIX::MDEntryType(t));
          req.addGroup(type);
        }
        sym.set(FIX::Symbol(SYMBOL));
        req.addGroup(sym);
        return FIX::Session::sendToTarget(req, id);
      });
    }
    // The driver closes stdin once it has the result: then log out.
    while (ctl.next()) {
    }
  } catch (...) {
    stop();
    throw;
  }
  stop();
}

} // namespace

// main exits with status 3 after an anomaly, 1 on error, 0 otherwise. It
// exits without running destructors: the connectors are stopped, and the
// control thread may still read stdin.
int main(int argc, char **argv) {
  bool failed = false;
  try {
    Params p = parse(argc, argv);
    if (p.role == "version") {
      std::printf("%s\n", QUICKFIX_CPP_VERSION);
      return 0;
    }
    if (p.role == "acceptor") {
      acceptor(p);
    } else {
      initiator(p);
    }
  } catch (const AnomalyError &) {
  } catch (const std::exception &e) {
    std::fprintf(stderr, "%s\n", e.what());
    failed = true;
  }
  std::fflush(stdout);
  if (failed) {
    std::_Exit(1);
  }
  if (int64_t n = anomalies.load(); n > 0) {
    std::fprintf(stderr, "%lld anomalies\n", static_cast<long long>(n));
    std::_Exit(3);
  }
  std::_Exit(0);
}
