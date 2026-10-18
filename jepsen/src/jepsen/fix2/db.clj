(ns jepsen.fix2.db
  "Runs fix2-jepsen-node on the nodes: the first node the initiator, the
  second the acceptor."
  (:require [clojure.string :as str]
            [clojure.tools.logging :refer [info warn]]
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

(def disk-image "A small filesystem for the store, to fill." "/var/lib/fix2/disk.img")

(def disk-size "Its size." "64M")

(defn- disk-dir
  "Where the small filesystem is mounted: the store's directory, or
  LazyFS's backing directory when the store is on LazyFS."
  [test]
  (if (:lazyfs test) (:data-dir lazy) data-dir))

(def filler "Fills the small filesystem." "filler")

(defn mount-disk!
  "Mounts a fresh small ext4 filesystem where the store goes."
  [test]
  (c/su
    (c/exec :mkdir :-p (disk-dir test))
    (c/exec :fallocate :-l disk-size disk-image)
    ;; No blocks reserved: the node runs as root, which could use them.
    (c/exec :mkfs.ext4 :-q :-F :-m 0 disk-image)
    (c/exec :mount :-o :loop disk-image (disk-dir test))))

(defn umount-disk!
  []
  (c/su
    (doseq [d [(:data-dir lazy) data-dir]]
      (c/exec :sh :-c (str "! mountpoint -q " d " || umount -l " d)))
    (c/exec :rm :-f disk-image)))

(defn fill-disk!
  "Fills the small filesystem, and checks nothing is left."
  []
  (c/su
    (let [dir (if (cu/exists? (str (:data-dir lazy))) (:data-dir lazy) data-dir)
          avail #(parse-long (str/trim (c/exec :sh :-c (str "df --output=avail -B1 " dir " | tail -1"))))]
      ;; All but a MiB at once (ext4 needs room for the file's own
      ;; extents), then the rest written until the disk says it is full.
      (c/exec :fallocate :-l (max 0 (- (avail) 1048576)) (str dir "/" filler))
      (c/exec :sh :-c (str "dd if=/dev/zero of=" dir "/" filler "2 bs=4096 2>/dev/null; true"))
      (let [left (avail)]
        (when (< 65536 left)
          (throw (ex-info "the store's filesystem is not full" {:dir dir :available left})))
        :filled))))

(defn free-disk!
  []
  (c/su (c/exec :rm :-f (str (:data-dir lazy) "/" filler) (str data-dir "/" filler)
                (str (:data-dir lazy) "/" filler "2") (str data-dir "/" filler "2")))
  :freed)

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

(def pending-kills
  "Nodes killed while their disk hung, their process to die once released."
  (atom #{}))

(declare kill!)

(defn complete-kill!
  "Once node's disk is released: waits for a pending kill's process to die,
  then starts the node again. True if one was pending."
  [test node]
  (when (contains? @pending-kills node)
    (kill! test node)
    (swap! pending-kills disj node)
    (start-new! test node)
    true))

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
        :-store (if (= :memstore (:session-store test)) "memstore" (str "sqlite:" data-dir "/fix.db"))
        :-ack-probe (name (:ack-probe test :lazy))
        :-sessions (:fix-sessions test 1)
        :-retention-interval (if (pos? (+ (:retention-keep test 0) (:retention-max test 0))) "500ms" "0s")
        :-retention-keep (:retention-keep test 0)
        :-retention-max (:retention-max test 0)
        (str "-last-processed=" (boolean (:last-processed test)))
        (str "-reset-on-logon=" (= :logon (:reset test)))
        (str "-reset-on-disconnect=" (= :disconnect (:reset test)))
        :-events events-dir
        :-incarnation incarnation
        :-heartbeat (str (:heartbeat test 1) "s")
        :-mutant (or (:mutant test) "")
        :-fromapp-delay (str (:fromapp-delay test 0) "ms")
        :-store-timeout (str (:store-timeout test 0) "ms")
        :-max-latency (str (:max-latency test 120) "s")))
    (cu/await-tcp-port http-port {:timeout 10000})))

(defn kill!
  "Kills the node with SIGKILL, and waits for its process to be gone: a
  start! would find it running. On LazyFS, what the store did not fsync is
  then lost: a power loss."
  [test node]
  (let [dead? (c/su
                (cu/grepkill! :kill "fix2-jepsen-node")
                (loop [i 0]
                  (cond (not (cu/daemon-running? pid-file)) true
                        (< i 100) (do (Thread/sleep 100) (recur (inc i)))
                        ;; Waiting on a stopped LazyFS, it dies once the
                        ;; disk is released.
                        (contains? (:faults test) :hang-disk) false
                        :else (throw (ex-info "fix2-jepsen-node survived SIGKILL" {:node node})))))]
    (cond
      (not dead?) (do (warn node "killed while its disk hangs: dies once released")
                      (swap! pending-kills conj node)
                      :kill-pending)
      ;; Writes are lost under a dead process only.
      (:power-loss test) (do (lose-unfsynced-writes! node) :killed)
      :else :killed)))

(defrecord DB []
  db/DB
  (setup! [this test node]
    (c/su
      (c/exec :mkdir :-p dir data-dir events-dir)
      (c/upload local-bin bin)
      (c/exec :chmod :+x bin))
    (when (:disk-full test)
      (mount-disk! test))
    (when (:lazyfs test)
      (lazyfs/mount! lazy))
    (start! test node))

  (teardown! [this test node]
    (kill! (dissoc test :power-loss) node)
    (c/su (cu/grepkill! :cont "lazyfs")) ; a hung disk left stopped
    ;; LazyFS, then the small filesystem, which may be its backing
    ;; directory: jepsen.lazyfs/umount! would remove that while mounted.
    (c/su (c/exec :sh :-c (str "! mountpoint -q " data-dir " || fusermount -uz " data-dir)))
    (umount-disk!)
    (c/su (c/exec :rm :-rf dir "/var/lib/fix2")))

  db/Kill
  (kill! [this test node] (kill! test node))
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
