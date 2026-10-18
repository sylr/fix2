(ns jepsen.fix2.client
  "Sends orders through a node's control API, and settles the nodes at the
  end: drained, then a marker each way."
  (:require [clojure.data.json :as json]
            [clojure.tools.logging :refer [warn]]
            [jepsen [client :as client]
                    [control :as c]]
            [jepsen.fix2.db :as db]
            [org.httpkit.client :as http]))

(def send-timeout-ms 5000)

(defn- url [node path] (str "http://" node ":" db/http-port path))

(defn- request
  "Makes an HTTP request to node and returns its decoded JSON body, or
  {:http-error kind} where kind is :refused (the node never got it) or
  :unknown."
  [method node path body timeout-ms]
  (let [{:keys [status body error]}
        @(http/request {:method  method
                        :url     (url node path)
                        :headers {"Content-Type" "application/json"}
                        :body    (when body (json/write-str body))
                        :timeout timeout-ms})]
    (cond
      (instance? java.net.ConnectException error) {:http-error :refused}
      error {:http-error :unknown :message (str error)}
      (not= 200 status) {:http-error :unknown :message (str "HTTP " status ": " body)}
      :else (json/read-str body :key-fn keyword))))

(defn status [node] (request :get node "/status" nil 2000))

(defn- send!
  "Sends an order, and turns the node's answer into the op's completion."
  [op node]
  (let [res (request :post node "/send" {:id (:value op) :timeout_ms send-timeout-ms}
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
  "Polls the nodes until each delivered the marker the other sent, or the
  deadline passes."
  [markers deadline]
  (loop []
    (let [delivered (into {} (for [[node _] markers]
                               [node (set (:markers (status node)))]))
          done? (every? (fn [[from id]]
                          (some (fn [[node ms]] (and (not= node from) (ms id))) delivered))
                        markers)]
      (cond done? true
            (< deadline (System/currentTimeMillis)) false
            :else (do (Thread/sleep 200) (recur))))))

(defn- drain!
  "Drains node: true once the sends it admitted returned."
  [node]
  (true? (:drained (request :post node "/drain" {:timeout_ms 10000} 15000))))

(defn- quiesce!
  "Drains both nodes, restarting one whose sends do not return, sends a
  marker each way, and waits for each to be delivered: every message sent
  before a marker is then delivered or skipped."
  [test op]
  (let [nodes     (:nodes test)
        stuck     (remove drain! nodes)
        _         (when (seq stuck)
                    (warn "Restarting nodes whose sends did not return:" stuck)
                    (c/on-nodes test stuck (fn [test node] (db/kill! test node) (db/start! test node))))
        undrained (remove drain! stuck)
        markers   (when (empty? undrained)
                    (into {} (for [n nodes]
                               [n (request :post n "/barrier" {:timeout_ms 10000} 15000)])))
        ids       (into {} (for [[n m] markers] [n (:id m)]))
        live?     (and (empty? undrained)
                       (every? :id (vals markers))
                       (await-markers ids (+ (System/currentTimeMillis)
                                             (* 1000 (:quiesce-timeout test)))))]
    (assoc op
           :type  (if live? :ok :fail)
           :value {:restarted (vec stuck) :markers ids}
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
