(ns jepsen.fix2.checker
  "The checks of docs/jepsen.md, over the Jepsen history and the nodes'
  event logs, per direction: A sends, B receives. The workload's
  application messages are NewOrderSingles (35=D); a message is known by
  its ClOrdID and its digest, which a resend keeps.

  Sequence numbers are taken within the sender's epochs, [store lifetime,
  generation] (see normalize): each side numbers its generations apart,
  and what the receiver read and did is matched to the sender's by the
  frames' bytes. An order in an epoch a reset ended, never processed past,
  was abandoned by it: excused, and counted. A run that does not reset
  keeps one lifetime a node: a memstore started again must carry on."
  (:require [clojure.string :as str]
            [jepsen.checker :as checker]
            [jepsen.history :as h]
            [jepsen.fix2 [client :as client]
                         [events :as events]]))

(def marker-prefix "BARRIER-")

(def redelivery-bound
  "How many MsgSeqNums a process kill may deliver again: the session
  records how far it processed every 64 at most, so a process may start
  delivering 63 before the highest an earlier one delivered."
  64)

(def skipped-error
  "The text of session.ErrSkipped."
  "gap-filled before the counterparty acknowledged it")

(def retention-error
  "The text of session.ErrBeyondRetention."
  "messages evicted from the store gap-filled")

(defn- marker? [id] (and id (str/starts-with? id marker-prefix)))

(defn- other [test node] (first (remove #{node} (:nodes test))))

(defn- instant ^java.time.Instant [s] (when s (java.time.Instant/parse s)))

(defn- unsynced-lost?
  "Whether what the store did not fsync may be lost: on LazyFS at each kill
  (power-loss), on a disk failing its writes when written back
  (flaky-disk). Runs recorded before :unsynced-lost tell by :power-loss."
  [test]
  (boolean (or (:power-loss test) (:unsynced-lost test))))

(defn- instant-ns [^java.time.Instant i] (+ (* 1000000000 (.getEpochSecond i)) (.getNano i)))

(defn- node-ns
  "When ev happened on its node, in ns, to compare with the node's other
  events: by CLOCK_MONOTONIC, which a step of the real clock does not
  move; by the real clock in runs logged before it. Whole: events a
  millisecond apart keep their order."
  [ev]
  (if-let [m (:mono ev)]
    (long m)
    (instant-ns (instant (:t ev)))))

(defn- ms-between [a b] (quot (- (node-ns b) (node-ns a)) 1000000))

(defn- wall-offset
  "Where the node's real clock stood when ev happened, against its
  monotonic one, in ms: a step of the real clock moves it."
  [ev]
  (when (:mono ev)
    (quot (- (instant-ns (instant (:t ev))) (long (:mono ev))) 1000000)))

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

  Returns :processes, each with its :open event and the
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
               (conj out {:inc (:inc (first inc-evs)) :open open :appended afront :durable dfront})))
      {:processes out :durable durable :appended appended})))

