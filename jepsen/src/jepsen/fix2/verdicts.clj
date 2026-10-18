(ns jepsen.fix2.verdicts
  "Prints each fix2 check's verdict of a stored test's results:
  lein run -m jepsen.fix2.verdicts store/fix2/latest/results.edn"
  (:require [clojure.edn :as edn]))

(defn -main
  [path]
  (let [res (:fix2 (edn/read-string {:default (fn [_ v] v)} (slurp path)))]
    (doseq [[check r] (sort-by key (dissoc res :valid?))]
      (println (name check) (:valid? r)))
    (println "fix2" (:valid? res))))
