// Agent is the benchmark agent of QuickFIX/J (org.quickfixj): the acceptor
// or the initiator of the scenarios, with QuickFIX/J's defaults but for what
// the scenarios set. It implements what the Go agents share in
// internal/scenario, the README describes it: the same arguments, line
// protocol, messages and measurement. It builds and reads every message with
// the classes quickfixj-codegenerator generates from the dictionary as the
// agent builds, and dispatches those it receives with their generated
// quickfix.fix44.MessageCracker; DefaultMessageFactory parses them into
// their generated classes.
package fix2bench;

import java.io.BufferedReader;
import java.io.ByteArrayInputStream;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.lang.foreign.Arena;
import java.lang.foreign.FunctionDescriptor;
import java.lang.foreign.Linker;
import java.lang.foreign.MemorySegment;
import java.lang.foreign.ValueLayout;
import java.lang.invoke.MethodHandle;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Properties;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.Semaphore;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicLongArray;
import quickfix.Application;
import quickfix.DefaultMessageFactory;
import quickfix.FieldNotFound;
import quickfix.IncorrectTagValue;
import quickfix.LogFactory;
import quickfix.MemoryStoreFactory;
import quickfix.Message;
import quickfix.Session;
import quickfix.SessionID;
import quickfix.SessionSettings;
import quickfix.SocketAcceptor;
import quickfix.SocketInitiator;
import quickfix.UnsupportedMessageType;
import quickfix.field.AvgPx;
import quickfix.field.ClOrdID;
import quickfix.field.CumQty;
import quickfix.field.ExecID;
import quickfix.field.ExecType;
import quickfix.field.HandlInst;
import quickfix.field.LeavesQty;
import quickfix.field.MDEntryPx;
import quickfix.field.MDEntrySize;
import quickfix.field.MDEntryType;
import quickfix.field.MDReqID;
import quickfix.field.MarketDepth;
import quickfix.field.NoMDEntries;
import quickfix.field.NoMDEntryTypes;
import quickfix.field.NoPartyIDs;
import quickfix.field.NoRelatedSym;
import quickfix.field.OrdStatus;
import quickfix.field.OrdType;
import quickfix.field.OrderID;
import quickfix.field.OrderQty;
import quickfix.field.PartyID;
import quickfix.field.PartyIDSource;
import quickfix.field.PartyRole;
import quickfix.field.Price;
import quickfix.field.Side;
import quickfix.field.SubscriptionRequestType;
import quickfix.field.Symbol;
import quickfix.field.TimeInForce;
import quickfix.field.TransactTime;
import quickfix.fix44.ExecutionReport;
import quickfix.fix44.MarketDataRequest;
import quickfix.fix44.MarketDataSnapshotFullRefresh;
import quickfix.fix44.MessageCracker;
import quickfix.fix44.NewOrderSingle;

public final class Agent {
    // The session: FIX 4.4, the initiator INITIATOR logs on to the acceptor
    // ACCEPTOR on 127.0.0.1, resetting sequence numbers.
    static final String BEGIN_STRING = "FIX.4.4";
    static final String INITIATOR = "INITIATOR";
    static final String ACCEPTOR = "ACCEPTOR";
    static final String HOST = "127.0.0.1";
    static final int HEART_BT_INT = 30;

    // The scenarios and their messages: see internal/scenario.
    static final String ORDERS = "orders";
    static final String MARKET_DATA = "marketdata";

    static final String SYMBOL = "EURUSD";
    static final char SIDE = '1';
    static final double ORDER_QTY = 1000000;
    static final char ORD_TYPE = '2';
    static final double PRICE = 1.08255;
    static final char TIME_IN_FORCE = '0';
    static final char HANDL_INST = '1';
    static final char EXEC_TYPE_NEW = '0';
    static final char ORD_STATUS_NEW = '0';

    /** An entry of the Parties group: PartyIDSource is a CHAR, PartyRole an INT, in QuickFIX/J's generated code. */
    record Party(String id, char source, int role) {}

    static final Party[] PARTIES = {new Party("BROKER-1", 'D', 1), new Party("CLIENT-1", 'D', 3), new Party("TRADER-1", 'D', 11)};

    static final char SUBSCRIPTION_SNAPSHOT = '0';
    static final int MARKET_DEPTH = 10;
    static final char[] ENTRY_TYPES = {'0', '1'}; // bids, offers

