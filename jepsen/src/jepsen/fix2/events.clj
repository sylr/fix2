(ns jepsen.fix2.events
  "Reads the event logs the nodes wrote, downloaded with the test's log
  files: events-<node>-<n>.jsonl, the nth process started on the node,
  of its first session, events-s<k>-<node>-<n>.jsonl of its k-th."
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

(defn session-number
  "k, of events-s<k>-<node>-<n>.jsonl, the k-th session's; 0 for the
  first's, events-<node>-<n>.jsonl: node's name may look like s<k>."
  [node file-name]
  (or (some->> file-name
               (re-find (re-pattern (str "^events-s(\\d+)-" (java.util.regex.Pattern/quote node) "-\\d+\\.jsonl$")))
               second parse-long)
      0))

(defn node-events
  "Every event node logged, each tagged with :node and :session, in the
  order its processes were started and, within one, of its files."
  [test node]
  (let [dir (store/path test node "events")]
    (->> (.listFiles (io/file dir))
         (filter #(re-find #"\.jsonl$" (.getName ^java.io.File %)))
         (sort-by (juxt incarnation-number #(session-number node (.getName ^java.io.File %))))
         (mapcat (fn [^java.io.File f]
                   (let [k (session-number node (.getName f))] (map #(assoc % :session k) (read-file f)))))
         (map #(assoc % :node node)))))

(defn all
  "Every node's events, by node."
  [test]
  (into {} (for [n (:nodes test)] [n (vec (node-events test n))])))
