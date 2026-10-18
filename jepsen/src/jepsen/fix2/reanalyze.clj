(ns jepsen.fix2.reanalyze
  "Checks a stored test again with the current fix2 checker, and prints
  each check's verdict and, of those that fail, a few problems:
  lein run -m jepsen.fix2.reanalyze store/fix2/latest"
  (:require [clojure.pprint :refer [pprint]]
            [jepsen.store :as store]
            [jepsen.fix2 [checker :as checker]
                         [events :as events]]))

(defn -main
  [dir]
  (let [test (store/test dir)
        res (checker/analyze test (:history test) (events/all test))]
    (doseq [[check r] (sort-by key (dissoc res :valid?))]
      (println (name check) (:valid? r))
      (when (#{:acked :silent-loss} check)
        (doseq [[dir d] r :when (map? d)]
          (println " " dir (select-keys d [:acknowledged :owed :undelivered-count :lost-count :reset-abandoned]))))
      (when (and (not (true? (:valid? r))) (map? r))
        (doseq [[dir d] r :when (map? d) :when (not (true? (:valid? d)))]
          (println " " dir)
          (pprint (into {} (for [[k v] d] [k (if (sequential? v) (take 4 v) v)]))))))
    (println "fix2" (:valid? res))
    (shutdown-agents)))