    /** The book the acceptor answers with, by entry type, best first: prices, then sizes. */
    static final double[][] BOOK_PX = {
        {1.08249, 1.08248, 1.08247, 1.08246, 1.08245, 1.08244, 1.08243, 1.08242, 1.08241, 1.08239},
        {1.08251, 1.08252, 1.08253, 1.08254, 1.08255, 1.08256, 1.08257, 1.08258, 1.08259, 1.08261},
    };
    static final double[] BOOK_SIZE = {1000000, 2000000, 3000000, 4000000, 5000000, 6000000, 7000000, 8000000, 9000000, 10000000};

    private Agent() {}

    public static void main(String[] args) {
        Exception err = null;
        try {
            Params p = Params.parse(args);
            switch (p.role) {
                case "version" -> {
                    System.out.println(version());
                    return;
                }
                case "acceptor" -> acceptor(p);
                case "initiator" -> initiator(p);
                default -> throw new IllegalArgumentException("unknown role " + p.role);
            }
        } catch (Exception e) {
            err = e;
        }
        exit(err);
    }

    /** An agent's arguments: see internal/scenario.Params. */
    static final class Params {
        String role;
        int port;
        String dict = "";
        String scenario = ORDERS;
        int messages;
        int warmup;
        int window = 1;

        static Params parse(String[] args) {
            if (args.length == 0) {
                throw new IllegalArgumentException("usage: agent acceptor|initiator|version [flags]");
            }
            Params p = new Params();
            p.role = args[0];
            for (int i = 1; i < args.length; i++) {
                String name = args[i];
                String value;
                int eq = name.indexOf('=');
                if (eq >= 0) {
                    value = name.substring(eq + 1);
                    name = name.substring(0, eq);
                } else if (i + 1 < args.length) {
                    value = args[++i];
                } else {
                    throw new IllegalArgumentException("flag needs an argument: " + name);
                }
                switch (name.replaceFirst("^--?", "")) {
                    case "port" -> p.port = Integer.parseInt(value);
                    case "dict" -> p.dict = value;
                    case "scenario" -> p.scenario = value;
                    case "messages" -> p.messages = Integer.parseInt(value);
                    case "warmup" -> p.warmup = Integer.parseInt(value);
                    case "window" -> p.window = Integer.parseInt(value);
                    default -> throw new IllegalArgumentException("unknown flag " + name);
                }
            }
            if (p.role.equals("version")) {
                return p;
            }
            if (p.port <= 0) {
                throw new IllegalArgumentException("-port is required");
            }
            // QuickFIX/J parses repeating groups only with a dictionary.
            if (p.dict.isEmpty()) {
                throw new IllegalArgumentException("-dict is required");
            }
            if (p.role.equals("initiator") && (p.messages <= 0 || p.warmup < 0 || p.window <= 0)) {
                throw new IllegalArgumentException("-messages and -window must be > 0, -warmup >= 0");
            }
            if (!p.scenario.equals(ORDERS) && !p.scenario.equals(MARKET_DATA)) {
                throw new IllegalArgumentException("unknown scenario " + p.scenario);
            }
            return p;
        }
    }

    static String version() throws Exception {
        Properties props = new Properties();
        try (InputStream in = Agent.class.getResourceAsStream("version.properties")) {
            props.load(in);
        }
        return props.getProperty("quickfixj.version", "unknown");
    }

    /**
     * settings returns the session's settings: those of the scenario, the
     * dictionary of p; the store (memory) and the log (none) are given to
     * the connector.
     */
    static SessionSettings settings(Params p, String sender, String target, String... extra) throws Exception {
        List<String> lines = new ArrayList<>(List.of(
                "[DEFAULT]",
                "BeginString=" + BEGIN_STRING,
                "HeartBtInt=" + HEART_BT_INT,
                "ResetOnLogon=Y",
                // QuickFIX/J requires a schedule: always on, as the other
                // engines are by default.
                "NonStopSession=Y"));
        lines.addAll(List.of(extra));
        lines.addAll(List.of("[SESSION]", "SenderCompID=" + sender, "TargetCompID=" + target));
        lines.add("DataDictionary=" + p.dict);
        byte[] b = (String.join("\n", lines) + "\n").getBytes(StandardCharsets.UTF_8);
        return new SessionSettings(new ByteArrayInputStream(b));
    }

    // -- the driver's line protocol -------------------------------------------

