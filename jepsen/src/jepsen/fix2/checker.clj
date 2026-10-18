(ns jepsen.fix2.checker
  "The checks of docs/jepsen.md, over the Jepsen history and the nodes'
  event logs, per direction: A sends, B receives. The workload's
  application messages are NewOrderSingles (35=D); a message is known by
  its ClOrdID and its digest, which a resend keeps.

  Sequence numbers are taken within a generation, each side's as its store
  numbers them: the base runs never reset, both sides stay at generation 1.
  Recovery losses carry no generation: they cover a sequence number in
  every generation, which runs that reset (J11) will have to refine."
  (:require [clojure.string :as str]
            [jepsen.checker :as checker]
            [jepsen.history :as h]
            [jepsen.fix2.events :as events]))

(def marker-prefix "BARRIER-")

(def redelivery-bound
  "How many MsgSeqNums a process kill may deliver again: the session
  records how far it processed every 64 at most, so a process may start
  delivering 63 before the highest an earlier one delivered."
  64)

(def skipped-error
  "The text of session.ErrSkipped."
  "gap-filled before the counterparty acknowledged it")

(defn- marker? [id] (and id (str/starts-with? id marker-prefix)))

(defn- other [test node] (first (remove #{node} (:nodes test))))

(defn- instant ^java.time.Instant [s] (when s (java.time.Instant/parse s)))

(defn process-node
  "The node a process's ops went to: Jepsen binds worker thread t to node
  t mod the node count, and a process crashed comes back as process plus
  the concurrency."
  [test process]
  (let [nodes (:nodes test)]
    (nth nodes (mod (mod process (:concurrency test)) (count nodes)))))

(defn- sender [test op] (or (:node op) (process-node test (:process op))))

(defn- worst
  "The verdict of problems, given uncertain ones that would excuse
  them."
  [problems uncertain]
  (cond (seq problems) false (seq uncertain) :unknown :else true))

;; Events

(defn- kind? [k] #(= k (:kind %)))

(defn- order-frame?
  "An order read or written, not a GapFill nor a frame that did not parse."
  [ev]
  (and (#{"in" "out"} (:kind ev)) (= "D" (:msgtype ev)) (nil? (:err ev))))

(defn- identity-of
  "What a frame or delivery carries at its sequence number: an order's
  ClOrdID and digest, or a session message's MsgType. A GapFill carries
  nothing: it covers the numbers it fills."
  [ev]
  (cond
    (or (= "deliver" (:kind ev)) (= "D" (:msgtype ev))) [(:clordid ev) (:digest ev)]
    (= "4" (:msgtype ev)) nil
    :else [:session (:msgtype ev)]))

(defn- incarnations
  "A node's events, by process, in the order they were started."
  [evs]
  (partition-by :inc evs))

(defn- store-end? [call] #(and (= "store" (:kind %)) (= call (:call %)) (= "end" (:phase %)) (nil? (:err %))))

(defn- bump [front [g s]] (update front g (fnil max 0) s))

(defn store-history
  "Walks a node's processes in order, from its store events: what each
  Open loaded, what was appended and what became durable with the SQLite
  store. An Append is durable once a Sync that returned nil began after it
  completed, or, for one an earlier process made that Open shows survived
  (below the NextSender it loaded), once any Sync of a later process
  returned nil: SQLite's Sync flushes every commit before it. An Append
  begun and never ended, its process killed, counts once the next Open
  shows it committed.

  Returns :processes, each with its :open event, :start time, and the
  :appended and :durable frontiers (generation → highest MsgSeqNum) of the
  processes before it; :durable and :appended, [gen seq] → [clordid
  digest], nil for a session message."
  [evs]
  (loop [incs (incarnations evs), survivors {}, uncertain {}, durable {}, appended {}
         dfront {}, afront {}, out []]
    (if-let [inc-evs (first incs)]
      (let [open (first (filter (store-end? "open") inc-evs))
            survives? (fn [[[g s] _]] (and (= g (:gen open)) (< s (:next open 0))))
            ;; An Append begun and never ended, its process killed, was
            ;; committed if the next Open loaded a NextSender past it.
            confirmed (if open (into {} (filter survives? uncertain)) {})
            survivors (if open (into confirmed (filter survives? survivors)) survivors)
            appends (concat confirmed
                            (for [ev inc-evs :when ((store-end? "append") ev)]
                              [[(:gen ev) (:seq ev)] (when (:clordid ev) [(:clordid ev) (:digest ev)])]))
            ended (set (for [ev inc-evs :when (and (= "store" (:kind ev)) (= "append" (:call ev)) (= "end" (:phase ev)))]
                         [(:gen ev) (:seq ev)]))
            unended (into {} (for [ev inc-evs
                                   :when (and (= "store" (:kind ev)) (= "append" (:call ev)) (= "begin" (:phase ev)))
                                   :when (not (ended [(:gen ev) (:seq ev)]))]
                               [[(:gen ev) (:seq ev)] (when (:clordid ev) [(:clordid ev) (:digest ev)])]))
            syncs (filter (store-end? "sync") inc-evs)
            cutoff (reduce (fn [m ev] (update m (:gen ev) (fnil max 0) (:cutoff ev 0))) {} syncs)
            made (concat (filter (fn [[[g s] _]] (<= s (get cutoff g 0))) appends)
                         (when (seq syncs) survivors))
            durable' (into durable made)]
        (recur (rest incs)
               (apply dissoc (into survivors appends) (map first made))
               (if open unended (into uncertain unended))
               durable'
               (into appended appends)
               (reduce bump dfront (map first made))
               (reduce bump afront (map first appends))
               (conj out {:inc (:inc (first inc-evs)) :open open :start (instant (:t (first inc-evs)))
                          :appended afront :durable dfront})))
      {:processes out :durable durable :appended appended})))

(defn- seen-before
  "For each of a's processes, generation → the highest MsgSeqNum b read
  before it started: b-ins and the processes' starts swept in time order.
  The nodes share one kernel clock."
  [processes b-evs]
  (let [ins (->> b-evs (filter (kind? "in")) (remove :err)
                 (map (fn [ev] [(instant (:t ev)) (:gen ev) (:seq ev)]))
                 (sort-by first))]
    (loop [ps processes, ins ins, front {}, out []]
      (if-let [p (first ps)]
        (let [[before after] (split-with #(.isBefore ^java.time.Instant (first %) (:start p)) ins)
              front (reduce (fn [f [_ g s]] (bump f [g s])) front before)]
          (recur (rest ps) after front (conj out front)))
        out))))

(defn- rejects
  "Every Reject (35=3) and BusinessMessageReject (35=j) of A's messages:
  B's out events and A's in events."
  [a-evs b-evs]
  (for [ev (concat (filter (kind? "out") b-evs) (filter (kind? "in") a-evs))
        :when (#{"3" "j"} (:msgtype ev))
        :when (:refseqnum ev)]
    {:key [(:gen ev) (:refseqnum ev)] :msgtype (:msgtype ev) :reason (:rejectreason ev)}))

(defn- sending-time-reject? [r] (= ["3" "10"] [(:msgtype r) (:reason r)]))

(defn- recovery-losses
  "Whether an OnRecoveryLoss reported A's message at a sequence number:
  A's own (evicted, missing from its store), and those B was reset past."
  [a-evs b-evs]
  (let [ranges (for [ev (concat (filter #(and ((kind? "recovery-loss") %) (not (re-find #"reset" (str (:err %))))) a-evs)
                                (filter #(and ((kind? "recovery-loss") %) (re-find #"reset" (str (:err %)))) b-evs))]
                 [(:begin ev) (:end ev)])]
    (fn [s] (some (fn [[lo hi]] (<= lo s hi)) ranges))))

;; One direction

(defn- direction
  "The checks of the messages from a to b."
  [test history evs a b markers]
  (let [a-evs (evs a)
        b-evs (evs b)
        b-incs (incarnations b-evs)
        deliveries (filter (kind? "deliver") b-evs)
        marker (get markers a)
        marker-seq (some #(when (= marker (:clordid %)) (:seq %)) deliveries)
        below? (fn [s] (or (nil? marker-seq) (< s marker-seq)))
        ;; Delivered before b delivered a's marker: what checks 4 and 5 ask of.
        closed (->> deliveries
                    (take-while #(not= marker (:clordid %)))
                    (reduce (fn [m d] (update m [(:gen d) (:seq d)] (fnil conj #{}) [(:clordid d) (:digest d)])) {}))
        delivered? (fn [k id] (contains? (get closed k) id))
        ;; Written: a's out events, b's in events.
        written (->> (concat (filter (kind? "out") a-evs) (filter (kind? "in") b-evs))
                     (filter order-frame?)
                     (map (fn [ev] [[(:gen ev) (:seq ev)] [(:clordid ev) (:digest ev)]]))
                     (into {}))
        stores (store-history a-evs)

        ;; Excuses for an order never delivered. b processes messages in
        ;; sequence, and passes one only once it was delivered, gap-filled,
        ;; or consumed by a Reject: b processed past it if it delivered a
        ;; later one, or asked its store to record a processed prefix past
        ;; it (a SetNextTarget begun: whether it was stored does not
        ;; matter). One b never processed past is not excused. One it did
        ;; is by a recovery loss; by a Reject for its SendingTime or a
        ;; SendingTime seen beyond MaxLatency with no Reject written, only
        ;; :unknown: such a Reject consumes a message received in sequence
        ;; but not one received ahead of a gap, which the log does not
        ;; tell apart yet (J10).
        recovery (recovery-losses a-evs b-evs)
        all-rejects (rejects a-evs b-evs)
        rejected-for-time (set (map :key (filter sending-time-reject? all-rejects)))
        processed (reduce (fn [m [g s]] (update m g (fnil max 0) s))
                          {}
                          (concat (for [ev b-evs
                                        :when (and (= "store" (:kind ev)) (= "set-next-target" (:call ev)) (= "begin" (:phase ev)))]
                                    [(:gen ev) (:target ev 0)])
                                  (for [d deliveries] [(:gen d) (:seq d)])))
        processed-past? (fn [[g s]] (< s (get processed g 0)))
        max-latency-ms (* 1000 (:max-latency test 120))
        stale (set (for [ev (filter (kind? "in") b-evs)
                         :when (order-frame? ev)
                         :let [st (instant (:sendingtime ev)) now (instant (:now ev))]
                         :when (and st now (<= max-latency-ms (Math/abs (- (.toEpochMilli now) (.toEpochMilli st)))))]
                     [(:gen ev) (:seq ev)]))
        excused (fn [[_ s :as k]]
                  (cond (recovery s)                     :lost
                        (not (processed-past? k))        nil
                        (or (contains? rejected-for-time k)
                            (contains? stale k))         :unknown))

        ;; 1. Order and duplicates, within each of b's processes.
        disorder (for [inc-evs b-incs
                       :let [ds (filter (kind? "deliver") inc-evs)]
                       [x y] (partition 2 1 ds)
                       :when (and (= (:gen x) (:gen y)) (<= (:seq y) (:seq x)))]
                   {:inc (:inc y) :after (select-keys x [:seq :clordid]) :delivered (select-keys y [:seq :clordid])})
        duplicates (for [inc-evs b-incs
                         [id n] (frequencies (map :clordid (filter (kind? "deliver") inc-evs)))
                         :when (< 1 n)]
                     {:inc (:inc (first inc-evs)) :clordid id :times n})

        ;; 2. Redelivery after a restart. gen → MsgSeqNum → the orders
        ;; written and not excused as lost, sorted, for range lookups; one
        ;; excused only perhaps makes a gap over it :unknown.
        pending (reduce (fn [m [[g s :as k] id]]
                          (let [why (excused k)]
                            (if (= :lost why)
                              m
                              (update m g (fnil assoc (sorted-map)) s {:clordid (first id) :unknown (= :unknown why)}))))
                        {} written)
        written-between (fn [gen lo hi]
                          (when-let [sm (pending gen)]
                            (when (< (inc lo) hi)
                              (for [[s w] (subseq sm > lo < hi)] (assoc w :seq s)))))
        redelivery (loop [incs b-incs, before {}, highest {}, out []]
                     (if-let [inc-evs (first incs)]
                       (let [ds (filter (kind? "deliver") inc-evs)
                             d0 (first ds)
                             m (get highest (:gen d0) 0)
                             problems (concat
                                        (for [d ds
                                              :let [k [(:gen d) (:seq d)] id [(:clordid d) (:digest d)]]
                                              :when (and (contains? before k) (not= id (before k)))]
                                          {:inc (:inc d) :problem :redelivered-another :seq (:seq d) :was (before k) :now id})
                                        (when d0
                                          (for [w (written-between (:gen d0) m (:seq d0))]
                                            (assoc w :inc (:inc d0) :problem :started-past)))
                                        (for [[x y] (partition 2 1 ds)
                                              :when (= (:gen x) (:gen y))
                                              w (written-between (:gen x) (:seq x) (:seq y))]
                                          (assoc w :inc (:inc y) :problem :skipped))
                                        (when (and d0 (not (:power-loss test)) (<= (:seq d0) m)
                                                   (<= redelivery-bound (- m (:seq d0))))
                                          [{:inc (:inc d0) :problem :redelivered-too-much :from (:seq d0) :highest m}]))]
                         (recur (rest incs)
                                (into before (for [d ds] [[(:gen d) (:seq d)] [(:clordid d) (:digest d)]]))
                                (reduce (fn [h d] (bump h [(:gen d) (:seq d)])) highest ds)
                                (into out problems)))
                       out))

        ;; 3. One message per sequence number.
        identities (->> (concat (filter (kind? "out") a-evs) (filter (kind? "in") b-evs) deliveries)
                        (remove :err)
                        (keep (fn [ev] (when-let [id (identity-of ev)] [[(:gen ev) (:seq ev)] id])))
                        (reduce (fn [m [k v]] (update m k (fnil conj #{}) v)) {}))
        conflicts (for [[k ids] identities :when (< 1 (count ids))] {:gen (first k) :seq (second k) :carried ids})

        ;; 4. Acknowledged implies delivered.
        acked (filter #(and (= :send (:f %)) (= :ok (:type %)) (= a (sender test %))) history)
        undelivered (for [op acked
                          :let [k [(:gen op) (:seq op)]]
                          :when (not (some #(= (:value op) (first %)) (get closed k)))]
                      (assoc (select-keys op [:value :gen :seq :process]) :excused (excused k)))

        ;; 5. Nothing lost silently.
        durable-orders (into {} (filter val (:durable stores)))
        owed (merge (if (:power-loss test) {} (into {} (filter val (:appended stores))))
                    durable-orders
                    written)
        silent (for [[[g s :as k] id] owed
                     :when (below? s)
                     :when (not (marker? (first id)))
                     :when (not (delivered? k id))]
                 {:gen g :seq s :clordid (first id) :durable (contains? durable-orders k) :excused (excused k)})

        ;; 6. ErrSkipped only when gap-filled.
        gap-fills (->> (concat (filter (kind? "out") a-evs) (filter (kind? "in") b-evs))
                       (filter #(and (= "4" (:msgtype %)) (:gapfill %)))
                       (group-by :gen))
        covered? (fn [g s] (some #(and (<= (:seq %) s) (< s (:newseqno %))) (gap-fills g)))
        skipped-unfilled (for [op history
                               :when (and (= :send (:f op)) (= :info (:type op)) (= a (sender test op)))
                               :when (str/includes? (str (:error op)) skipped-error)
                               :when (not (covered? (:gen op) (:seq op)))]
                           (select-keys op [:value :gen :seq]))

        ;; 7. Sequence numbers survive: every Append, session messages
        ;; included, without power loss; what was durable with it; and
        ;; whatever b read before.
        forgotten (for [[p seen] (map vector (:processes stores) (seen-before (:processes stores) b-evs))
                        :let [open (:open p)
                              g (:gen open)
                              kept (if (:power-loss test) (:durable p) (:appended p))
                              highest (max (get kept g 0) (get seen g 0))]
                        :when (and open (<= (:next open 0) highest))]
                    {:inc (:inc p) :next-sender (:next open) :highest-before highest
                     :kept (get kept g 0) :seen (get seen g 0)})

        ;; 8. Halts: nothing stored after, no send begun after succeeding.
        halts (for [inc-evs (incarnations a-evs)
                    :let [halt (first (filter (kind? "halt") inc-evs))]
                    :when halt
                    :let [halted (instant (:t halt))
                          after (filter #(.isAfter (instant (:t %)) halted) inc-evs)
                          began (set (for [ev after :when (and ((kind? "send") ev) (= "begin" (:phase ev)))] (:clordid ev)))]
                    problem (concat
                              (when-not (:disk-faults test) [{:problem :halted :err (:err halt)}])
                              (for [ev after :when (and (= "store" (:kind ev)) (= "append" (:call ev)) (= "begin" (:phase ev)))]
                                {:problem :appended-after-halt :seq (:seq ev)})
                              (for [ev after
                                    :when (and ((kind? "send") ev) (= "end" (:phase ev)) (= "ok" (:type ev)) (began (:clordid ev)))]
                                {:problem :sent-after-halt :clordid (:clordid ev)}))]
                (assoc problem :inc (:inc halt)))

        ;; Rejects of valid orders, but for their SendingTime.
        bad-rejects (for [r all-rejects
                          :when (contains? written (:key r))
                          :when (not (sending-time-reject? r))]
                      r)]
    {:order       {:valid? (and (empty? disorder) (empty? duplicates))
                   :disorder (take 16 disorder) :duplicates (take 16 duplicates)}
     :redelivery  {:valid? (worst (remove :unknown redelivery) (filter :unknown redelivery))
                   :problems (take 16 redelivery)}
     :identity    {:valid? (empty? conflicts) :conflicts (take 16 conflicts)}
     :acked       (let [lost (remove :excused undelivered)]
                    {:valid? (worst lost (filter #(= :unknown (:excused %)) undelivered))
                     :acknowledged (count acked) :undelivered-count (count lost) :undelivered (take 16 lost)})
     :silent-loss (let [lost (remove :excused silent)]
                    {:valid? (worst lost (filter #(= :unknown (:excused %)) silent))
                     :owed (count owed) :lost-count (count lost) :lost (take 16 lost)
                     :unknown (take 16 (filter #(= :unknown (:excused %)) silent))})
     :skipped     {:valid? (empty? skipped-unfilled) :unfilled (take 16 skipped-unfilled)}
     :durability  {:valid? (empty? forgotten) :forgotten (take 16 forgotten)}
     :halts       {:valid? (empty? halts) :problems (take 16 halts)}
     :rejects     {:valid? (empty? bad-rejects) :rejected (take 16 bad-rejects)}}))

;; Both directions

(defn- phantoms
  "Check 9: every ClOrdID a node delivered was sent, from the other node,
  or is a marker the other node issued."
  [test history evs]
  (let [sends (filter #(= :send (:f %)) history)
        issued (into {} (for [[node node-evs] evs
                              ev node-evs
                              :when (and ((kind? "send") ev) (marker? (:clordid ev)))]
                          [(:clordid ev) node]))
        from (merge (into {} (for [op sends] [(:value op) (sender test op)])) issued)
        found (for [[node node-evs] evs
                    id (distinct (map :clordid (filter (kind? "deliver") node-evs)))
                    :when (or (nil? (from id)) (= node (from id)))]
                {:node node :clordid id :sent-from (from id)})]
    {:valid? (empty? found) :phantoms (take 32 found)}))

(defn analyze
  "Checks history against evs, each node's events in order."
  [test history evs]
  (let [quiesce (->> history (filter #(= :quiesce (:f %))) (remove h/invoke?) first)
        live (= :ok (:type quiesce))
        markers (get-in quiesce [:value :markers])
        [n1 n2] (:nodes test)
        dirs {(str n1 "->" n2) (direction test history evs n1 n2 markers)
              (str n2 "->" n1) (direction test history evs n2 n1 markers)}
        checks (into {:live {:valid? live :quiesce quiesce}
                      :phantoms (phantoms test history evs)}
                     (for [check [:order :redelivery :identity :acked :silent-loss :skipped :durability :halts :rejects]]
                       [check (let [by-dir (into {} (for [[d r] dirs] [d (r check)]))]
                                (assoc by-dir :valid? (checker/merge-valid (map :valid? (vals by-dir)))))]))]
    (assoc checks :valid? (checker/merge-valid (map :valid? (vals checks))))))

(defn checker
  []
  (reify checker/Checker
    (check [_ test history opts] (analyze test history (events/all test)))))
