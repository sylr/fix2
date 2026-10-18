(ns jepsen.fix2
  "Jepsen tests of fix2's FIX session layer: an initiator and an acceptor,
  each on its SQLite store, sending orders to each other. See
  docs/jepsen.md."
  (:require [clojure.string :as str]
            [jepsen [checker :as checker]
                    [cli :as cli]
                    [generator :as gen]
                    [net :as net]
                    [os :as os]
                    [tests :as tests]]
            [jepsen.checker.timeline :as timeline]
            [jepsen.fix2 [checker :as fix2-checker]
                         [client :as client]
                         [db :as db]
                         [nemesis :as nemesis]]))

(defn sends
  "Orders with unique ClOrdIDs."
  []
  (map (fn [i] {:f :send :value (str "O" i)}) (range)))

(defn fix2-test
  "Builds the test from the CLI's options."
  [opts]
  (when (not= 2 (count (:nodes opts)))
    (throw (ex-info "fix2 runs on two nodes: the initiator, then the acceptor" {:nodes (:nodes opts)})))
  (let [db  (db/db)
        pkg (nemesis/package {:db db :faults (:nemesis opts) :interval (:nemesis-interval opts)})
        faults (:nemesis opts)
        power-loss? (contains? faults :power-loss)
        hang? (contains? faults :hang-disk)
        _ (when (and power-loss? hang?)
            ;; A kill left pending on a hung disk would lose its power loss.
            (throw (ex-info "power-loss and hang-disk do not combine yet" {:faults faults})))
        _ (when (and (= :memstore (:session-store opts)) (some #{:power-loss :hang-disk :disk-full} faults))
            (throw (ex-info "memstore has no disk to fault" {:faults faults})))]
    (merge tests/noop-test
           opts
           {:name      (str "fix2"
                            (when (= :memstore (:session-store opts)) "-memstore")
                            (when (not= :lazy (:ack-probe opts)) (str "-probe-" (name (:ack-probe opts))))
                            (when (pos? (+ (:retention-keep opts) (:retention-max opts))) "-retention")
                            (when (:last-processed opts) "-369")
                            (when (seq faults) (str "-" (str/join "," (map name (sort faults))))))
            ;; The store on LazyFS, for a power loss or a hung disk; the
            ;; checkers told what a power loss may take, and that a disk
            ;; fault may halt a session.
            ;; :nemesis holds the nemesis itself: the faults go here.
            :faults      faults
            :lazyfs      (or power-loss? hang?)
            :power-loss  power-loss?
            :disk-full   (contains? faults :disk-full)
            :disk-faults (boolean (some #{:hang-disk :disk-full} faults))
            ;; A hung disk halts the session once a write passes
            ;; StoreTimeout: shortened in those runs, unless given.
            :store-timeout (if (and hang? (zero? (:store-timeout opts))) 2000 (:store-timeout opts))
            ;; Clock skews of seconds against a MaxLatency of 5s, unless
            ;; given; the checkers read the same.
            :max-latency (or (:max-latency opts) (if (contains? faults :clock) 5 120))
            :os        os/noop
            ;; The interface named: jepsen.net shapes the first one `ip link`
            ;; lists, and Docker Desktop's kernel puts tunnel devices (tunl0,
            ;; gre0, sit0...) before eth0 in every container.
            :net       (net/iptables-with-dev "eth0")
            :db        db
            :client    (client/client)
            :nemesis   (:nemesis pkg)
            :checker   (checker/compose
                         {:perf       (checker/perf {:nemeses (:perf pkg)})
                          :stats      (checker/stats)
                          :exceptions (checker/unhandled-exceptions)
                          :panics     (checker/log-file-pattern #"panic:" "node.log")
                          :timeline   (timeline/html)
                          :fix2       (fix2-checker/checker)})
            :generator (gen/phases
                         (->> (sends)
                              (gen/stagger (/ (:rate opts)))
                              (gen/nemesis (:generator pkg))
                              (gen/time-limit (:time-limit opts)))
                         ;; Heal: resume what is paused, start what is down,
                         ;; and give the sessions time to log on again.
                         (gen/nemesis (:final-generator pkg))
                         (gen/sleep (:heal-time opts))
                         (gen/clients (gen/once {:f :quiesce})))})))

(def cli-opts
  "Options beyond Jepsen's."
  [[nil "--rate HZ" "Orders per second, across all workers."
    :default 50
    :parse-fn parse-double
    :validate [#(and % (pos? %)) "Must be a positive number"]]
   [nil "--heartbeat SECONDS" "The sessions' HeartBtInt."
    :default 1
    :parse-fn parse-long
    :validate [#(and % (pos? %)) "Must be a positive integer"]]
   [nil "--max-latency SECONDS" "The sessions' MaxLatency: 120, or 5 under clock."
    :parse-fn parse-long
    :validate [#(and % (pos? %)) "Must be a positive whole number of seconds"]]
   [nil "--store-timeout MS" "The sessions' StoreTimeout, their default if 0 (2000 under hang-disk)."
    :default 0
    :parse-fn parse-long
    :validate [#(and % (not (neg? %))) "Must be a whole number of milliseconds, not negative"]]
   [nil "--nemesis FAULTS" "Faults, comma-separated: kill, pause, partition, packet, power-loss (kill, with the store on LazyFS losing what was not fsynced), hang-disk (LazyFS stopped), disk-full (the store's small filesystem filled)."
    :default #{}
    :parse-fn (fn [s] (set (map keyword (remove str/blank? (str/split s #",")))))
    :validate [#(every? nemesis/faults %) (str "Must be among " (str/join ", " (map name (sort nemesis/faults))))]]
   [nil "--nemesis-interval SECONDS" "Mean seconds between nemesis operations, starts and resumes included."
    :default 10
    :parse-fn parse-double
    :validate [#(and % (pos? %)) "Must be a positive number"]]
   [nil "--heal-time SECONDS" "Seconds between healing and the quiesce phase."
    :default 5
    :parse-fn parse-double
    :validate [#(and % (not (neg? %))) "Must not be negative"]]
   [nil "--quiesce-timeout SECONDS" "How long the final phase waits for the markers: more than MaxLatency and the largest clock skew."
    :default 150
    :parse-fn parse-long
    :validate [#(and % (pos? %)) "Must be a positive integer"]]
   [nil "--fromapp-delay MS" "How long, in whole milliseconds, the nodes' application takes for each message: a slow one keeps the delivery queue full, and a kill then has messages to deliver again."
    :default 0
    :parse-fn parse-long
    :validate [#(and % (not (neg? %))) "Must be a whole number of milliseconds, not negative"]]
   [nil "--session-store STORE" "The sessions' store (:store is Jepsen's): sqlite, or memstore, which a kill empties: the checks are expected to fail under kills."
    :default :sqlite
    :parse-fn keyword
    :validate [#{:sqlite :memstore} "Must be sqlite or memstore"]]
   [nil "--ack-probe WHEN" "The sessions' AckProbe: lazy or eager. Not never: a send waits to be acknowledged, which only a probe tells."
    :default :lazy
    :parse-fn keyword
    :validate [#{:lazy :eager} "Must be lazy or eager"]]
   [nil "--retention-keep N" "Retention on, every 500 ms: the last N sequence numbers kept, acknowledged or not."
    :default 0
    :parse-fn parse-long
    :validate [#(and % (not (neg? %))) "Must be a whole number, not negative"]]
   [nil "--retention-max N" "Retention on, every 500 ms: at most N sequence numbers kept, acknowledged or not."
    :default 0
    :parse-fn parse-long
    :validate [#(and % (not (neg? %))) "Must be a whole number, not negative"]]
   [nil "--last-processed" "The sessions send LastMsgSeqNumProcessed (369): the processed prefix, which retention takes for acknowledgements, and Keep the margin for a counterparty forgetting part of it."]
   [nil "--mutant NAMES" "Breaks the nodes on purpose, for the checkers to catch: no-sync, drop-deliver, redeliver, comma-separated."]])

(defn -main
  [& args]
  (cli/run! (merge (cli/single-test-cmd {:test-fn  fix2-test
                                         :opt-spec cli-opts})
                   (cli/serve-cmd))
            args))