    /** Control reads the driver's commands, one per line on stdin. */
    static final class Control {
        private static final String EOF = new String("EOF");
        private final BlockingQueue<String> lines = new LinkedBlockingQueue<>();

        Control() {
            Thread t = new Thread(() -> {
                try (BufferedReader r = new BufferedReader(new InputStreamReader(System.in, StandardCharsets.UTF_8))) {
                    for (String l; (l = r.readLine()) != null; ) {
                        lines.add(l);
                    }
                } catch (Exception e) {
                    // A read error ends the commands as the end of stdin does.
                }
                lines.add(EOF);
            }, "control");
            t.setDaemon(true);
            t.start();
        }

        /** next returns the next command, null once stdin is closed. */
        String next() throws InterruptedException {
            String l = lines.take();
            if (l == EOF) {
                lines.add(EOF);
                return null;
            }
            return l;
        }
    }

    static synchronized void say(String line) {
        System.out.print(line + "\n");
        System.out.flush();
    }

    // getrusage(2), which the Go agents read their CPU time from. Not the
    // JVM's getProcessCpuTime: on Linux it reads times(2), in ticks of 10ms.
    private static final MethodHandle GETRUSAGE;

    static {
        Linker linker = Linker.nativeLinker();
        GETRUSAGE = linker.downcallHandle(linker.defaultLookup().find("getrusage").orElseThrow(),
                FunctionDescriptor.of(ValueLayout.JAVA_INT, ValueLayout.JAVA_INT, ValueLayout.ADDRESS));
    }

    /** cpuTime returns the user and system CPU time the process used, in ns. */
    static long cpuTime() {
        try (Arena arena = Arena.ofConfined()) {
            // struct rusage starts with ru_utime and ru_stime, struct timevals
            // of 16 bytes on LP64 Linux and macOS: a 64-bit tv_sec, then
            // tv_usec, below 1e6, whose low 32 bits come first on the
            // little-endian machines the benchmarks run on.
            MemorySegment ru = arena.allocate(256, 8);
            if ((int) GETRUSAGE.invokeExact(0 /* RUSAGE_SELF */, ru) != 0) {
                throw new IllegalStateException("failed");
            }
            long us = ru.get(ValueLayout.JAVA_LONG, 0) * 1_000_000 + ru.get(ValueLayout.JAVA_INT, 8)
                    + ru.get(ValueLayout.JAVA_LONG, 16) * 1_000_000 + ru.get(ValueLayout.JAVA_INT, 24);
            return us * 1000;
        } catch (Throwable t) {
            throw new IllegalStateException("getrusage: " + t, t);
        }
    }

    static void serveAcceptor(Control ctl) throws Exception {
        say("READY");
        for (String cmd; (cmd = ctl.next()) != null; ) {
            if (!cmd.equals("USAGE")) {
                throw new IllegalArgumentException("unknown command " + cmd);
            }
            say("USAGE " + cpuTime());
        }
    }

    // What invalidates a run: Rejects, sequence recovery, unexpected messages.
    static final AtomicLong anomalies = new AtomicLong();

    static final class AnomalyException extends Exception {
        AnomalyException() {
            super("anomaly: the run is invalid");
        }
    }

    /** anomaly reports something invalidating the run: the agent will exit with status 3. */
    static void anomaly(String message) {
        anomalies.incrementAndGet();
        System.err.println("anomaly: " + message);
    }

    /** exit exits with status 3 after an anomaly, 1 if err is set, 0 otherwise. */
    static void exit(Exception err) {
        if (err != null && !(err instanceof AnomalyException)) {
            System.err.println(err);
            System.exit(1);
        }
        long n = anomalies.get();
        if (n > 0) {
            System.err.println(n + " anomalies");
            System.exit(3);
        }
        System.exit(0);
    }

    // -- the initiator's measurement ------------------------------------------

    interface Sender {
        boolean send(String clOrdID) throws Exception;
    }

    /**
     * Tracker runs and measures the initiator's side, as internal/scenario's
     * does: it sends requests keeping at most window of them unanswered,
     * and times each round trip. The handler calls ack with the identifier
     * of each answer.
     */
    static final class Tracker {
        private final Params p;
        private final long start = System.nanoTime();
        private final AtomicLongArray sent; // when request i was sent, since start, +1; 0 once answered
        private final long[] rtt; // round trips of the measured requests
        private final Semaphore slots; // one per request awaiting its answer
        private final AtomicLong acked = new AtomicLong();
        private final AtomicLong end = new AtomicLong(); // the latest answer
        private volatile long target; // acked count ending the current phase
        private volatile CountDownLatch done;
        private volatile Exception err;

