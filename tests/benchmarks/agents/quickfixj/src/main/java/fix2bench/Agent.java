// Agent is the benchmark agent of QuickFIX/J (org.quickfixj): the acceptor
// or the initiator of the scenario, with QuickFIX/J's defaults but for what
// the scenario sets. It implements what the Go agents share in
// internal/scenario, the README describes it: the same arguments, line
// protocol, messages and measurement.
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
import java.time.LocalDateTime;
import java.time.ZoneOffset;
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
import quickfix.LogFactory;
import quickfix.MemoryStoreFactory;
import quickfix.Message;
import quickfix.Session;
import quickfix.SessionID;
import quickfix.SessionSettings;
import quickfix.SocketAcceptor;
import quickfix.SocketInitiator;
import quickfix.UnsupportedMessageType;
import quickfix.UtcTimestampPrecision;

public final class Agent {
    // The session: FIX 4.4, the initiator INITIATOR logs on to the acceptor
    // ACCEPTOR on 127.0.0.1, resetting sequence numbers.
    static final String BEGIN_STRING = "FIX.4.4";
    static final String INITIATOR = "INITIATOR";
    static final String ACCEPTOR = "ACCEPTOR";
    static final String HOST = "127.0.0.1";
    static final int HEART_BT_INT = 30;

    // The NewOrderSingle the initiator sends and the ExecutionReport the
    // acceptor answers it with: see internal/scenario.
    static final String SYMBOL = "EURUSD";
    static final String SIDE = "1";
    static final String ORDER_QTY = "1000000";
    static final String ORD_TYPE = "2";
    static final String PRICE = "1.08255";
    static final String TIME_IN_FORCE = "0";
    static final String HANDL_INST = "1";
    static final String EXEC_TYPE_NEW = "0";
    static final String ORD_STATUS_NEW = "0";
    static final String ZERO = "0";

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
            if (p.role.equals("initiator") && (p.messages <= 0 || p.warmup < 0 || p.window <= 0)) {
                throw new IllegalArgumentException("-messages and -window must be > 0, -warmup >= 0");
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
     * dictionary of p if any; the store (memory) and the log (none) are
     * given to the connector.
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
        if (p.dict.isEmpty()) {
            lines.add("UseDataDictionary=N");
        } else {
            lines.add("DataDictionary=" + p.dict);
        }
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
     * does: it sends orders keeping at most window of them unacknowledged,
     * and times each round trip. The handler calls ack with the ClOrdID of
     * each ExecutionReport.
     */
    static final class Tracker {
        private final Params p;
        private final long start = System.nanoTime();
        private final AtomicLongArray sent; // when order i was sent, since start, +1; 0 once acknowledged
        private final long[] rtt; // round trips of the measured orders
        private final Semaphore slots; // one per order awaiting its acknowledgement
        private final AtomicLong acked = new AtomicLong();
        private final AtomicLong end = new AtomicLong(); // the latest acknowledgement
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

        /** ack tells order clOrdID is acknowledged. */
        void ack(String clOrdID) {
            long now = now();
            int i;
            try {
                i = Integer.parseInt(clOrdID);
            } catch (NumberFormatException e) {
                i = -1;
            }
            if (i < 0 || i >= sent.length()) {
                fail("ExecutionReport for unknown ClOrdID " + clOrdID);
                return;
            }
            long s = sent.getAndSet(i, 0);
            if (s == 0) {
                fail("ExecutionReport for ClOrdID " + i + " not awaiting one");
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
         * run sends the warm-up orders, tells the driver WARM, waits for its
         * GO, sends and measures the orders, and tells the driver its RESULT.
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

        /** phase sends orders from to to, and waits for their acknowledgements. */
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
                sent.set(i, now());
                if (!send.send(Integer.toString(i))) {
                    throw new IllegalStateException("sending order " + i + ": not logged on");
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

        UnsupportedMessageType unexpected(Message m, SessionID id) {
            anomaly(id + " received " + m);
            return new UnsupportedMessageType();
        }
    }

    /** Exchange acknowledges each NewOrderSingle with an ExecutionReport. */
    static final class Exchange extends Watcher {
        private long n; // fromApp runs on the connector's one thread

        @Override
        public void onLogout(SessionID id) {}

        @Override
        public void fromApp(Message m, SessionID id) throws FieldNotFound, UnsupportedMessageType {
            if (!msgType(m).equals("D")) {
                throw unexpected(m, id);
            }
            String orderID = Long.toString(++n);
            String qty = m.getString(38);
            Message er = new Message();
            er.getHeader().setString(35, "8");
            er.setString(6, ZERO);
            er.setString(11, m.getString(11));
            er.setString(14, ZERO);
            er.setString(17, orderID);
            er.setString(37, orderID);
            er.setString(38, qty);
            er.setString(39, ORD_STATUS_NEW);
            er.setString(44, m.getString(44));
            er.setString(54, m.getString(54));
            er.setString(55, m.getString(55));
            er.setUtcTimeStamp(60, LocalDateTime.now(ZoneOffset.UTC), UtcTimestampPrecision.MILLIS);
            er.setString(150, EXEC_TYPE_NEW);
            er.setString(151, qty);
            try {
                if (!Session.sendToTarget(er, id)) {
                    anomaly(id + ": ExecutionReport " + orderID + " not sent");
                }
            } catch (Exception e) {
                anomaly(id + ": sending ExecutionReport " + orderID + ": " + e);
            }
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

    /** Client passes the ExecutionReports it receives to the tracker. */
    static final class Client extends Watcher {
        final Tracker tr;

        Client(Tracker tr) {
            this.tr = tr;
        }

        @Override
        public void fromApp(Message m, SessionID id) throws FieldNotFound, UnsupportedMessageType {
            if (!msgType(m).equals("8")) {
                throw unexpected(m, id);
            }
            tr.ack(m.getString(11));
        }
    }

    static void initiator(Params p) throws Exception {
        Client c = new Client(new Tracker(p));
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
            c.tr.run(ctl, clOrdID -> {
                Message nos = new Message();
                nos.getHeader().setString(35, "D");
                nos.setString(11, clOrdID);
                nos.setString(21, HANDL_INST);
                nos.setString(38, ORDER_QTY);
                nos.setString(40, ORD_TYPE);
                nos.setString(44, PRICE);
                nos.setString(54, SIDE);
                nos.setString(55, SYMBOL);
                nos.setString(59, TIME_IN_FORCE);
                nos.setUtcTimeStamp(60, LocalDateTime.now(ZoneOffset.UTC), UtcTimestampPrecision.MILLIS);
                return Session.sendToTarget(nos, id);
            });
            // The driver closes stdin once it has the result: then log out.
            while (ctl.next() != null) {
            }
        } finally {
            c.stopping = true;
            i.stop();
        }
    }
}
