(ns jepsen.fix2.events
  "Reads the event logs the nodes wrote, downloaded with the test's log
  files: events-<node>-<n>.jsonl, the nth process started on the node."
  (:require [clojure.data.json :as json]
            [clojure.java.io :as io]
            [jepsen.store :as store]))

(defn- read-file
  "The events of one log file, in order. Only the last line may be cut
  short, by a kill: it is dropped. Any other that does not parse is an
  error."
  [^java.io.File f]
  (with-open [r (io/reader f)]
    (let [lines (vec (line-seq r))
          last-i (dec (count lines))]
      (->> lines
           (map-indexed
             (fn [i line]
               (try (json/read-str line :key-fn keyword)
                    (catch Exception e
                      (when (< i last-i)
                        (throw (ex-info (str "event log " f ", line " (inc i) ": " (.getMessage e))
                                        {:file (str f) :line (inc i)} e)))))))
           (remove nil?)
           doall))))

(defn- incarnation-number
  "n, of events-<node>-<n>.jsonl."
  [^java.io.File f]
  (or (some->> (.getName f) (re-find #"-(\d+)\.jsonl$") second parse-long) 0))

(defn node-events
  "Every event node logged, each tagged with :node, in the order its
  processes were started and, within one, of its file."
  [test node]
  (let [dir (store/path test node "events")]
    (->> (.listFiles (io/file dir))
         (filter #(re-find #"\.jsonl$" (.getName ^java.io.File %)))
         (sort-by incarnation-number)
         (mapcat read-file)
         (map #(assoc % :node node)))))

(defn all
  "Every node's events, by node."
  [test]
  (into {} (for [n (:nodes test)] [n (vec (node-events test n))])))
