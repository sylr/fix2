(ns jepsen.fix2.checker-test
  "The checks, fed a clean run and runs that break each of them."
  (:require [clojure.test :refer [deftest is testing]]
            [jepsen.fix2.checker :as c]))

(def test-map {:nodes ["n1" "n2"] :concurrency 2 :max-latency 120})

(def ^:private clock (atom 0))

(defn- t
  "Event times, rising."
  []
  (str (java.time.Instant/ofEpochMilli (+ 1700000000000 (swap! clock inc)))))

(defn- ev [inc kind & {:as fields}] (merge {:inc inc :kind kind :t (t) :gen 1} fields))

(defn- each
  "mapcat, eager: events are stamped when made, which must follow the
  order they are laid out in."
  [f xs]
  (vec (mapcat f xs)))

(defn- digest [id] (str "d-" id))

(defn- appended [inc id seq]
  [(ev inc "store" :call "append" :phase "begin" :seq seq :clordid id :digest (digest id))
   (ev inc "store" :call "append" :phase "end" :seq seq :clordid id :digest (digest id))])

(defn- synced [inc cutoff]
  [(ev inc "store" :call "sync" :phase "begin" :cutoff cutoff)
   (ev inc "store" :call "sync" :phase "end" :cutoff cutoff)])

(defn- written [inc id seq] (ev inc "out" :msgtype "D" :seq seq :clordid id :digest (digest id)))

(defn- sent
  "An order sent: appended, synced, written."
  [inc id seq]
  (concat (appended inc id seq) (synced inc seq) [(written inc id seq)]))

(defn- received [inc id seq] (ev inc "in" :msgtype "D" :seq seq :clordid id :digest (digest id)))

(defn- stale-received
  "Received with its SendingTime ten minutes off."
  [inc id seq]
  (assoc (received inc id seq) :sendingtime "2023-11-14T22:00:00Z" :now "2023-11-14T22:10:00Z"))

(defn- processed
  "B's store recording it processed everything below target."
  [inc target]
  (ev inc "store" :call "set-next-target" :phase "end" :target target))

(defn- rejected
  "B rejecting A's message at seq, for reason."
  [inc seq reason]
  (ev inc "out" :msgtype "3" :seq 9 :refseqnum seq :rejectreason reason))

(defn- delivered [inc id seq] (ev inc "deliver" :seq seq :clordid id :digest (digest id)))

(defn- started [inc next]
  [(ev inc "start")
   (ev inc "store" :call "open" :phase "end" :next next :target 1)])

(defn- marker-sent [inc id seq]
  (cons (ev inc "send" :phase "begin" :clordid id) (sent inc id seq)))

(def m1 "BARRIER-n1-1-1")
(def m2 "BARRIER-n2-1-1")

(defn- n2-receives
  "n2 receiving and delivering orders, then n1's marker at mseq, and
  sending its own, its process started with start: made before any of
  n1's events, so that times rise as in a real run."
  [start orders mseq]
  (vec (concat start
               [(ev "n2-1" "in" :msgtype "A" :seq 1)
                (ev "n2-1" "out" :msgtype "A" :seq 1)]
               (each (fn [[id s]] [(received "n2-1" id s) (delivered "n2-1" id s)]) orders)
               (marker-sent "n2-1" m2 2)
               [(received "n2-1" m1 mseq)
                (delivered "n2-1" m1 mseq)])))

(defn- n1-tail
  "n1 receiving n2's marker."
  [inc]
  [(received inc m2 2) (delivered inc m2 2)])

(defn clean
  "n1 sends O1–O3 at 2–4 and its marker at 5; n2 its marker at 2."
  []
  (let [orders [["O1" 2] ["O2" 3] ["O3" 4]]
        s1 (started "n1-1" 1)
        s2 (started "n2-1" 1)]
    {"n1" (vec (concat s1
                       [(ev "n1-1" "out" :msgtype "A" :seq 1)
                        (ev "n1-1" "in" :msgtype "A" :seq 1)]
                       (each (fn [[id s]] (sent "n1-1" id s)) orders)
                       (marker-sent "n1-1" m1 5)
                       (n1-tail "n1-1")))
     "n2" (n2-receives s2 orders 5)}))

(defn- acked [id s] [{:type :invoke :f :send :value id :process 0}
                     {:type :ok :f :send :value id :process 0 :node "n1" :gen 1 :seq s}])

(def quiesced [{:type :invoke :f :quiesce :process 1}
               {:type :ok :f :quiesce :process 1 :value {:markers {"n1" m1 "n2" m2}}}])

