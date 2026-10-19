(ns jepsen.fix2.nemesis
  "The faults of docs/jepsen.md, composed from jepsen.nemesis.combined's
  packages one by one: nemesis-package would bring the clock package,
  whose setup steps the clock of the whole Docker VM."
  (:require [clojure.string :as str]
            [jepsen [control :as c]
                    [generator :as gen]
                    [nemesis :as n]]
            [jepsen.control.util :as cu]
            [jepsen.nemesis.time :as nt]
            [jepsen.fix2 [client :as client]
                         [db :as db]]
            [jepsen.nemesis.combined :as nc]))

(def faults
  "The faults --nemesis takes. power-loss is kill with the store on LazyFS,
  what was not fsynced lost at each kill; hang-disk stops LazyFS, every
  store I/O hanging until it resumes; disk-full fills the store's
  filesystem, a small one, then frees it. clock skews the sessions' clocks
  (Config.Now); system-clock steps and strobes the nodes' real clocks,
  which only nodes with a kernel each have: not on Docker."
  #{:kill :pause :partition :packet :power-loss :hang-disk :disk-full :clock :system-clock})

(def packet-behaviors
  "What the packet nemesis does to the traffic of the nodes it targets:
  delays (TCP hides reordering and duplication from fix2, not their
  delay), loss, corruption TCP's checksum should catch, and a slow link."
  [{:delay {}}
   {:delay {:time :200ms :jitter :100ms}}
   {:loss {:percent :20%}}
   {:loss {:percent :60%}}
   {:corrupt {:percent :10%}}
   {:duplicate {:percent :20%}}
   {:reorder {:percent :20%}}
   {:rate {:rate :100kbit}}
   {:delay {} :loss {:percent :10%} :duplicate {:percent :10%}}])

(defn- disk-nemesis
  "Hangs and fills the nodes' stores: :hang-disk stops LazyFS on the nodes
  of its value's spec, :fill-disk fills their filesystems; :release-disk
  resumes LazyFS and frees the filesystems of all nodes."
  [db]
  (reify
    n/Reflection
    (fs [_] #{:hang-disk :fill-disk :release-disk})

    n/Nemesis
    (setup! [this test] this)
    (invoke! [_ test op]
      (case (:f op)
        :hang-disk    (assoc op :value (c/on-nodes test (nc/db-nodes test db (:value op))
                                                   (fn [_ _] (c/su (cu/grepkill! :stop "lazyfs")) :hung)))
        :fill-disk    (assoc op :value (c/on-nodes test (nc/db-nodes test db (:value op))
                                                   (fn [_ _] (db/fill-disk!))))
        ;; Released, a node whose session halted is restarted, the
        ;; documented reconciliation: else it stays down the whole run.
        :release-disk (assoc op :value (c/on-nodes test
                                                   (fn [test node]
                                                     (when (contains? (:faults test) :hang-disk)
                                                       (c/su (cu/grepkill! :cont "lazyfs")))
                                                     (db/free-disk!)
                                                     (cond
                                                       (db/complete-kill! test node)
                                                       :released-killed
                                                       (client/halted? test node)
                                                       (do (db/kill! (dissoc test :power-loss) node)
                                                           (db/start! test node)
                                                           :released-restarted)
                                                       :else
                                                       :released))))))
    (teardown! [_ test])))

(defn- disk-package
  [opts]
  (let [faults  (:faults opts)
        targets [:one :all]
        hang    (fn [_ _] {:type :info :f :hang-disk :value (rand-nth targets)})
        fill    (fn [_ _] {:type :info :f :fill-disk :value (rand-nth targets)})
        release {:type :info :f :release-disk :value :all}
        starts  (cond-> [] (faults :hang-disk) (conj hang) (faults :disk-full) (conj fill))]
    {:generator       (->> (gen/flip-flop (gen/mix (map gen/repeat starts)) (gen/repeat release))
                           (gen/stagger (:interval opts nc/default-interval)))
     :final-generator release
     :nemesis         (disk-nemesis (:db opts))
     :perf            #{{:name "disk" :start #{:hang-disk :fill-disk} :stop #{:release-disk} :color "#C0A0E9"}}}))

(def clock-offsets
  "Offsets of a node's session clock, in ms, against a MaxLatency of 5s:
  some within it, some beyond."
  [-30000 -8000 -3000 3000 8000 30000])

(defn- clock-nemesis
  "Skews the nodes' session clocks (Config.Now) through their control API:
  :bump-clock [node-spec offset-ms], :reset-clock puts all back at 0."
  [db]
  (reify
    n/Reflection
    (fs [_] #{:bump-clock :reset-clock})

    n/Nemesis
    (setup! [this test] this)
    (invoke! [_ test op]
      (case (:f op)
        :bump-clock  (let [[spec offset] (:value op)
                           nodes (nc/db-nodes test db spec)]
                       (assoc op :value (into {} (for [node nodes]
                                                   [node (client/set-clock! node offset)]))))
        :reset-clock (assoc op :value (into {} (for [node (:nodes test)]
                                                 [node (client/set-clock! node 0)])))))
    (teardown! [_ test])))

(defn- clock-package
  [opts]
  {:generator       (->> (gen/flip-flop (fn [_ _] {:type :info :f :bump-clock
                                                    :value [(rand-nth [:one :all]) (rand-nth clock-offsets)]})
                                        (gen/repeat {:type :info :f :reset-clock}))
                         (gen/stagger (:interval opts nc/default-interval)))
   :final-generator {:type :info :f :reset-clock}
   :nemesis         (clock-nemesis (:db opts))
   :perf            #{{:name "clock" :start #{:bump-clock} :stop #{:reset-clock} :color "#A0E9DB"}}})

(def ntp-units
  "The services that would set the nodes' clocks back while system-clock
  runs: jepsen.nemesis.time stops timedated's, which Ubuntu does not run."
  ["chrony" "systemd-timesyncd" "ntp" "ntpsec"])

(def ntp-stopped
  "The time services system-clock stopped on a node, kept until started
  again: a run cut short leaves them for the next to start."
  "/var/lib/fix2-jepsen-ntp-stopped")

(defn- stop-ntp!
  "Stops the time services running on the nodes, each recorded first: a
  record or a stop that fails fails it, nothing more stopped."
  [test]
  (c/on-nodes test (fn [_ _]
                     (c/su (c/exec :sh :-c
                                   (str "set -e; for u in " (str/join " " ntp-units) "; do "
                                        "if systemctl is-active -q $u; then echo $u >>" ntp-stopped "; systemctl stop $u; fi; "
                                        "done"))))))

(defn- start-ntp!
  "Starts the time services stopped on the nodes, this run or one cut
  short; those that fail to start stay recorded."
  [test]
  (c/on-nodes test (fn [_ _]
                     (c/su (c/exec :sh :-c
                                   (str "[ -e " ntp-stopped " ] || exit 0; left=; "
                                        "for u in $(sort -u " ntp-stopped "); do systemctl start $u || left=\"$left $u\"; done; "
                                        "if [ -n \"$left\" ]; then printf '%s\\n' $left >" ntp-stopped "; exit 1; fi; "
                                        "rm -f " ntp-stopped))))))

(defn- system-clock-nemesis
  "Jepsen's clock nemesis, its fs renamed apart from clock's: :bump-time,
  :strobe-time, :reset-time, :check-time. Set up, it refuses nodes sharing
  a kernel, whose clock it would step for all, and stops the nodes'
  time services, started again at teardown, once the clocks are reset."
  ([]
   (system-clock-nemesis (n/compose {{:bump-time   :bump
                                      :strobe-time :strobe
                                      :reset-time  :reset
                                      :check-time  :check-offsets}
                                     (nt/clock-nemesis)})))
  ([nem]
   (reify
     n/Reflection
     (fs [_] #{:bump-time :strobe-time :reset-time :check-time})

     n/Nemesis
     (setup! [_ test]
       (let [boots (c/on-nodes test (fn [_ _] (c/exec :cat "/proc/sys/kernel/random/boot_id")))]
         (when (< (count (set (vals boots))) (count boots))
           (throw (ex-info "system-clock needs a kernel, and a clock, per node: these nodes share one (Docker?)"
                           {:boot-ids boots}))))
       ;; Before Jepsen's setup, whose ntpdate a running chronyd fails;
       ;; started again if either fails, on any node: Jepsen runs no
       ;; teardown for a nemesis whose setup failed.
       (system-clock-nemesis (try (stop-ntp! test)
                                  (n/setup! nem test)
                                  (catch Throwable e
                                    (try (start-ntp! test) (catch Throwable _))
                                    (throw e)))))
     (invoke! [_ test op] (n/invoke! nem test op))
     (teardown! [_ test]
       (try (n/teardown! nem test)
            (finally (start-ntp! test)))))))

(defn- system-clock-package
  "Jepsen's clock package, on system-clock-nemesis: bumps from 11 ms to
  290 s and strobes of up to 262 s for up to 32 s, on one node or both,
  the clocks reset by NTP between them and at the end."
  [opts]
  (let [targets   [:one :all]
        nodes     (fn [test] (nc/db-nodes test (:db opts) (rand-nth targets)))
        generator (->> (gen/phases {:type :info :f :check-offsets}
                                   (gen/mix [(nt/reset-gen-select nodes)
                                             (nt/bump-gen-select nodes)
                                             (nt/strobe-gen-select nodes)]))
                       (gen/f-map {:reset :reset-time :check-offsets :check-time
                                   :strobe :strobe-time :bump :bump-time})
                       (gen/stagger (:interval opts nc/default-interval)))]
    {:generator       generator
     :final-generator (gen/once (fn [test _] {:type :info :f :reset-time :value (:nodes test)}))
     :nemesis         (system-clock-nemesis)
     :perf            #{{:name "system clock" :start #{:bump-time} :stop #{:reset-time} :fs #{:strobe-time}
                         :color "#E9C3A0"}}}))

(defn package
  "The nemesis package for opts: :faults, a set of faults; :interval,
  mean seconds between nemesis operations; :db. The network's and disks'
  packages come first: their final generators heal them before the nodes
  are started again, a process waiting on a stopped LazyFS not to be
  killed."
  [opts]
  (let [opts (merge {:kill      {:targets [:one :all]}
                     :pause     {:targets [:one :all]}
                     ; Two nodes: isolating one isolates both.
                     :partition {:targets [:one]}
                     :packet    {:targets [:one :all] :behaviors packet-behaviors}}
                    opts)
        faults (cond-> (:faults opts) (contains? (:faults opts) :power-loss) (conj :kill))
        opts (assoc opts :faults faults)]
    (nc/compose-packages
      (cond-> []
        (faults :partition)                       (conj (nc/partition-package opts))
        (faults :packet)                          (conj (nc/packet-package opts))
        (some #{:hang-disk :disk-full} faults)    (conj (disk-package opts))
        (faults :clock)                           (conj (clock-package opts))
        (faults :system-clock)                    (conj (system-clock-package opts))
        (some #{:kill :pause} faults)             (conj (nc/db-package opts))))))
