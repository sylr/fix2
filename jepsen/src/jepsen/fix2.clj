(ns jepsen.fix2
  "Jepsen tests of fix2's FIX session layer: an initiator and an acceptor,
  each on its SQLite store, sending orders to each other. See
  docs/jepsen.md."
  (:require [jepsen [checker :as checker]
                    [cli :as cli]
                    [generator :as gen]
                    [os :as os]
                    [tests :as tests]]
            [jepsen.checker.timeline :as timeline]
            [jepsen.fix2 [checker :as fix2-checker]
                         [client :as client]
                         [db :as db]]))

(defn sends
  "Orders with unique ClOrdIDs."
  []
  (map (fn [i] {:f :send :value (str "O" i)}) (range)))

(defn fix2-test
  "Builds the test from the CLI's options."
  [opts]
  (when (not= 2 (count (:nodes opts)))
    (throw (ex-info "fix2 runs on two nodes: the initiator, then the acceptor" {:nodes (:nodes opts)})))
  (merge tests/noop-test
         opts
         {:name      "fix2"
          :os        os/noop
          :db        (db/db)
          :client    (client/client)
          :checker   (checker/compose
                       {:perf       (checker/perf)
                        :stats      (checker/stats)
                        :exceptions (checker/unhandled-exceptions)
                        :panics     (checker/log-file-pattern #"panic:" "node.log")
                        :timeline   (timeline/html)
                        :fix2       (fix2-checker/checker)})
          :generator (gen/phases
                       (->> (sends)
                            (gen/stagger (/ (:rate opts)))
                            (gen/time-limit (:time-limit opts))
                            gen/clients)
                       (gen/clients (gen/once {:f :quiesce})))}))

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
   [nil "--quiesce-timeout SECONDS" "How long the final phase waits for the markers: more than MaxLatency and the largest clock skew."
    :default 150
    :parse-fn parse-long
    :validate [#(and % (pos? %)) "Must be a positive integer"]]
   [nil "--mutant NAMES" "Breaks the nodes on purpose, for the checkers to catch: no-sync, drop-deliver, redeliver, comma-separated."]])

(defn -main
  [& args]
  (cli/run! (merge (cli/single-test-cmd {:test-fn  fix2-test
                                         :opt-spec cli-opts})
                   (cli/serve-cmd))
            args))