        Tracker(Params p) {
            this.p = p;
            sent = new AtomicLongArray(p.warmup + p.messages);
            rtt = new long[p.messages];
            slots = new Semaphore(p.window);
        }

        private long now() {
            return System.nanoTime() - start + 1;
        }

        /** fail fails the run, even once measured: the agent exits with status 3. */
        private void fail(String message) {
            anomaly(message);
            if (err == null) {
                err = new AnomalyException();
            }
        }

        /** ack tells request clOrdID (or MDReqID) is answered. */
        void ack(String clOrdID) {
            long now = now();
            int i;
            try {
                i = Integer.parseInt(clOrdID);
            } catch (NumberFormatException e) {
                i = -1;
            }
            if (i < 0 || i >= sent.length()) {
                fail("answer to unknown request " + clOrdID);
                return;
            }
            long s = sent.getAndSet(i, 0);
            if (s == 0) {
                fail("answer to request " + i + " not awaiting one");
                return;
            }
            int j = i - p.warmup;
            if (j >= 0) {
                rtt[j] = now - s;
            }
            end.accumulateAndGet(now, Math::max);
            slots.release();
            if (acked.incrementAndGet() == target) {
                done.countDown();
            }
        }

        private void check() throws Exception {
            if (err != null) {
                throw err;
            }
            if (anomalies.get() > 0) {
                throw new AnomalyException();
            }
        }

        /**
         * run sends the warm-up requests, tells the driver WARM, waits for its
         * GO, sends and measures the requests, and tells the driver its RESULT.
         */
        void run(Control ctl, Sender send) throws Exception {
            phase(0, p.warmup, send);
            say("WARM");
            String cmd = ctl.next();
            if (!"GO".equals(cmd)) {
                throw new IllegalStateException("expected GO, got " + cmd);
            }
            long cpu = cpuTime();
            long begin = now();
            phase(p.warmup, p.warmup + p.messages, send);
            long elapsed = end.get() - begin;
            cpu = cpuTime() - cpu;
            StringBuilder b = new StringBuilder(64 + 8 * rtt.length);
            b.append("RESULT {\"elapsed_ns\":").append(elapsed).append(",\"cpu_ns\":").append(cpu).append(",\"rtt_ns\":[");
            for (int k = 0; k < rtt.length; k++) {
                if (k > 0) {
                    b.append(',');
                }
                b.append(rtt[k]);
            }
            say(b.append("]}").toString());
        }

        /** phase sends requests from to to, and waits for their answers. */
        private void phase(int from, int to, Sender send) throws Exception {
            if (from == to) {
                return;
            }
            done = new CountDownLatch(1);
            target = to;
            for (int i = from; i < to; i++) {
                while (!slots.tryAcquire(10, TimeUnit.MILLISECONDS)) {
                    check();
                }
                String id = Integer.toString(i);
                sent.set(i, now());
                if (!send.send(id)) {
                    throw new IllegalStateException("sending request " + i + ": not logged on");
                }
            }
            while (!done.await(10, TimeUnit.MILLISECONDS)) {
                check();
            }
            check();
        }
    }

    // -- the engine ---------------------------------------------------------

    // No log: a null LogFactory gives each session a NullLog, where the
    // connectors' constructors without one default to a ScreenLogFactory,
    // logging every message to stdout.
    static final LogFactory NO_LOG = null;

    static String msgType(Message m) {
        try {
            return m.getHeader().getString(35);
        } catch (FieldNotFound e) {
            return "";
        }
    }

    /** Watcher reports the Rejects, sequence recovery and unexpected messages. */
    abstract static class Watcher implements Application {
        volatile boolean stopping; // the agent logs out: a logout is expected
        final CountDownLatch logons = new CountDownLatch(1);

        @Override
        public void onCreate(SessionID id) {}

        @Override
        public void onLogon(SessionID id) {
            logons.countDown();
        }

        @Override
        public void onLogout(SessionID id) {
            if (!stopping) {
                anomaly(id + " logged out");
            }
        }