(defn target-history
  "Walks a node's processes in order, from its store events, for how far
  it recorded processing what it received (SetNextTarget). A recorded
  prefix is durable once a Sync that returned nil began after it was
  committed, or, the one an Open loaded, once a Sync of that process
  returned nil. Returns, for each process, its :open event and the
  :committed and :durable frontiers (generation → highest prefix) of the
  processes before it.

  A Sync makes durable what its process committed since it opened, on
  top of what that Open loaded: not what processes before committed and
  a loss (power-loss, flaky-disk) took, which only a later commit and Sync
  make durable again."
  [evs]
  (loop [incs (incarnations evs), committed {}, durable {}, out []]
    (if-let [inc-evs (first incs)]
      (let [open (first (filter (store-end? "open") inc-evs))
            ;; [every commit, this process's from what it loaded, durable,
            ;; this process's when the Sync in progress began]
            [c _ d] (reduce (fn [[c cur d snap] ev]
                              (cond
                                ((store-end? "set-next-target") ev)
                                (let [k [(:gen ev) (:target ev 0)]] [(bump c k) (bump cur k) d snap])
                                (and (= "store" (:kind ev)) (= "sync" (:call ev)) (= "begin" (:phase ev)))
                                [c cur d cur]
                                ((store-end? "sync") ev)
                                [c cur (merge-with max d snap) snap]
                                :else [c cur d snap]))
                            (let [loaded (when open {(:gen open) (:target open 0)})]
                              [(merge-with max committed loaded) (or loaded committed) durable {}])
                            inc-evs)]
        (recur (rest incs) c d (conj out {:inc (:inc (first inc-evs)) :open open :committed committed :durable durable})))
      out)))

(defn- places
  "Each process's place among its node's, by incarnation: 0 the first
  started."
  [evs]
  (into {} (map-indexed (fn [i inc-evs] [(:inc (first inc-evs)) i]) (incarnations evs))))

(defn- writers
  "Which of a's processes wrote each frame b read, with no clock shared
  between the nodes: the one that logged writing the same bytes; else the
  one that wrote what b read on the same connection (b's process, its
  epoch), a process killed before logging its last frames. Of a connection
  with none logged, a process killed as it connected, any process, but on
  an initiator: its connections follow one another, as do each node's
  processes, so a later one cannot join an earlier process, and one lies
  between those before and after it. An acceptor numbers its connections
  as they attach, not as they were made: one of a process since killed may
  attach late. Returns ev → [lo hi], the places (see places) of the
  processes that may have written it."
  [a-evs b-evs]
  (let [place (places a-evs)
        last-place (dec (count place))
        span (fn [[lo hi] i] [(min (or lo i) i) (max (or hi i) i)])
        by-hash (reduce (fn [m ev] (if (:hash ev) (update m (:hash ev) span (place (:inc ev))) m))
                        {} (filter (kind? "out") a-evs))
        ins (filter (kind? "in") b-evs)
        conn (juxt :inc #(:epoch % 0))
        b-place (places b-evs)
        conns (sort-by (fn [[inc e]] [(b-place inc) e]) (distinct (map conn ins)))
        matched (reduce (fn [m ev] (if-let [[lo hi] (by-hash (:hash ev))]
                                     (update m (conn ev) #(span (span % lo) hi))
                                     m))
                        {} ins)
        initiator? (= "initiator" (some #(when ((kind? "start") %) (:role %)) b-evs))
        ;; Past the lowest a connection before it joined, short of the
        ;; highest one after it did.
        lows (rest (reductions (fn [lo c] (max lo (get-in matched [c 0] 0))) 0 conns))
        highs (reverse (rest (reductions (fn [hi c] (min hi (get-in matched [c 1] last-place))) last-place (reverse conns))))
        ranges (into {} (map (fn [c lo hi]
                               [c (cond (matched c)                    (matched c)
                                        (and initiator? (<= lo hi)) [lo hi]
                                        :else                          [0 last-place])])
                             conns lows highs))]
    (fn [ev] (or (by-hash (:hash ev)) (ranges (conn ev))))))

(defn- seen-before
  "For each of a's processes, generation → the highest MsgSeqNum b read
  that an earlier process wrote (:seen), or may have (:maybe), by writer
  (see writers): the nodes' clocks are not compared."
  [processes writer b-evs]
  (let [ins (for [ev b-evs :when ((kind? "in") ev) :when (not (:err ev))] [(writer ev) (:gen ev) (:seq ev)])
        ;; Process i: the frames whose bound is below i.
        fronts (fn [bound]
                 (let [by (reduce (fn [m [r g s]] (update m (bound r) bump [g s])) {} ins)]
                   (reductions (fn [f i] (merge-with max f (by i))) {} (range (dec (count processes))))))]
    (map (fn [seen maybe] {:seen seen :maybe maybe}) (fronts second) (fronts first))))

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
  "Whether an OnRecoveryLoss reported A's message at a sequence number
  (:told): A's own (evicted, missing from its store), and those B was
  reset past. Or would have: a GapFill A wrote or B read over messages A's
  store was asked to truncate before, the callback queued after the
  GapFill, which a kill may forestall. A GapFill B read that A never
  logged writing, killed first, follows a truncation of a process before
  any that may have written it (see writers); :perhaps one of a process
  that may have, the nodes' clocks not compared."
  [a-evs b-evs writer]
  (let [truncations (filter #(and (= "store" (:kind %)) (= "truncate" (:call %)) (= "begin" (:phase %))) a-evs)
        place (places a-evs)
        logged (set (keep #(when ((kind? "out") %) (:hash %)) a-evs))
        gap-fill? #(and (= "4" (:msgtype %)) (:gapfill %) (:newseqno %))
        ;; [epoch lo hi] of a GapFill over what tr truncated.
        filled (fn [gf tr]
                 (let [hi (dec (min (:newseqno gf) (:begin tr 0)))]
                   (when (and (= (:gen tr) (:gen gf)) (<= (:seq gf) hi)) [(:gen gf) (:seq gf) hi])))
        ;; A loss logged before OnLoss told its generation, of any epoch.
        told (concat
               (for [ev (concat (filter #(and ((kind? "recovery-loss") %) (not (re-find #"reset" (str (:err %))))) a-evs)
                                (filter #(and ((kind? "recovery-loss") %) (re-find #"reset" (str (:err %)))) b-evs))]
                 [(when (some? (:ours ev)) (:gen ev)) (:begin ev) (:end ev)])
               (for [gf (filter (kind? "out") a-evs)
                     :when (gap-fill? gf)
                     tr truncations
                     :when (<= (node-ns tr) (node-ns gf))
                     :let [r (filled gf tr)]
                     :when r]
                 r))
        ;; [range sure?]
        unlogged (for [gf (filter (kind? "in") b-evs)
                       :when (and (gap-fill? gf) (not (logged (:hash gf))))
                       :let [[lo hi] (writer gf)]
                       tr truncations
                       :let [p (place (:inc tr))]
                       :when (<= p hi)
                       :let [r (filled gf tr)]
                       :when r]
                   [r (< p lo)])
        sure (concat told (map first (filter second unlogged)))
        perhaps (map first (remove second unlogged))
        in? (fn [ranges [g s]] (some (fn [[lg lo hi]] (and (or (nil? lg) (= lg g)) (<= lo s hi))) ranges))]
    (fn [k] (cond (in? sure k) :told (in? perhaps k) :perhaps))))

;; Epochs

(defn- lives
  "Each process's store lifetime, from its start event: one for the whole
  node in a run that does not reset, a memstore started again then having
  to carry on, which the checks fail when it does not."
  [test evs]
  (into {} (for [inc-evs (incarnations evs)
                 :let [start (first (filter (kind? "start") inc-evs))]]
             [(:inc (first inc-evs)) (if (:reset test) (or (:life start) "") "")])))

(defn normalize
  "a's and b's events, for the messages from a to b: their generations
  replaced by a's epochs, [lifetime generation]. Each side numbers its
  generations apart, and a kill amid a reset puts them out of step. a's
  events by a's store lifetime; b's frames read by the frame a wrote with
  the same bytes, else by the one read before at the same sequence
  number in b's generation, else the last; what b did with a message
  (delivered, rejected, processed past it) by the frame read at its
  sequence number. b's own messages are b's: [:b lifetime generation].
  b's generation stays as :bgen."
  [test a-evs b-evs]
  (let [a-life (lives test a-evs)
        a-evs (mapv (fn [ev] (cond-> ev (:gen ev) (assoc :gen [(a-life (:inc ev)) (:gen ev)]))) a-evs)
        by-hash (into {} (for [ev a-evs :when (and ((kind? "out") ev) (:hash ev))] [(:hash ev) (:gen ev)]))
        b-life (lives test b-evs)
        step (fn [[segs out] ev]
               (if-let [g (:gen ev)]
                 (let [k [(b-life (:inc ev)) g]
                       {:keys [by-seq last]} (get segs k {:by-seq (sorted-map)})
                       own [:b (first k) g]
                       ;; Without resets both sides are at generation 1.
                       guess (if (:reset test) own ["" g])
                       at #(when % (get by-seq %))
                       below #(some-> (rsubseq by-seq < %) first val)
                       above #(some-> (subseq by-seq > %) first val)
                       e (case (:kind ev)
                           "in"            (or (by-hash (:hash ev)) (at (:seq ev)) last guess)
                           ("deliver" "rejected" "admin") (or (at (:seq ev)) last guess)
                           "out"           (if (:refseqnum ev) (or (at (:refseqnum ev)) last guess) own)
                           "recovery-loss" (if (:ours ev) own (or (above (:end ev)) (below (:begin ev)) last guess))
                           "store"         (if (and (#{"set-next-target" "open"} (:call ev)) (:target ev))
                                             (or (at (dec (:target ev))) (below (:target ev)) last guess)
                                             own)
                           own)
                       segs (if (and ((kind? "in") ev) (not= e own))
                              (assoc segs k {:by-seq (assoc by-seq (:seq ev) e) :last e})
                              segs)]
                   [segs (conj out (assoc ev :gen e :bgen g))])
                 [segs (conj out ev)]))]
    {:a a-evs :b (second (reduce step [{} []] b-evs))}))

(defn- probed
  "Whether one of b's processes owed delivering a slot ([epoch seq]) it
  read, a reset or not: it answered a TestRequest sent after it, which it
  does once it processed what came before in sequence (one behind a gap is
  held), then delivered a message it read after answering. Delivery is in
  order: the slot came first. A slot read with no such probe after it is
  not known accepted rather than held ahead of a gap, which a reset may
  drop."
  [b-incs]
  (let [proofs (for [inc-evs b-incs
                     :let [v (map-indexed vector inc-evs)
                           ;; Each reply paired with the request it answers:
                           ;; the last before it of its TestReqID on its
                           ;; connection (the liveness probes share one).
                           ;; The slots read before it.
                           [replies _ _] (reduce (fn [[out pending reads] [i ev]]
                                                   (cond
                                                     (and ((kind? "in") ev) (= "1" (:msgtype ev)) (:testreqid ev))
                                                     [out (assoc pending [(:epoch ev) (:testreqid ev)] ev) (conj reads [(:gen ev) (:seq ev)])]
                                                     ((kind? "in") ev)
                                                     [out pending (conj reads [(:gen ev) (:seq ev)])]
                                                     (and ((kind? "out") ev) (= "0" (:msgtype ev)) (:testreqid ev))
                                                     (let [pk [(:epoch ev) (:testreqid ev)]]
                                                       (if-let [tr (pending pk)]
                                                         [(conj out {:at i :gen (:gen tr) :below (:seq tr) :reads reads}) (dissoc pending pk) reads]
                                                         [out pending reads]))
                                                     :else [out pending reads]))
                                                 [[] {} #{}] v)
                           ;; The position of the first read of the last message
                           ;; it delivered: a later read is a duplicate.
                           last-read (first (reduce (fn [[lr first-in] [i ev]]
                                                      (let [k [(:gen ev) (:seq ev)]]
                                                        (cond ((kind? "in") ev) [lr (if (contains? first-in k) first-in (assoc first-in k i))]
                                                              ((kind? "deliver") ev) [(max lr (get first-in k -1)) first-in]
                                                              :else [lr first-in])))
                                                    [-1 {}] v))]
                     r replies
                     :when (< (:at r) last-read)]
                 r)]
    (fn [[g s :as k]] (some #(and (= g (:gen %)) (< s (:below %)) (contains? (:reads %) k)) proofs))))

(defn- epochs
  "a's epochs, in the order a's events first show them."
  [a-evs]
  (distinct (keep (fn [ev] (let [g (:gen ev)] (when (vector? g) g))) a-evs)))

;; One direction

(defn- direction
  "The checks of the messages from a to b."
  [test history evs a b markers]
  (let [{a-evs :a b-evs :b} (normalize test (evs a) (evs b))
        b-incs (incarnations b-evs)
        deliveries (filter (kind? "deliver") b-evs)
        ;; An op's epoch, by its order's Append, or out frame.
        op-epoch (into {} (for [ev a-evs
                                :when (or (and (= "store" (:kind ev)) (= "append" (:call ev))) ((kind? "out") ev))
                                :when (:clordid ev)]
                            [(:clordid ev) (:gen ev)]))
        op-key (fn [op] [(or (op-epoch (:value op)) [(if (:reset test) :unknown "") (:gen op)]) (:seq op)])
        ;; The marker closes its epoch below it; the epochs before it a
        ;; reset ended.
        marker (get markers a)
        [marker-gen marker-seq] (some #(when (= marker (:clordid %)) [(:gen %) (:seq %)]) deliveries)
        order (into {} (map-indexed (fn [i e] [e i]) (epochs a-evs)))
        ended? (fn [g] (when-let [i (order g)] (some #(< i %) (vals order))))
        owed? (fn [[g s]] (cond (nil? marker-seq)  true
                                (= g marker-gen)   (< s marker-seq)
                                :else (and (order g) (order marker-gen) (< (order g) (order marker-gen)))))
        read (set (for [ev b-evs :when ((kind? "in") ev)] [(:gen ev) (:seq ev)]))
        ;; A reset does not drop what b queued.
        owed-by-b? (probed b-incs)
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
        ;; is by a recovery loss, or a Reject b's session says counted it;
        ;; only :unknown by a rejection b's process may have ended before
        ;; telling (see cut-short).
        writer (writers a-evs b-evs)
        recovery (recovery-losses a-evs b-evs writer)
        all-rejects (rejects a-evs b-evs)
        ;; How many times a message was rejected for its SendingTime: as b
        ;; wrote it, as a read it, whichever saw more.
        times-rejected (merge-with max
                                   (frequencies (for [r (rejects [] b-evs) :when (sending-time-reject? r)] (:key r)))
                                   (frequencies (for [r (rejects a-evs []) :when (sending-time-reject? r)] (:key r))))
        processed (reduce (fn [m [g s]] (update m g (fnil max 0) s))
                          {}
                          (concat (for [ev b-evs
                                        :when (and (= "store" (:kind ev)) (= "set-next-target" (:call ev)) (= "begin" (:phase ev)))]
                                    [(:gen ev) (:target ev 0)])
                                  (for [d deliveries] [(:gen d) (:seq d)])))
        processed-past? (fn [[g s]] (< s (get processed g 0)))
        max-latency-ms (* 1000 (:max-latency test 120))
        stale-at? (fn [ev ^java.time.Instant now]
                    (let [st (instant (:sendingtime ev))]
                      (and (order-frame? ev) st now
                           (<= max-latency-ms (Math/abs (- (.toEpochMilli now) (.toEpochMilli st)))))))
        ;; b's session tells (RejectObserver) of each Reject it sends, and
        ;; whether it counted the message as received: one it did is never
        ;; delivered. A message may be rejected more than once, ahead of a
        ;; gap then in sequence.
        notes (group-by (juxt :gen :seq) (filter (kind? "rejected") b-evs))
        ;; A frame with its SendingTime too far off that b's process ended
        ;; on, no word of it after: killed handling it, rejected or not, no
        ;; Reject written perhaps. One followed by another frame was
        ;; handled: rejected with word of it, or ignored (from a connection
        ;; no longer current, after the peer's Logout). The session dates
        ;; a frame before the observer does, once the frame before is
        ;; handled: the process paused in between perhaps, the clock
        ;; changed, more than once: stale by any offset the process had, at
        ;; either end. The real clock stepped too (system-clock): read
        ;; between the frame before and the event after ev, by any offset
        ;; against the monotonic clock the process was seen at meanwhile.
        ;; A step made and undone with no event in between is not seen: a
        ;; rejection then may be called a loss, a loss never excused.
        system-clock? (contains? (:faults test) :system-clock)
        cut-short (set (for [inc-evs b-incs
                             :let [[after [ev & before]] (split-with (complement (kind? "in")) (reverse inc-evs))]
                             :when (and ev (:t ev) (:now ev))
                             :let [lo-ev (or (first (filter (kind? "in") before)) (last before) ev)
                                   hi (instant (:t ev))
                                   lo (instant (:t lo-ev))
                                   session (into (hash-set 0 (- (.toEpochMilli (instant (:now ev))) (.toEpochMilli hi)))
                                                 (mapcat (juxt #(:was_ms % 0) #(:offset_ms % 0)))
                                                 (filter (kind? "clock") before))
                                   ;; When the session may have read its
                                   ;; clock, without and with the session's
                                   ;; offsets.
                                   reads (if (and system-clock? (:mono lo-ev) (:mono ev))
                                           (let [end (or (last after) ev)
                                                 seen (into #{} (keep #(when (<= (node-ns lo-ev) (node-ns %) (node-ns end)) (wall-offset %)))
                                                            inc-evs)]
                                             (for [m [(:mono lo-ev) (:mono ev)] w seen]
                                               (java.time.Instant/ofEpochMilli (+ (quot (long m) 1000000) w))))
                                           [lo hi])]
                             :when (some (fn [o] (some #(stale-at? ev (.plusMillis ^java.time.Instant % (long o))) reads)) session)
                             :when (not-any? #(and ((kind? "rejected") %) (= [(:gen ev) (:seq ev)] [(:gen %) (:seq %)]))
                                             after)]
                         [(:gen ev) (:seq ev)]))
        excused (fn [[g _ :as k]]
                  (cond (= :told (recovery k))           :lost
                        ;; Abandoned by a reset that ended its epoch,
                        ;; before b processed past it, or queued it.
                        (and (ended? g) (not (processed-past? k)) (not (owed-by-b? k))) :reset
                        (and (processed-past? k) (some :counted (notes k))) :lost
                        (recovery k)                     :unknown
                        (not (processed-past? k))        nil
                        ;; A rejection with no word of it: b killed in
                        ;; between, counted or not.
                        (or (< (count (notes k)) (times-rejected k 0))
                            (cut-short k))               :unknown))

        ;; 1. Order and duplicates, within each of b's processes and
        ;; generations: b reset, a may resend what it delivered.
        disorder (for [inc-evs b-incs
                       :let [ds (filter (kind? "deliver") inc-evs)]
                       [x y] (partition 2 1 ds)
                       :when (and (= (:bgen x) (:bgen y)) (<= (:seq y) (:seq x)))]
                   {:inc (:inc y) :after (select-keys x [:seq :clordid]) :delivered (select-keys y [:seq :clordid])})
        duplicates (for [inc-evs b-incs
                         [[_ id] n] (frequencies (map (juxt :bgen :clordid) (filter (kind? "deliver") inc-evs)))
                         :when (< 1 n)]
                     {:inc (:inc (first inc-evs)) :clordid id :times n})

        ;; 2. Redelivery after a restart. gen → MsgSeqNum → the orders
        ;; written and not excused as lost, sorted, for range lookups; one
        ;; excused only perhaps makes a gap over it :unknown.
        pending (reduce (fn [m [[g s :as k] id]]
                          (let [why (excused k)]
                            (if (#{:lost :reset} why)
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
                             ;; The highest delivered of its epoch, in b's
                             ;; generation: b reset, a resends from 1.
                             m (get highest [(:gen d0) (:bgen d0)] 0)
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
                                        (when (and d0 (not (unsynced-lost? test)) (<= (:seq d0) m)
                                                   (<= redelivery-bound (- m (:seq d0))))
                                          [{:inc (:inc d0) :problem :redelivered-too-much :from (:seq d0) :highest m}]))]
                         (recur (rest incs)
                                (into before (for [d ds] [[(:gen d) (:seq d)] [(:clordid d) (:digest d)]]))
                                (reduce (fn [h d] (bump h [[(:gen d) (:bgen d)] (:seq d)])) highest ds)
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
        ;; Acknowledged, b's engine read it: one it did not is not
        ;; excused, whatever lost it after.
        undelivered (for [op acked
                          :let [k (op-key op)]
                          :when (not (some #(= (:value op) (first %)) (get closed k)))]
                      (assoc (select-keys op [:value :gen :seq :process])
                             :excused (when (read k) (excused k))
                             :read (boolean (read k))))

        ;; 5. Nothing lost silently.
        durable-orders (into {} (filter val (:durable stores)))
        owed (merge (if (unsynced-lost? test) {} (into {} (filter val (:appended stores))))
                    durable-orders
                    written)
        silent (for [[[g s :as k] id] owed
                     :when (owed? k)
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
                               :when (not (apply covered? (op-key op)))]
                           (select-keys op [:value :gen :seq]))

        ;; 10. A loss told ErrBeyondRetention is of messages a's store
        ;; was asked to truncate before, failed or not (a failed Truncate
        ;; may have dropped them), of its epoch: a resend must not give up
        ;; on what it still has. A loss logged before OnLoss told its
        ;; generation, of any.
        truncations (filter #(and (= "store" (:kind %)) (= "truncate" (:call %)) (= "begin" (:phase %))) a-evs)
        unevicted (for [ev a-evs
                        :when ((kind? "recovery-loss") ev)
                        :when (str/includes? (str (:err ev)) retention-error)
                        :when (not-any? #(and (< (:end ev) (:begin % 0))
                                              (or (nil? (:gen ev)) (= (:gen ev) (:gen %)))
                                              (<= (node-ns %) (node-ns ev)))
                                        truncations)]
                    (select-keys ev [:inc :begin :end]))

        ;; 7. Sequence numbers survive: every Append, session messages
        ;; included, without power loss; what was durable with it; and
        ;; whatever b read of an earlier process: :unknown if b read it of
        ;; a process that may have been this one.
        forgotten (for [[p {:keys [seen maybe]}] (map vector (:processes stores) (seen-before (:processes stores) writer b-evs))
                        :let [open (:open p)
                              g (:gen open)
                              kept (if (unsynced-lost? test) (:durable p) (:appended p))
                              highest (max (get kept g 0) (get seen g 0))
                              perhaps (max highest (get maybe g 0))]
                        :when (and open (<= (:next open 0) perhaps))]
                    (cond-> {:inc (:inc p) :next-sender (:next open) :highest-before highest
                             :kept (get kept g 0) :seen (get seen g 0)}
                      (< highest (:next open 0)) (assoc :unknown true :maybe-seen (get maybe g 0))))
        ;; ...and so does how far a recorded processing what b sent: not
        ;; below a durable prefix, nor, without power loss, a committed one.
        prefix-forgotten (for [p (target-history a-evs)
                               :let [open (:open p)
                                     g (:gen open)
                                     kept (get (if (unsynced-lost? test) (:durable p) (:committed p)) g 0)]
                               :when (and open (< (:target open 0) kept))]
                           {:inc (:inc p) :problem :processed-prefix-forgotten :next-target (:target open) :kept kept})

        ;; 8. Halts: nothing stored after, no send begun after succeeding.
        halts (for [inc-evs (incarnations a-evs)
                    :let [halt (first (filter (kind? "halt") inc-evs))]
                    :when halt
                    :let [halted (node-ns halt)
                          after (filter #(< halted (node-ns %)) inc-evs)
                          began (set (for [ev after :when (and ((kind? "send") ev) (= "begin" (:phase ev)))] (:clordid ev)))]
                    problem (concat
                              (when-not (:disk-faults test) [{:problem :halted :err (:err halt)}])
                              (for [ev after :when (and (= "store" (:kind ev)) (= "append" (:call ev)) (= "begin" (:phase ev)))]
                                {:problem :appended-after-halt :seq (:seq ev)})
                              (for [ev after
                                    :when (and ((kind? "send") ev) (= "end" (:phase ev)) (= "ok" (:type ev)) (began (:clordid ev)))]
                                {:problem :sent-after-halt :clordid (:clordid ev)})
                              (for [ev after :when (and ((kind? "out") ev) (= "A" (:msgtype ev)))]
                                {:problem :logon-after-halt :seq (:seq ev)}))]
                (assoc problem :inc (:inc halt)))
        ;; ...and a store write under the session's lock still running past
        ;; 1.5 StoreTimeout halts it by then (the halt event comes from a
        ;; watcher polling every 100ms: 300ms more allowed).
        store-timeout-ms (let [st (:store-timeout test 0)] (if (pos? st) st 30000))
        unhalted (for [inc-evs (incarnations a-evs)
                       :let [v (vec inc-evs)
                             halt (some #(when ((kind? "halt") %) %) v)
                             last-ev (peek v)
                             limit (long (+ (* 1.5 store-timeout-ms) 300))]
                       [i ev] (map-indexed vector v)
                       :when (and (= "store" (:kind ev)) (= "begin" (:phase ev))
                                  (#{"append" "set-next-target" "set-next-sender" "reset" "truncate"} (:call ev)))
                       :let [end (some #(when (and (= "store" (:kind %)) (= "end" (:phase %)) (= (:call ev) (:call %))) %)
                                       (subvec v (inc i)))
                             ms (ms-between ev (or end last-ev))]
                       ;; Hung past the limit, its process alive that long.
                       :when (< limit ms)
                       :when (or (nil? halt) (< limit (ms-between ev halt)))]
                   {:inc (:inc ev) :problem :not-halted-past-store-timeout :call (:call ev) :seq (:seq ev) :ran-ms ms})

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
                     :acknowledged (count acked) :undelivered-count (count lost) :undelivered (take 16 lost)
                     :reset-abandoned (count (filter #(= :reset (:excused %)) undelivered))})
     :silent-loss (let [lost (remove :excused silent)]
                    {:valid? (worst lost (filter #(= :unknown (:excused %)) silent))
                     :owed (count owed) :lost-count (count lost) :lost (take 16 lost)
                     :reset-abandoned (count (filter #(= :reset (:excused %)) silent))
                     :unknown (take 16 (filter #(= :unknown (:excused %)) silent))})
     :skipped     {:valid? (empty? skipped-unfilled) :unfilled (take 16 skipped-unfilled)}
     :retention   {:valid? (empty? unevicted) :unevicted (take 16 unevicted)}
     :durability  (let [all (concat forgotten prefix-forgotten)]
                    {:valid? (worst (remove :unknown all) (filter :unknown all)) :forgotten (take 16 all)})
     :halts       (let [all (concat halts unhalted)]
                    {:valid? (empty? all) :problems (take 16 all)})
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
  "Checks history against evs, each node's events in order, session by
  session: each has its sends (:session, 0 if none), events (:session, 0 if
  none) and markers."
  [test history evs]
  (let [quiesce (->> history (filter #(= :quiesce (:f %))) (remove h/invoke?) first)
        live (= :ok (:type quiesce))
        markers (get-in quiesce [:value :markers])
        [n1 n2] (:nodes test)
        ks (range (:fix-sessions test 1))
        label (fn [k d] (if (= [0] ks) d (str "s" k " " d)))
        per (for [k ks]
              (let [in? #(= k (:session % 0))
                    h (filter #(or (not= :send (:f %)) (in? %)) history)
                    e (update-vals evs #(filterv in? %))
                    m (into {} (for [n [n1 n2]] [n (get markers (client/marker-key n k))]))]
                {:k k :history h :evs e
                 :dirs {(label k (str n1 "->" n2)) (direction test h e n1 n2 m)
                        (label k (str n2 "->" n1)) (direction test h e n2 n1 m)}}))
        dirs (into {} (mapcat :dirs per))
        phantom-checks (for [{:keys [k history evs]} per] [k (phantoms test history evs)])
        checks (into {:live {:valid? live :quiesce quiesce}
                      :phantoms (if (= [0] ks)
                                  (second (first phantom-checks))
                                  (assoc (into {} (for [[k r] phantom-checks] [(label k "") r]))
                                         :valid? (checker/merge-valid (map (comp :valid? second) phantom-checks))))}
                     (for [check [:order :redelivery :identity :acked :silent-loss :skipped :retention :durability :halts :rejects]]
                       [check (let [by-dir (into {} (for [[d r] dirs] [d (r check)]))]
                                (assoc by-dir :valid? (checker/merge-valid (map :valid? (vals by-dir)))))]))]
    (assoc checks :valid? (checker/merge-valid (map :valid? (vals checks))))))

(defn checker
  []
  (reify checker/Checker
    (check [_ test history opts] (analyze test history (events/all test)))))
