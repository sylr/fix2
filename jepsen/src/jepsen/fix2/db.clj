(ns jepsen.fix2.db
  "Runs fix2-jepsen-node on the nodes: the first node the initiator, the
  second the acceptor."
  (:require [clojure.string :as str]
            [clojure.tools.logging :refer [info]]
            [jepsen [control :as c]
                    [db :as db]
                    [lazyfs :as lazyfs]]
            [jepsen.control.util :as cu]))

(def dir "Where the binary and its log live." "/opt/fix2")
(def bin (str dir "/fix2-jepsen-node"))
(def log-file (str dir "/node.log"))
(def pid-file (str dir "/node.pid"))
(def data-dir "The store's directory." "/var/lib/fix2/store")
(def events-dir "The event logs' directory, on a disk no nemesis touches." "/var/lib/fix2/events")
(def fix-port 9876)
(def http-port 8080)

(def lazy
  "The store's directory on LazyFS, when the test has :lazyfs: what was not
  fsynced is lost when a node is killed, as in a power loss. LazyFS is
  built into the node image; jepsen.lazyfs/install! would fetch and
  rebuild it, so only mount! is called.

  LazyFS 0.3.0 drops the data and sizes it cached and did not fsync, not
  files created, renamed or removed meanwhile, which reach its backing
  directory at once: a power loss losing those is not simulated."
  (lazyfs/lazyfs {:dir data-dir :cache-size "0.5GB"}))

(defn- caches-cleared
  "How many times LazyFS's log says it cleared its cache."
  []
  (c/su (parse-long (str/trim (c/exec :sh :-c (str "grep -c 'cache is cleared' " (:log-file lazy) " || true"))))))

(defn- lose-unfsynced-writes!
  "Asks LazyFS to drop what was not fsynced, and waits for its log to say
  it did: jepsen.lazyfs's returns once the command is written, and a node
  started meanwhile would open its store before the loss."
  [node]
  (let [before (caches-cleared)]
    (lazyfs/lose-unfsynced-writes! lazy)
    (loop [i 0]
      (when (<= (caches-cleared) before)
        (if (< i 200)
          (do (Thread/sleep 50) (recur (inc i)))
          (throw (ex-info "LazyFS did not clear its cache" {:node node})))))))

(def local-bin
  "The binary on the control node, built by `make node-binary`."
  "bin/fix2-jepsen-node")

(defn role
  "The node's role: the first node initiates, the second accepts."
  [test node]
  (if (= node (first (:nodes test))) :initiator :acceptor))

(defn acceptor [test] (second (:nodes test)))

(def incarnations
  "Counts the processes started, to name their event logs."
  (atom 0))

(declare start-new!)

(defn start!
  "Starts the node, with a new incarnation, unless it is running: the
  nemesis starts all nodes, those it did not kill too."
  [test node]
  (if (c/su (cu/daemon-running? pid-file))
    :already-running
    (start-new! test node)))

(defn- start-new!
  [test node]
  (let [incarnation (str node "-" (swap! incarnations inc))]
    (info "Starting fix2-jepsen-node" incarnation)
    (c/su
      (cu/start-daemon!
        {:logfile log-file :pidfile pid-file :chdir dir}
        bin
        :-role (name (role test node))
        :-listen (str ":" fix-port)
        :-connect (str (acceptor test) ":" fix-port)
        :-http (str ":" http-port)
        :-store (str "sqlite:" data-dir "/fix.db")
        :-events events-dir
        :-incarnation incarnation
        :-heartbeat (str (:heartbeat test 1) "s")
        :-mutant (or (:mutant test) "")
        :-fromapp-delay (str (:fromapp-delay test 0) "ms")))
    (cu/await-tcp-port http-port {:timeout 10000})))

(defn kill!
  "Kills the node with SIGKILL, and waits for its process to be gone: a
  start! would find it running. On LazyFS, what the store did not fsync is
  then lost: a power loss."
  [test node]
  (c/su
    (cu/grepkill! :kill "fix2-jepsen-node")
    (loop [i 0]
      (when (cu/daemon-running? pid-file)
        (if (< i 100)
          (do (Thread/sleep 100) (recur (inc i)))
          (throw (ex-info "fix2-jepsen-node survived SIGKILL" {:node node}))))))
  (when (:lazyfs test)
    (lose-unfsynced-writes! node)))

(defrecord DB []
  db/DB
  (setup! [this test node]
    (c/su
      (c/exec :mkdir :-p dir data-dir events-dir)
      (c/upload local-bin bin)
      (c/exec :chmod :+x bin))
    (when (:lazyfs test)
      (lazyfs/mount! lazy))
    (start! test node))

  (teardown! [this test node]
    (kill! (dissoc test :lazyfs) node)
    (when (lazyfs/mounted? lazy) ; by this test or one before
      (lazyfs/umount! lazy))
    (c/su (c/exec :rm :-rf dir "/var/lib/fix2")))

  db/Kill
  (kill! [this test node] (kill! test node) :killed)
  (start! [this test node] (start! test node) :started)

  db/Pause
  (pause! [this test node] (c/su (cu/grepkill! :stop "fix2-jepsen-node")) :paused)
  (resume! [this test node] (c/su (cu/grepkill! :cont "fix2-jepsen-node")) :resumed)

  db/LogFiles
  (log-files [this test node]
    (let [events (try (cu/ls-full events-dir) (catch Exception _ []))
          store  (try (cu/ls-full data-dir) (catch Exception _ []))]
      (merge {log-file "node.log"}
             (when (:lazyfs test) {(:log-file lazy) "lazyfs.log"})
             (into {} (map (fn [f] [f (str "events/" (last (str/split f #"/")))]) events))
             (into {} (map (fn [f] [f (str "store/" (last (str/split f #"/")))]) store))))))

(defn db [] (DB.))
