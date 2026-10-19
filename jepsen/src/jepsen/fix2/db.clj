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

(def dm-name
  "The device-mapper device the store's filesystem sits on, with :dm-disk:
  its table linear, or delaying the disk's writes, or failing them."
  "fix2-store")

(def dm-dev (str "/dev/mapper/" dm-name))

(def loop-record "The loop device under dm-name." "/var/lib/fix2/disk.loop")

(defn mount-disk!
  "Mounts a fresh small ext4 filesystem where the store goes: on the image
  looped, or, with :dm-disk, on dm-name over it."
  [test]
  (c/su
    (c/exec :mkdir :-p (disk-dir test))
    (c/exec :fallocate :-l disk-size disk-image)
    (if (:dm-disk test)
      (let [_ (c/exec :modprobe :-a :dm_delay :dm_flakey)
            loop-dev (str/trim (c/exec :losetup :-f :--show disk-image))
            sectors (str/trim (c/exec :blockdev :--getsz loop-dev))]
        (c/exec :echo loop-dev :> loop-record)
        (c/exec :dmsetup :create dm-name :--table (str "0 " sectors " linear " loop-dev " 0"))
        ;; No blocks reserved: the node runs as root, which could use them.
        (c/exec :mkfs.ext4 :-q :-F :-m 0 dm-dev)
        (c/exec :mount dm-dev (disk-dir test)))
      (do (c/exec :mkfs.ext4 :-q :-F :-m 0 disk-image)
          (c/exec :mount :-o :loop disk-image (disk-dir test))))))

(defn- dm-table
  "dm-name's table: :linear; [:delay ms], writes and flushes delayed;
  :flakey, writes failing with EIO 4 s in every 5."
  [kind]
  (let [loop-dev (str/trim (c/exec :cat loop-record))
        sectors (str/trim (c/exec :blockdev :--getsz loop-dev))
        [k ms] (if (vector? kind) kind [kind])]
    (str "0 " sectors " "
         (case k
           :linear (str "linear " loop-dev " 0")
           :delay  (str "delay " loop-dev " 0 0 " loop-dev " 0 " ms)
           :flakey (str "flakey " loop-dev " 0 1 4 1 error_writes")))))

(defn set-disk!
  "Swaps dm-name's table for kind's (see dm-table). Suspended without
  freezing the filesystem, whose writes may be failing."
  [kind]
  (c/su
    (let [table (dm-table kind)]
      (c/exec :dmsetup :suspend :--nolockfs dm-name)
      (try (c/exec :dmsetup :load dm-name :--table table)
           (finally (c/exec :dmsetup :resume dm-name))))
    kind))

(defn disk-damaged?
  "Whether the store's filesystem stopped taking writes: ext4 aborts its
  journal on a failed write and goes read-only; or is not mounted, a
  repair having failed half way."
  [test]
  (c/su
    (let [d (disk-dir test)]
      (or (not= "ok" (str/trim (c/exec :sh :-c (str "mountpoint -q " d " && echo ok || true"))))
          (str/includes? (str "," (str/trim (c/exec :findmnt :-no :OPTIONS d)) ",") ",ro,")
          (not= "ok" (str/trim (c/exec :sh :-c (str "touch " d "/.probe 2>/dev/null && rm -f " d "/.probe && echo ok || true"))))))))

(defn repair-disk!
  "Checks and mounts again the store's filesystem, its node stopped: what
  its journal committed kept, what failed to reach the disk lost, the
  page cache dropped. Not under LazyFS, which flaky-disk refuses."
  [test]
  (c/su
    (let [d (disk-dir test)]
      (c/exec :sh :-c (str "! mountpoint -q " d " || umount " d))
      ;; 0 clean, 1 errors fixed, 2 fixed and reboot advised: a loop
      ;; device's filesystem needs none.
      (c/exec :sh :-c (str "e2fsck -fy " dm-dev " >/dev/null; [ $? -le 2 ]"))
      (c/exec :mount dm-dev d)))
  :repaired)

