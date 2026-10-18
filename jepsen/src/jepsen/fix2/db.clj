(ns jepsen.fix2.db
  "Runs fix2-jepsen-node on the nodes: the first node the initiator, the
  second the acceptor."
  (:require [clojure.string :as str]
            [clojure.tools.logging :refer [info]]
            [jepsen [control :as c]
                    [db :as db]]
            [jepsen.control.util :as cu]))

(def dir "Where the binary and its log live." "/opt/fix2")
(def bin (str dir "/fix2-jepsen-node"))
(def log-file (str dir "/node.log"))
(def pid-file (str dir "/node.pid"))
(def data-dir "The store's directory." "/var/lib/fix2/store")
(def events-dir "The event logs' directory, on a disk no nemesis touches." "/var/lib/fix2/events")
(def fix-port 9876)
(def http-port 8080)

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
  start! would find it running."
  [test node]
  (c/su
    (cu/grepkill! :kill "fix2-jepsen-node")
    (loop [i 0]
      (when (cu/daemon-running? pid-file)
        (if (< i 100)
          (do (Thread/sleep 100) (recur (inc i)))
          (throw (ex-info "fix2-jepsen-node survived SIGKILL" {:node node})))))))

(defrecord DB []
  db/DB
  (setup! [this test node]
    (c/su
      (c/exec :mkdir :-p dir data-dir events-dir)
      (c/upload local-bin bin)
      (c/exec :chmod :+x bin))
    (start! test node))

  (teardown! [this test node]
    (kill! test node)
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
             (into {} (map (fn [f] [f (str "events/" (last (str/split f #"/")))]) events))
             (into {} (map (fn [f] [f (str "store/" (last (str/split f #"/")))]) store))))))

(defn db [] (DB.))
