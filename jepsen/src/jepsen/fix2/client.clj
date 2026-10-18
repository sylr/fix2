(ns jepsen.fix2.client
  "Sends orders through a node's control API, and settles the nodes at the
  end: drained, then a marker each way, in each session."
  (:require [clojure.data.json :as json]
            [clojure.tools.logging :refer [warn]]
            [jepsen [client :as client]
                    [control :as c]]
            [jepsen.fix2.db :as db]
            [org.httpkit.client :as http]))

(def send-timeout-ms 5000)

(defn sessions "The sessions' numbers." [test] (range (:fix-sessions test 1)))

(defn marker-key
  "Where the quiesce records the marker node sent in session k: node for
  the first, node/s<k> for the others."
  [node k]
  (if (zero? k) node (str node "/s" k)))

(defn- url [node path k]
  (str "http://" node ":" db/http-port path (when (pos? k) (str "?session=" k))))

(defn- request
  "Makes an HTTP request to node's session k and returns its decoded JSON
  body, or {:http-error kind} where kind is :refused (the node never got
  it) or :unknown."
  [method node k path body timeout-ms]
  (let [{:keys [status body error]}
        @(http/request {:method  method
                        :url     (url node path k)
                        :headers {"Content-Type" "application/json"}
                        :body    (when body (json/write-str body))
                        :timeout timeout-ms})]
    (cond
      (instance? java.net.ConnectException error) {:http-error :refused}
      error {:http-error :unknown :message (str error)}
      (not= 200 status) {:http-error :unknown :message (str "HTTP " status ": " body)}
      :else (json/read-str body :key-fn keyword))))

(defn status
  ([node] (status node 0))
  ([node k] (request :get node k "/status" nil 2000)))

(defn halted?
  "Whether a session of node halted."
  [test node]
  (boolean (some #(seq (:err (status node %))) (sessions test))))

(defn set-clock!
  "Sets node's session clock offset, in ms: every session's, the process's."
  [node offset-ms]
  (let [r (request :post node 0 "/clock" {:offset_ms offset-ms} 2000)]
    (if (:http-error r) (:http-error r) offset-ms)))

(defn- send!
  "Sends an order, and turns the node's answer into the op's completion."
  [op node]
  (let [res (request :post node (:session op 0) "/send" {:id (:value op) :timeout_ms send-timeout-ms}
                     (+ send-timeout-ms 2000))]
    (case (:http-error res)
      :refused (assoc op :type :fail :error :connection-refused)
      :unknown (assoc op :type :info :error (:message res))
      (assoc op
             :type (keyword (:type res))
             :gen (:gen res)
             :seq (:seq res)
             :level (:level res)
             :error (:error res)))))

(defn- await-markers
  "Polls the nodes until each delivered, in each session, the marker the
  other sent in it, or the deadline passes. markers: [node k] → id."
  [markers deadline]
  (loop []
    (let [delivered (into {} (for [[nk _] markers]
                               [nk (set (:markers (apply status nk)))]))
          done? (every? (fn [[[from k] id]]
                          (some (fn [[[node k'] ms]] (and (not= node from) (= k k') (ms id))) delivered))
                        markers)]
      (cond done? true
            (< deadline (System/currentTimeMillis)) false
            :else (do (Thread/sleep 200) (recur))))))

(defn- drained?
  "Drains node's sessions: true once the sends each admitted returned."
  [test node]
  (every? (fn [k] (true? (:drained (request :post node k "/drain" {:timeout_ms 10000} 15000))))
          (sessions test)))

(defn- await-logged-on
  "Polls the nodes until each session is logged on, or ms pass."
  [test nodes ms]
  (let [deadline (+ (System/currentTimeMillis) ms)]
    (loop []
      (when (and (not-every? (fn [n] (every? #(:logged_on (status n %)) (sessions test))) nodes)
                 (< (System/currentTimeMillis) deadline))
        (Thread/sleep 200)
        (recur)))))

(defn- barrier!
  "Sends node's marker in session k, again while it fails outright (not
  logged on yet), for up to 30s."
  [node k]
  (let [deadline (+ (System/currentTimeMillis) 30000)]
    (loop []
      (let [m (request :post node k "/barrier" {:timeout_ms 10000} 15000)]
        (if (and (= "fail" (:type m)) (< (System/currentTimeMillis) deadline))
          (do (Thread/sleep 500) (recur))
          m)))))

(defn- quiesce!
  "Drains both nodes, restarting one whose sends do not return, sends a
  marker each way in each session, and waits for each to be delivered:
  every message sent before a marker is then delivered or skipped."
  [test op]
  (let [nodes     (:nodes test)
        ;; A halted session neither sends nor connects again: restarting
        ;; its process is the documented reconciliation.
        ;; Or whose disk was faulted: a store in doubt does not always
        ;; halt its session (docs/jepsen.md, "Found while planning").
        halted    (if (:disk-faults test) nodes (filter #(halted? test %) nodes))
        _         (when (seq halted)
                    (warn "Restarting halted or disk-faulted nodes:" halted)
                    (c/on-nodes test halted (fn [test node] (db/kill! (dissoc test :power-loss) node) (db/start! test node))))
        stuck     (remove #(drained? test %) nodes)
        _         (when (seq stuck)
                    (warn "Restarting nodes whose sends did not return:" stuck)
                    (c/on-nodes test stuck (fn [test node] (db/kill! test node) (db/start! test node))))
        undrained (remove #(drained? test %) stuck)
        ;; Restarted, the sessions log on again before a marker can go.
        _         (when (or (seq halted) (seq stuck)) (await-logged-on test nodes 30000))
        markers   (when (empty? undrained)
                    (into {} (for [n nodes k (sessions test)] [[n k] (barrier! n k)])))
        ids       (into {} (for [[nk m] markers] [nk (:id m)]))
        live?     (and (empty? undrained)
                       (every? :id (vals markers))
                       (await-markers ids (+ (System/currentTimeMillis)
                                             (* 1000 (:quiesce-timeout test)))))]
    (assoc op
           :type  (if live? :ok :fail)
           :value {:halted (vec halted) :restarted (vec stuck)
                   :markers (into {} (for [[[n k] id] ids] [(marker-key n k) id]))}
           :error (cond (seq undrained) [:not-drained undrained]
                        (not live?)     :markers-not-delivered))))

(defrecord Client [node]
  client/Client
  (open! [this test node] (assoc this :node node))
  (setup! [this test])
  (invoke! [this test op]
    (case (:f op)
      :send    (send! (assoc op :node node) node)
      :quiesce (quiesce! test op)))
  (teardown! [this test])
  (close! [this test]))

(defn client [] (Client. nil))