(defn heal-disk!
  "Puts dm-name back to linear, if there, resumed first: a run cut short
  between a suspend and its resume leaves the store's I/O waiting, which
  no signal ends, and its node unkillable until then. Best effort."
  []
  (c/su
    (c/exec :sh :-c (str "dmsetup info " dm-name " >/dev/null 2>&1 || exit 0; "
                         "dmsetup resume " dm-name "; "
                         "[ -e " loop-record " ] || exit 0; l=$(cat " loop-record "); "
                         "dmsetup suspend --nolockfs " dm-name " && "
                         "{ dmsetup load " dm-name " --table \"0 $(blockdev --getsz $l) linear $l 0\"; "
                         "dmsetup resume " dm-name "; }; true"))))

(defn umount-disk!
  []
  (c/su
    ;; Healed first, if a run ended on a fault: unmounted, the filesystem
    ;; writes back what it holds.
    (heal-disk!)
    (doseq [d [(:data-dir lazy) data-dir]]
      (c/exec :sh :-c (str "! mountpoint -q " d " || umount " d " || umount -l " d)))
    ;; dm-name and its loop device, if any.
    (c/exec :sh :-c (str "if dmsetup info " dm-name " >/dev/null 2>&1; then dmsetup remove --retry " dm-name "; fi; "
                         "if [ -e " loop-record " ]; then losetup -d $(cat " loop-record ") 2>/dev/null || true; rm -f " loop-record "; fi"))
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

(def pending-starts
  "Nodes whose start waits for their disk to stop failing writes: under
  flaky-disk a start mounts the store's filesystem again, which a failing
  disk fails."
  (atom #{}))

(defn disk-failing?
  "Whether dm-name fails writes now (flaky-disk)."
  []
  (c/su (str/includes? (c/exec :sh :-c (str "dmsetup table " dm-name " 2>/dev/null || true")) "flakey")))

(defn start!
  "Starts the node, with a new incarnation, unless it is running: the
  nemesis starts all nodes, those it did not kill too. Under flaky-disk,
  on a disk failing writes, the start waits for the disk's release."
  [test node]
  (cond (c/su (cu/daemon-running? pid-file))
        :already-running
        (and (contains? (:faults test) :flaky-disk) (disk-failing?))
        (do (swap! pending-starts conj node) :start-pending)
        :else
        (start-new! test node)))

(defn- start-new!
  "Starts a process. Under flaky-disk, on the store's filesystem checked
  and mounted again first, which drops the page cache: a write-back the
  disk failed leaves its page clean in the cache and stale on the disk, a
  later fsync need not say so, and a process started on that cache reads
  what is not there, builds on it and syncs until the cache goes; SQLite
  then drops every WAL frame after the stale one, synced or not. This
  checks the filesystem, not the database."
  [test node]
  (swap! pending-starts disj node)
  (when (contains? (:faults test) :flaky-disk)
    (repair-disk! test))
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
    (when (or (:disk-full test) (:dm-disk test))
      (mount-disk! test))
    (when (:lazyfs test)
      (lazyfs/mount! lazy))
    (start! test node))

  (teardown! [this test node]
    ;; Before the kill, which waits for the node to die: a suspended disk
    ;; holds it.
    (heal-disk!)
    (kill! (dissoc test :power-loss) node)
    (c/su (cu/grepkill! :cont "lazyfs")) ; a hung disk left stopped
    ;; LazyFS, then the small filesystem, which may be its backing
    ;; directory: jepsen.lazyfs/umount! would remove that while mounted.
    ;; umount, not fusermount: Ubuntu's AppArmor lets fusermount unmount
    ;; under /tmp, /mnt and homes only, not the store.
    (c/su (c/exec :sh :-c (str "! mountpoint -q " data-dir " || umount -l " data-dir)))
    ;; Gone, LazyFS holds its backing files no more: a device-mapper
    ;; device under them could not be removed.
    (c/su (cu/grepkill! :kill "lazyfs"))
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
