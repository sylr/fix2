(ns jepsen.fix2.nemesis
  "The faults of docs/jepsen.md, composed from jepsen.nemesis.combined's
  packages one by one: nemesis-package would bring the clock package,
  whose setup steps the clock of the whole Docker VM."
  (:require [jepsen [control :as c]
                    [generator :as gen]
                    [nemesis :as n]]
            [jepsen.control.util :as cu]
            [jepsen.fix2 [client :as client]
                         [db :as db]]
            [jepsen.nemesis.combined :as nc]))

(def faults
  "The faults --nemesis takes. power-loss is kill with the store on LazyFS,
  what was not fsynced lost at each kill; hang-disk stops LazyFS, every
  store I/O hanging until it resumes; disk-full fills the store's
  filesystem, a small one, then frees it."
  #{:kill :pause :partition :packet :power-loss :hang-disk :disk-full})

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
                                                       (seq (:err (client/status node)))
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
        (some #{:kill :pause} faults)             (conj (nc/db-package opts))))))