        @Override
        public void toAdmin(Message m, SessionID id) {
            switch (msgType(m)) {
                case "3" -> anomaly(id + " sent a Reject: " + m);
                // On the loopback, recovering a gap means a message was lost.
                case "2", "4" -> anomaly(id + " sent a sequence recovery: " + m);
                default -> {}
            }
        }

        @Override
        public void fromAdmin(Message m, SessionID id) {
            switch (msgType(m)) {
                case "3" -> anomaly(id + " received a Reject: " + m);
                case "2", "4" -> anomaly(id + " received a sequence recovery: " + m);
                default -> {}
            }
        }

        // toApp runs for every message sent: it checks nothing, as the other
        // agents do not. The counterparty reports a BusinessMessageReject.
        @Override
        public void toApp(Message m, SessionID id) {}

        /** dispatch cracks m to cracker's onMessage, reporting a message of a type it does not handle. */
        static void dispatch(MessageCracker cracker, Message m, SessionID id)
                throws FieldNotFound, UnsupportedMessageType, IncorrectTagValue {
            try {
                cracker.crack(m, id);
            } catch (UnsupportedMessageType e) {
                anomaly(id + " received " + m);
                throw e;
            }
        }
    }

    /** send sends m, reporting a failure: what and ref name m, joined only then. */
    static void send(Message m, SessionID id, String what, String ref) {
        try {
            if (!Session.sendToTarget(m, id)) {
                anomaly(id + ": " + what + " " + ref + " not sent");
            }
        } catch (Exception e) {
            anomaly(id + ": sending " + what + " " + ref + ": " + e);
        }
    }

    /**
     * Exchange answers each NewOrderSingle with an ExecutionReport, each MarketDataRequest with a
     * MarketDataSnapshotFullRefresh. Its handlers run on the connector's one thread: the group entries it
     * reuses are its own.
     */
    static final class Exchange extends Watcher {
        private long n;
        private final NewOrderSingle.NoPartyIDs partyIn = new NewOrderSingle.NoPartyIDs();
        private final ExecutionReport.NoPartyIDs partyOut = new ExecutionReport.NoPartyIDs();
        private final MarketDataRequest.NoMDEntryTypes type = new MarketDataRequest.NoMDEntryTypes();
        private final MarketDataRequest.NoRelatedSym sym = new MarketDataRequest.NoRelatedSym();
        private final MarketDataSnapshotFullRefresh.NoMDEntries entry = new MarketDataSnapshotFullRefresh.NoMDEntries();

        private final MessageCracker cracker = new MessageCracker() {
            @Override
            public void onMessage(NewOrderSingle m, SessionID id) throws FieldNotFound {
                ClOrdID clOrdID = m.getClOrdID();
                Symbol symbol = m.getSymbol();
                Side side = m.getSide();
                OrderQty qty = m.getOrderQty();
                Price px = m.getPrice();
                OrderID orderID = new OrderID(Long.toString(++n));
                ExecutionReport er = new ExecutionReport(orderID, new ExecID(orderID.getValue()),
                        new ExecType(EXEC_TYPE_NEW), new OrdStatus(ORD_STATUS_NEW), side,
                        new LeavesQty(qty.getValue()), new CumQty(0), new AvgPx(0));
                er.set(clOrdID);
                er.set(qty);
                er.set(px);
                er.set(symbol);
                er.set(new TransactTime());
                for (int i = 1, count = m.getGroupCount(NoPartyIDs.FIELD); i <= count; i++) {
                    m.getGroup(i, partyIn);
                    partyOut.set(partyIn.getPartyID());
                    partyOut.set(partyIn.getPartyIDSource());
                    partyOut.set(partyIn.getPartyRole());
                    er.addGroup(partyOut);
                }
                send(er, id, "ExecutionReport", orderID.getValue());
            }

            @Override
            public void onMessage(MarketDataRequest m, SessionID id) throws FieldNotFound {
                MDReqID reqID = m.getMDReqID();
                SubscriptionRequestType sub = m.getSubscriptionRequestType();
                MarketDepth depth = m.getMarketDepth();
                int nTypes = m.getGroupCount(NoMDEntryTypes.FIELD);
                int nSyms = m.getGroupCount(NoRelatedSym.FIELD);
                if (sub.getValue() != SUBSCRIPTION_SNAPSHOT || depth.getValue() != MARKET_DEPTH
                        || nTypes != ENTRY_TYPES.length || nSyms != 1) {
                    anomaly("MarketDataRequest " + reqID.getValue() + ": unexpected request");
                    return;
                }
                m.getGroup(1, sym);
                MarketDataSnapshotFullRefresh w = new MarketDataSnapshotFullRefresh();
                w.set(reqID);
                w.set(sym.getSymbol());
                for (int k = 0; k < nTypes; k++) {
                    m.getGroup(k + 1, type);
                    MDEntryType typ = type.getMDEntryType();
                    if (typ.getValue() != ENTRY_TYPES[k]) {
                        anomaly("MarketDataRequest " + reqID.getValue() + ": MDEntryType " + typ.getValue());
                    }
                    for (int lvl = 0; lvl < depth.getValue(); lvl++) {
                        entry.set(typ);
                        entry.set(new MDEntryPx(BOOK_PX[k][lvl]));
                        entry.set(new MDEntrySize(BOOK_SIZE[lvl]));
                        w.addGroup(entry);
                    }
                }
                send(w, id, "MarketDataSnapshotFullRefresh", reqID.getValue());
            }
        };

