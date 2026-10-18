(ns jepsen.fix2.nemesis
  "The faults of docs/jepsen.md, composed from jepsen.nemesis.combined's
  packages one by one: nemesis-package would bring the clock package,
  whose setup steps the clock of the whole Docker VM."
  (:require [jepsen.nemesis.combined :as nc]))

(def faults
  "The faults --nemesis takes."
  #{:kill :pause})

(defn package
  "The nemesis package for opts: :faults, a set of faults; :interval,
  seconds between faults; :db."
  [opts]
  (let [opts (merge {:kill  {:targets [:one :all]}
                     :pause {:targets [:one :all]}}
                    opts)]
    (nc/compose-packages
      (cond-> []
        (some #{:kill :pause} (:faults opts)) (conj (nc/db-package opts))))))