(defn history
  "n1's sends acknowledged, then the quiesce op."
  []
  (vec (concat (acked "O1" 2) (acked "O2" 3) (acked "O3" 4) quiesced)))

(defn- check
  ([evs] (check test-map (history) evs))
  ([test history evs] (c/analyze test history evs)))

(defn- valid [res k] (get-in res [k :valid?]))

(defn- remove-ev [evs node pred] (update evs node #(vec (remove pred %))))

(defn- delivery? [id] #(and (= "deliver" (:kind %)) (= id (:clordid %))))

(deftest clean-run
  (let [res (check (clean))]
    (is (true? (:valid? res)) (pr-str res))))

(deftest order
  (testing "a duplicate within a process"
    (is (false? (valid (check (update (clean) "n2" (fn [es] (vec (mapcat #(if ((delivery? "O2") %) [% %] [%]) es))))) :order))))
  (testing "out of order"
    (let [swap (fn [es] (let [i (.indexOf es (first (filter (delivery? "O2") es)))
                              j (.indexOf es (first (filter (delivery? "O3") es)))]
                          (assoc es i (es j) j (es i))))]
      (is (false? (valid (check (update (clean) "n2" swap)) :order))))))

(defn- n2-restarted
  "n2 killed after delivering O1, its next process delivering from first."
  [first-id]
  (let [evs (clean)
        [before after] (split-with (complement (delivery? "O2")) (evs "n2"))]
    (assoc evs "n2" (vec (concat before
                                 (remove #(= "deliver" (:kind %)) after)
                                 (started "n2-2" 3)
                                 [(delivered "n2-2" first-id 2)
                                  (delivered "n2-2" "O2" 3)
                                  (delivered "n2-2" "O3" 4)
                                  (delivered "n2-2" m1 5)])))))

(deftest redelivery
  (testing "the same message delivered again after a restart: valid"
    (let [res (check (n2-restarted "O1"))]
      (is (true? (valid res :redelivery)))
      (is (true? (valid res :order)))))
  (testing "another message at a number delivered before"
    (is (false? (valid (check (n2-restarted "X1")) :redelivery))))
  (testing "the first message skipped by the first process"
    (is (false? (valid (check (remove-ev (clean) "n2" (delivery? "O1"))) :redelivery))))
  (testing "a message skipped at the restart"
    (let [evs (clean)
          kept (vec (take-while (complement (delivery? "O2")) (evs "n2")))
          evs (assoc evs "n2" (vec (concat kept (started "n2-2" 3) [(delivered "n2-2" "O3" 4) (delivered "n2-2" m1 5)])))]
      (is (false? (valid (check evs) :redelivery)))))
  (testing "after a kill, at most 64 MsgSeqNums from the highest delivered"
    (let [orders (for [i (range 2 80)] [(str "P" i) i])
          run (fn [from]
                {"n1" (vec (concat (started "n1-1" 1) (each (fn [[id s]] (sent "n1-1" id s)) orders)
                                   (marker-sent "n1-1" m1 80) (n1-tail "n1-1")))
                 "n2" (vec (concat (started "n2-1" 1)
                                   (each (fn [[id s]] [(received "n2-1" id s) (delivered "n2-1" id s)]) orders)
                                   (started "n2-2" 2)
                                   (vec (for [[id s] orders :when (<= from s)] (delivered "n2-2" id s)))
                                   (marker-sent "n2-2" m2 2)
                                   [(delivered "n2-2" m1 80)]))})
          h (vec (concat (for [[id s] orders op (acked id s)] op) quiesced))]
      (testing "63 before: valid"
        (is (true? (valid (check test-map h (run 16)) :redelivery))))
      (testing "64 before: too many"
        (is (false? (valid (check test-map h (run 15)) :redelivery))))
      (testing "77 before, after a power loss: valid"
        (is (true? (valid (check (assoc test-map :power-loss true) h (run 2)) :redelivery)))))))

(deftest identity-per-seq
  (is (false? (valid (check (update (clean) "n1" conj (written "n1-2" "X" 3))) :identity))))

(deftest acked-delivered
  (testing "an acknowledged order never delivered"
    (is (false? (valid (check (remove-ev (clean) "n2" (delivery? "O1"))) :acked))))
  (testing "delivered after the closing marker only"
    (let [evs (-> (clean) (remove-ev "n2" (delivery? "O1")) (update "n2" conj (delivered "n2-1" "O1" 2)))]
      (is (false? (valid (check evs) :acked)))))
  (testing "reported lost by the sender: excused"
    (let [evs (-> (clean) (remove-ev "n2" (delivery? "O1"))
                  (update "n1" conj (ev "n1-1" "recovery-loss" :begin 2 :end 2 :err "session: messages evicted from the store gap-filled")))
          res (check evs)]
      (is (true? (valid res :acked)))
      (is (true? (valid res :silent-loss)))))
  (testing "rejected for its SendingTime, processed past: unknown, consumed or not cannot be told yet"
    (let [evs (-> (clean) (remove-ev "n2" (delivery? "O1"))
                  (update "n2" into [(rejected "n2-1" 2 "10") (processed "n2-1" 5)]))
          res (check evs)]
      (is (= :unknown (valid res :acked)) (pr-str (:acked res)))
      (is (= :unknown (valid res :silent-loss)))
      (is (true? (valid res :rejects)))))
  (testing "rejected, the receiver saying it counted it: excused"
    (let [evs (-> (clean) (remove-ev "n2" (delivery? "O1"))
                  (update "n2" into [(rejected "n2-1" 2 "10") (ev "n2-1" "rejected" :seq 2 :rejectreason "10" :counted true)]))
          res (check evs)]
      (is (true? (valid res :acked)))
      (is (true? (valid res :silent-loss)))
      (is (true? (valid res :redelivery)))))
  (testing "rejected, the receiver saying it did not count it, never delivered after: lost"
    (let [evs (-> (clean) (remove-ev "n2" (delivery? "O1"))
                  (update "n2" into [(rejected "n2-1" 2 "10") (ev "n2-1" "rejected" :seq 2 :rejectreason "10")]))]
      (is (false? (valid (check evs) :acked)))
      (is (false? (valid (check evs) :silent-loss)))))
  (testing "rejected ahead of a gap, not counted, then in sequence with no word of it: unknown"
    (let [evs (-> (clean) (remove-ev "n2" (delivery? "O1"))
                  (update "n2" into [(rejected "n2-1" 2 "10") (ev "n2-1" "rejected" :seq 2 :rejectreason "10")
                                     (rejected "n2-1" 2 "10") (processed "n2-1" 5)]))]
      (is (= :unknown (valid (check evs) :acked)))
      (is (= :unknown (valid (check evs) :silent-loss)))))
  (testing "its SendingTime too far off, the receiver's process ending on it: unknown"
    (let [evs (-> (clean) (remove-ev "n2" (delivery? "O1")) (update "n2" conj (stale-received "n2-1" "O1" 2)))]
      (is (= :unknown (valid (check evs) :acked)))))
  (testing "fresh as logged, the process ending on it, but stale by the clock before its last change: unknown"
    (let [fresh (assoc (received "n2-1" "O1" 2) :sendingtime "2023-11-14T22:13:20Z" :now "2023-11-14T22:13:21Z")
          evs (-> (clean) (remove-ev "n2" (delivery? "O1")))]
      (is (= :unknown (valid (check (update evs "n2" into [(ev "n2-1" "clock" :was_ms 600000) fresh])) :acked)))
      (testing "two changes, the last from 0"
        (is (= :unknown (valid (check (update evs "n2" into [(ev "n2-1" "clock" :offset_ms 600000)
                                                             (ev "n2-1" "clock" :was_ms 600000)
                                                             (ev "n2-1" "clock" :offset_ms 3000)
                                                             fresh]))
                               :acked))))
      (testing "fresh as logged, but logged after a pause: unknown"
        (let [late (assoc (received "n2-1" "O1" 2) :t "2023-11-14T22:30:00Z"
                          :sendingtime "2023-11-14T22:29:00Z" :now "2023-11-14T22:30:00Z")]
          (is (= :unknown (valid (check (update evs "n2" conj late)) :acked)))))
      (testing "no clock change: lost"
        (is (false? (valid (check (update evs "n2" conj fresh)) :acked))))))
  (testing "rejected, not counted, then received again and ignored, handled since: lost"
    (let [evs (-> (clean) (remove-ev "n2" (delivery? "O1"))
                  (update "n2" into [(stale-received "n2-1" "O1" 2) (ev "n2-1" "rejected" :seq 2 :rejectreason "10")
                                     (stale-received "n2-1" "O1" 2) (received "n2-1" "O9" 99)]))]
      (is (false? (valid (check evs) :acked)))
      (is (false? (valid (check evs) :silent-loss)))))
  (testing "rejected for its SendingTime, the receiver never past it: lost"
    (let [evs (-> (clean) (remove-ev "n2" #(= "deliver" (:kind %))) (update "n2" conj (rejected "n2-1" 2 "10")))]
      (is (false? (valid (check evs) :acked)))))
  (testing "processed past without a Reject nor a delivery"
    (let [evs (-> (clean) (remove-ev "n2" (delivery? "O1")) (update "n2" conj (processed "n2-1" 5)))]
      (is (false? (valid (check evs) :acked)))
      (is (false? (valid (check evs) :silent-loss)))))
  (testing "processed past by a SetNextTarget that failed: still processed past"
    (let [evs (-> (clean) (remove-ev "n2" #(= "deliver" (:kind %)))
                  (update "n2" into [(rejected "n2-1" 2 "10") (rejected "n2-1" 3 "10") (rejected "n2-1" 4 "10")
                                     (ev "n2-1" "store" :call "set-next-target" :phase "begin" :target 5)]))]
      (is (= :unknown (valid (check evs) :silent-loss))))))

(deftest retention
  (let [lost (fn [evs] (-> evs (remove-ev "n2" (delivery? "O1"))
                           (update "n1" conj (ev "n1-1" "recovery-loss" :begin 2 :end 2 :err "session: messages evicted from the store gap-filled"))))]
    (testing "a loss told ErrBeyondRetention, the store never truncated"
      (is (false? (valid (check (lost (clean))) :retention))))
    (testing "truncated below it"
      (is (false? (valid (check (lost (update (clean) "n1" into [(ev "n1-1" "store" :call "truncate" :phase "begin" :begin 2)]))) :retention))))
    (testing "truncated past it"
      (is (true? (valid (check (lost (update (clean) "n1" into [(ev "n1-1" "store" :call "truncate" :phase "begin" :begin 3)]))) :retention))))
    (testing "gap-filled over messages truncated, the sender killed before telling: excused"
      (let [evs (-> (clean) (remove-ev "n2" (delivery? "O1"))
                    (update "n1" into [(ev "n1-1" "store" :call "truncate" :phase "begin" :begin 3)
                                       (ev "n1-1" "out" :msgtype "4" :seq 2 :gapfill true :newseqno 3)]))]
        (is (true? (valid (check evs) :acked)))
        (is (true? (valid (check evs) :silent-loss)))
        (testing "not truncated: lost"
          (is (false? (valid (check (update evs "n1" (fn [es] (vec (remove #(= "truncate" (:call %)) es))))) :acked))))))
    (testing "truncated past it, but after the loss"
      (is (false? (valid (check (update (lost (clean)) "n1" conj (ev "n1-1" "store" :call "truncate" :phase "begin" :begin 3))) :retention))))))

(deftest rejects
  (testing "a Reject of a valid order, but for its SendingTime"
    (is (false? (valid (check (update (clean) "n2" conj (rejected "n2-1" 3 "5"))) :rejects))))
  (testing "a forbidden Reject not hidden by a later one for SendingTime"
    (is (false? (valid (check (update (clean) "n2" into [(rejected "n2-1" 3 "5") (rejected "n2-1" 3 "10")])) :rejects)))))

(defn- power-lost
  "n1 appends O3 at 4, synced or not, never writes it, and loses power;
  its next process opens at next and sends its marker there."
  [o3-synced? next]
  (let [s2 (started "n2-1" 1)
        n1 (vec (concat (started "n1-1" 1)
                        (sent "n1-1" "O1" 2) (sent "n1-1" "O2" 3)
                        (appended "n1-1" "O3" 4)
                        (when o3-synced? (synced "n1-1" 4))
                        (started "n1-2" next)
                        (marker-sent "n1-2" m1 next)
                        (n1-tail "n1-2")))]
    {"n1" n1 "n2" (n2-receives s2 [["O1" 2] ["O2" 3]] next)}))

(def power-loss-history (vec (concat (acked "O1" 2) (acked "O2" 3) quiesced)))

(deftest silent-loss
  (testing "unsynced, unwritten, lost to a power loss, its number reused: not owed"
    (let [res (check (assoc test-map :power-loss true) power-loss-history (power-lost false 4))]
      (is (true? (valid res :silent-loss)) (pr-str (:silent-loss res)))
      (is (true? (valid res :durability)))))
  (testing "the same without power loss: a kill loses no commit, the number reused is a durability bug"
    (is (false? (valid (check test-map power-loss-history (power-lost false 4)) :durability))))
  (testing "durable, never written, never delivered"
    (let [res (check (assoc test-map :power-loss true) power-loss-history (power-lost true 5))]
      (is (false? (valid res :silent-loss)))
      (is (true? (valid res :durability)))))
  (testing "seen past MaxLatency, processed past, handled with no word of a Reject: lost"
    (let [stale (fn [es] (mapv #(if (and (= "in" (:kind %)) (= "O1" (:clordid %)))
                                  (assoc % :sendingtime "2026-10-18T10:00:00Z" :now "2026-10-18T10:05:00Z")
                                  %)
                               es))
          evs (-> (clean) (remove-ev "n2" (delivery? "O1")) (update "n2" stale) (update "n2" conj (processed "n2-1" 5)))
          res (check evs)]
      (is (false? (valid res :silent-loss)))
      (is (false? (valid res :acked)))
      (is (false? (valid res :redelivery)))
      (testing "but not processed past: lost"
        (is (false? (valid (check (-> (clean) (remove-ev "n2" #(= "deliver" (:kind %))) (update "n2" stale))) :silent-loss))))
      (testing "stale but delivered after all: definite"
        (is (true? (valid (check (update (clean) "n2" stale)) :redelivery))))))
  (testing "an Append whose end a kill took, found committed by the next Open, then synced"
    (let [s2 (started "n2-1" 1)
          evs {"n1" (vec (concat (started "n1-1" 1)
                                 (sent "n1-1" "O1" 2) (sent "n1-1" "O2" 3)
                                 [(ev "n1-1" "store" :call "append" :phase "begin" :seq 4 :clordid "O3" :digest (digest "O3"))]
                                 (started "n1-2" 5)
                                 (marker-sent "n1-2" m1 5)
                                 (n1-tail "n1-2")))
               "n2" (n2-receives s2 [["O1" 2] ["O2" 3]] 5)}]
      (is (false? (valid (check (assoc test-map :power-loss true) power-loss-history evs) :silent-loss)))
      (is (false? (valid (check test-map power-loss-history evs) :silent-loss))))))

(deftest durability
  (testing "restarted on a NextSender below what was durable"
    (let [res (check (assoc test-map :power-loss true) power-loss-history (power-lost true 4))]
      (is (false? (valid res :durability)))
      (is (= 4 (-> res :durability (get "n1->n2") :forgotten first :kept)))))
  (testing "restarted below what the counterparty saw, the sender's store saying nothing"
    (let [res (check (-> (clean) (remove-ev "n1" #(= "store" (:kind %))) (update "n1" into (started "n1-2" 3))))]
      (is (false? (valid res :durability)))
      (is (= 5 (-> res :durability (get "n1->n2") :forgotten first :seen)))))
  (testing "restarted past both"
    (is (true? (valid (check (update (clean) "n1" into (started "n1-2" 6))) :durability))))
  (testing "a session message appended, never written, its number reused after a kill"
    (let [evs (-> (clean)
                  (update "n1" conj (ev "n1-1" "store" :call "append" :phase "end" :seq 6))
                  (update "n1" into (started "n1-2" 6)))]
      (is (false? (valid (check evs) :durability)))))
  (testing "a processed prefix synced, then reopened below"
    (let [evs (-> (clean)
                  (update "n1" into (concat [(processed "n1-1" 3)] (synced "n1-1" 5)))
                  (update "n1" into (started "n1-2" 6)))
          evs (update evs "n1" (fn [es] (mapv #(if (and (= "n1-2" (:inc %)) (= "open" (:call %))) (assoc % :target 2) %) es)))
          res (check (assoc test-map :power-loss true) (history) evs)]
      (is (false? (valid res :durability)))
      (is (= :processed-prefix-forgotten (some :problem (-> res :durability (get "n1->n2") :forgotten))))))
  (testing "a processed prefix committed, not synced, below after a power loss: valid; after a kill: forgotten"
    (let [evs (-> (clean)
                  (update "n1" conj (processed "n1-1" 3))
                  (update "n1" into (started "n1-2" 6)))
          evs (update evs "n1" (fn [es] (mapv #(if (and (= "n1-2" (:inc %)) (= "open" (:call %))) (assoc % :target 2) %) es)))]
      (is (true? (valid (check (assoc test-map :power-loss true) (history) evs) :durability)))
      (is (false? (valid (check test-map (history) evs) :durability)))))
  (testing "an Append an earlier process made, synced by a later one, forgotten after a power loss"
    (let [evs (-> (clean)
                  (update "n1" into (appended "n1-1" "O4" 6))           ; unsynced
                  (update "n1" into (started "n1-2" 7))                 ; it survived
                  (update "n1" into (concat (appended "n1-2" nil 7) (synced "n1-2" 7)))
                  (update "n1" into (started "n1-3" 6)))]               ; rolled back
      (is (false? (valid (check (assoc test-map :power-loss true) (history) evs) :durability))))))

(deftest skipped
  (let [h (conj (history) {:type :info :f :send :value "O9" :process 0 :node "n1" :gen 1 :seq 3
                           :error (str "session: message " c/skipped-error)})]
    (testing "ErrSkipped with no GapFill covering it"
      (is (false? (valid (check test-map h (clean)) :skipped))))
    (testing "covered by a GapFill written"
      (is (true? (valid (check test-map h (update (clean) "n1" conj (ev "n1-1" "out" :msgtype "4" :seq 3 :gapfill true :newseqno 4)))
                        :skipped))))))

(deftest halts
  (testing "a halt with no disk fault"
    (is (false? (valid (check (update (clean) "n1" conj (ev "n1-1" "halt" :err "store"))) :halts))))
  (testing "a send begun after a halt succeeding"
    (is (false? (valid (check (assoc test-map :disk-faults true) (history)
                              (update (clean) "n1" into [(ev "n1-1" "halt" :err "store")
                                                         (ev "n1-1" "send" :phase "begin" :clordid "O8")
                                                         (ev "n1-1" "send" :phase "end" :clordid "O8" :type "ok")]))
                       :halts))))
  (testing "a send begun before a halt succeeding after it: valid"
    (is (true? (valid (check (assoc test-map :disk-faults true) (history)
                             (update (clean) "n1" into [(ev "n1-1" "send" :phase "begin" :clordid "O8")
                                                        (ev "n1-1" "halt" :err "store")
                                                        (ev "n1-1" "send" :phase "end" :clordid "O8" :type "ok")]))
                      :halts))))
  (testing "a store append after a halt"
    (is (false? (valid (check (assoc test-map :disk-faults true) (history)
                              (update (clean) "n1" into (cons (ev "n1-1" "halt" :err "store") (appended "n1-1" "O7" 9))))
                       :halts))))
  (testing "a Logon written after a halt"
    (is (false? (valid (check (assoc test-map :disk-faults true) (history)
                              (update (clean) "n1" into [(ev "n1-1" "halt" :err "store") (ev "n1-1" "out" :msgtype "A" :seq 9)]))
                       :halts))))
  (testing "a store write hung past 1.5 StoreTimeout without a halt"
    (let [hung (fn [halt-after-ms]
                 (let [b (ev "n1-1" "store" :call "append" :phase "begin" :seq 9 :clordid "O9" :digest "d-O9")
                       at (fn [ms] (str (.plusMillis (java.time.Instant/parse (:t b)) ms)))]
                   (update (clean) "n1" into
                           (concat [b]
                                   (when halt-after-ms [(assoc (ev "n1-1" "halt" :err "watchdog" :watchdog true) :t (at halt-after-ms))])
                                   [(assoc (ev "n1-1" "store" :call "append" :phase "end" :seq 9 :err "timeout") :t (at 10000))]))))
          t (assoc test-map :disk-faults true :store-timeout 2000)]
      (is (false? (valid (check t (history) (hung nil)) :halts)))
      (is (false? (valid (check t (history) (hung 6000)) :halts)))
      (is (true? (valid (check t (history) (hung 3100)) :halts)))))
  (testing "a halt, nothing after it, with disk faults: valid"
    (is (true? (valid (check (assoc test-map :disk-faults true) (history)
                             (update (clean) "n1" conj (ev "n1-1" "halt" :err "store")))
                      :halts)))))

(deftest phantoms
  (testing "a ClOrdID never sent"
    (is (false? (valid (check (update (clean) "n2" conj (delivered "n2-1" "NEVER" 6))) :phantoms))))
  (testing "a marker no node issued"
    (is (false? (valid (check (update (clean) "n2" conj (delivered "n2-1" "BARRIER-invented" 6))) :phantoms))))
  (testing "a node delivering its own order"
    (is (false? (valid (check (update (clean) "n1" conj (delivered "n1-1" "O1" 3))) :phantoms)))))


(deftest live
  (is (false? (valid (check test-map (vec (butlast (history))) (clean)) :live))))