        @Override
        public void onLogout(SessionID id) {}

        @Override
        public void fromApp(Message m, SessionID id) throws FieldNotFound, UnsupportedMessageType, IncorrectTagValue {
            dispatch(cracker, m, id);
        }
    }

    static void acceptor(Params p) throws Exception {
        Exchange x = new Exchange();
        SessionSettings st = settings(p, ACCEPTOR, INITIATOR,
                "ConnectionType=acceptor", "SocketAcceptAddress=" + HOST, "SocketAcceptPort=" + p.port);
        SocketAcceptor a = new SocketAcceptor(x, new MemoryStoreFactory(), st, NO_LOG, new DefaultMessageFactory());
        a.start();
        try {
            serveAcceptor(new Control());
        } finally {
            a.stop();
        }
    }

    /** Client reads the answers it receives and passes their identifiers to the tracker. */
    static final class Client extends Watcher {
        final Tracker tr;
        final String scenario;
        private final ExecutionReport.NoPartyIDs party = new ExecutionReport.NoPartyIDs();
        private final MarketDataSnapshotFullRefresh.NoMDEntries entry = new MarketDataSnapshotFullRefresh.NoMDEntries();

        private final MessageCracker cracker = new MessageCracker() {
            @Override
            public void onMessage(ExecutionReport m, SessionID id) throws FieldNotFound, UnsupportedMessageType {
                answers(ORDERS);
                String clOrdID = m.getClOrdID().getValue();
                char execType = m.getExecType().getValue();
                char ordStatus = m.getOrdStatus().getValue();
                char side = m.getSide().getValue();
                String symbol = m.getSymbol().getValue();
                double qty = m.getOrderQty().getValue();
                double px = m.getPrice().getValue();
                if (execType != EXEC_TYPE_NEW || ordStatus != ORD_STATUS_NEW || side != SIDE || !symbol.equals(SYMBOL)
                        || qty != ORDER_QTY || px != PRICE) {
                    anomaly("ExecutionReport: ExecType " + execType + ", OrdStatus " + ordStatus + ", Side " + side
                            + ", Symbol " + symbol + ", OrderQty " + qty + ", Price " + px);
                }
                int n = m.getGroupCount(NoPartyIDs.FIELD);
                if (n != PARTIES.length) {
                    anomaly(n + " Parties entries, want " + PARTIES.length);
                }
                for (int i = 1; i <= n; i++) {
                    m.getGroup(i, party);
                    String pid = party.getPartyID().getValue();
                    char src = party.getPartyIDSource().getValue();
                    int role = party.getPartyRole().getValue();
                    if (i <= PARTIES.length) {
                        Party want = PARTIES[i - 1];
                        if (!pid.equals(want.id()) || src != want.source() || role != want.role()) {
                            anomaly("Parties entry " + (i - 1) + ": " + pid + " " + src + " " + role + ", want " + want);
                        }
                    }
                }
                tr.ack(clOrdID);
            }

            @Override
            public void onMessage(MarketDataSnapshotFullRefresh m, SessionID id)
                    throws FieldNotFound, UnsupportedMessageType {
                answers(MARKET_DATA);
                String reqID = m.getMDReqID().getValue();
                String symbol = m.getSymbol().getValue();
                if (!symbol.equals(SYMBOL)) {
                    anomaly("MarketDataSnapshotFullRefresh: Symbol " + symbol);
                }
                int n = m.getGroupCount(NoMDEntries.FIELD);
                if (n != 2 * MARKET_DEPTH) {
                    anomaly(n + " NoMDEntries entries, want " + 2 * MARKET_DEPTH);
                }
                for (int i = 1; i <= n; i++) {
                    m.getGroup(i, entry);
                    char typ = entry.getMDEntryType().getValue();
                    double px = entry.getMDEntryPx().getValue();
                    double size = entry.getMDEntrySize().getValue();
                    if (i <= 2 * MARKET_DEPTH) {
                        int k = (i - 1) / MARKET_DEPTH, lvl = (i - 1) % MARKET_DEPTH;
                        if (typ != ENTRY_TYPES[k] || px != BOOK_PX[k][lvl] || size != BOOK_SIZE[lvl]) {
                            anomaly("NoMDEntries entry " + (i - 1) + ": " + typ + " " + size + "@" + px);
                        }
                    }
                }
                tr.ack(reqID);
            }
        };

