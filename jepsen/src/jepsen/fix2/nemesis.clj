(ns jepsen.fix2.nemesis
  "The faults of docs/jepsen.md, composed from jepsen.nemesis.combined's
  packages one by one: nemesis-package would bring the clock package,
  whose setup steps the clock of the whole Docker VM."
  (:require [jepsen.nemesis.combined :as nc]))

(def faults
  "The faults --nemesis takes. power-loss is kill with the store on LazyFS,
  what was not fsynced lost at each kill."
  #{:kill :pause :partition :packet :power-loss})

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

(defn package
  "The nemesis package for opts: :faults, a set of faults; :interval,
  mean seconds between nemesis operations; :db. The network's packages
  come first: their final generators heal it before the nodes are
  started again."
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
        (faults :partition)                (conj (nc/partition-package opts))
        (faults :packet)                   (conj (nc/packet-package opts))
        (some #{:kill :pause} faults)      (conj (nc/db-package opts))))))
