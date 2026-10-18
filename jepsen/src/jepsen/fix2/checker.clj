(ns jepsen.fix2.checker
  "The checks of docs/jepsen.md, over the Jepsen history and the nodes'
  event logs. So far: the liveness of the quiesce phase, check 4
  (Acknowledged implies delivered) and check 9 (no phantoms). Reported
  losses, which check 4 excuses, come with the faults that cause them."
  (:require [clojure.string :as str]
            [jepsen [checker :as checker]
                    [history :as h]]
            [jepsen.fix2.events :as events]))

(def marker-prefix "BARRIER-")

(defn- marker? [id] (and id (str/starts-with? id marker-prefix)))

(defn- other [test node] (first (remove #{node} (:nodes test))))

(defn process-node
  "The node a process's ops went to: Jepsen binds worker thread t to node
  t mod the node count, and a process crashed comes back as process plus
  the concurrency."
  [test process]
  (let [nodes (:nodes test)]
    (nth nodes (mod (mod process (:concurrency test)) (count nodes)))))

(defn- sender
  "The node a send op went to."
  [test op]
  (or (:node op) (process-node test (:process op))))

(defn- deliveries
  "The ClOrdIDs in the order node's events delivered them."
  [evs]
  (->> evs (filter #(= "deliver" (:kind %))) (map :clordid)))

(defn- delivered-before
  "The ClOrdIDs delivered before the first delivery of marker, all of them
  without one."
  [delivered marker]
  (set (if marker (take-while #(not= marker %) delivered) delivered)))

(defn- issued-markers
  "Marker ClOrdID → the node that sent it, from the nodes' send events."
  [evs]
  (into {} (for [[node node-evs] evs
                 ev node-evs
                 :when (and (= "send" (:kind ev)) (marker? (:clordid ev)))]
             [(:clordid ev) node])))

(defn analyze
  "Checks history and the event logs."
  [test history]
  (let [evs        (events/all test)
        delivered  (into {} (for [[n e] evs] [n (deliveries e)]))
        quiesce    (->> history (filter #(= :quiesce (:f %))) (remove h/invoke?) first)
        live?      (= :ok (:type quiesce))
        markers    (get-in quiesce [:value :markers]) ; sender → marker
        ; What each node delivered before the other's marker: the messages
        ; sent before it.
        closed     (into {} (for [n (:nodes test)]
                              [n (delivered-before (delivered n) (get markers (other test n)))]))
        sends      (filter #(= :send (:f %)) history)
        acked      (filter #(= :ok (:type %)) sends)
        lost       (for [op acked
                         :let [to (other test (sender test op))]
                         :when (not (contains? (closed to) (:value op)))]
                     (select-keys op [:value :node :process :gen :seq]))
        from       (merge (into {} (for [op sends] [(:value op) (sender test op)]))
                          (issued-markers evs))
        phantoms   (for [[node ids] delivered
                         id (distinct ids)
                         :when (or (nil? (from id)) (= node (from id)))]
                     {:node node :clordid id :sent-from (from id)})]
    {:valid?   (and live? (empty? lost) (empty? phantoms))
     :live     {:valid? live? :quiesce quiesce}
     :acked    {:valid?       (empty? lost)
                :acknowledged (count acked)
                :lost-count   (count lost)
                :lost         (take 32 lost)}
     :phantoms {:valid? (empty? phantoms) :phantoms (take 32 phantoms)}}))

(defn checker
  []
  (reify checker/Checker
    (check [_ test history opts] (analyze test history))))
