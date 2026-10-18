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
        power-loss? (contains? (:nemesis opts) :power-loss)]
    (merge tests/noop-test
           opts
           {:name      (str "fix2" (when (seq (:nemesis opts)) (str "-" (str/join "," (map name (sort (:nemesis opts)))))))
            ;; The store on LazyFS, and the checkers told what a power loss
            ;; may take.
            :lazyfs     power-loss?
            :power-loss power-loss?
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
   [nil "--nemesis FAULTS" "Faults, comma-separated: kill, pause, partition, packet, power-loss (kill, with the store on LazyFS losing what was not fsynced)."
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
   [nil "--mutant NAMES" "Breaks the nodes on purpose, for the checkers to catch: no-sync, drop-deliver, redeliver, comma-separated."]])

(defn -main
  [& args]
  (cli/run! (merge (cli/single-test-cmd {:test-fn  fix2-test
                                         :opt-spec cli-opts})
                   (cli/serve-cmd))
            args))