        /**
         * answers reports an answer of scenario s other than the client's: a MarketDataSnapshotFullRefresh answers
         * no order, an ExecutionReport no MarketDataRequest.
         */
        void answers(String s) throws UnsupportedMessageType {
            if (!s.equals(scenario)) {
                anomaly("an answer of the " + s + " scenario in the " + scenario + " scenario");
                throw new UnsupportedMessageType();
            }
        }

        Client(Tracker tr, String scenario) {
            this.tr = tr;
            this.scenario = scenario;
        }

        @Override
        public void fromApp(Message m, SessionID id) throws FieldNotFound, UnsupportedMessageType, IncorrectTagValue {
            dispatch(cracker, m, id);
        }
    }

    static void initiator(Params p) throws Exception {
        Client c = new Client(new Tracker(p), p.scenario);
        SessionSettings st = settings(p, INITIATOR, ACCEPTOR,
                "ConnectionType=initiator", "SocketConnectHost=" + HOST, "SocketConnectPort=" + p.port);
        SocketInitiator i = new SocketInitiator(c, new MemoryStoreFactory(), st, NO_LOG, new DefaultMessageFactory());
        i.start();
        try {
            if (!c.logons.await(10, TimeUnit.SECONDS)) {
                throw new IllegalStateException("timed out waiting for the logon");
            }
            Control ctl = new Control();
            SessionID id = new SessionID(BEGIN_STRING, INITIATOR, ACCEPTOR);
            if (p.scenario.equals(ORDERS)) {
                NewOrderSingle.NoPartyIDs party = new NewOrderSingle.NoPartyIDs();
                c.tr.run(ctl, clOrdID -> {
                    // TransactTime's generated constructor writes milliseconds.
                    NewOrderSingle nos = new NewOrderSingle(new ClOrdID(clOrdID), new Side(SIDE), new TransactTime(),
                            new OrdType(ORD_TYPE));
                    nos.set(new HandlInst(HANDL_INST));
                    nos.set(new OrderQty(ORDER_QTY));
                    nos.set(new Price(PRICE));
                    nos.set(new Symbol(SYMBOL));
                    nos.set(new TimeInForce(TIME_IN_FORCE));
                    for (Party pt : PARTIES) {
                        party.set(new PartyID(pt.id()));
                        party.set(new PartyIDSource(pt.source()));
                        party.set(new PartyRole(pt.role()));
                        nos.addGroup(party);
                    }
                    return Session.sendToTarget(nos, id);
                });
            } else {
                MarketDataRequest.NoMDEntryTypes type = new MarketDataRequest.NoMDEntryTypes();
                MarketDataRequest.NoRelatedSym sym = new MarketDataRequest.NoRelatedSym();
                c.tr.run(ctl, reqID -> {
                    MarketDataRequest req = new MarketDataRequest(new MDReqID(reqID),
                            new SubscriptionRequestType(SUBSCRIPTION_SNAPSHOT), new MarketDepth(MARKET_DEPTH));
                    for (char t : ENTRY_TYPES) {
                        type.set(new MDEntryType(t));
                        req.addGroup(type);
                    }
                    sym.set(new Symbol(SYMBOL));
                    req.addGroup(sym);
                    return Session.sendToTarget(req, id);
                });
            }
            // The driver closes stdin once it has the result: then log out.
            while (ctl.next() != null) {
            }
        } finally {
            c.stopping = true;
            i.stop();
        }
    }
}
