(defproject fix2-jepsen "0.1.0-SNAPSHOT"
  :description "Jepsen tests of fix2's FIX session layer: see docs/jepsen.md."
  :url "https://github.com/sylr/fix2"
  :license {:name "MIT License"
            :url "https://opensource.org/license/mit"}
  ; jepsen 0.3.12 to 0.3.14 have a grepkill! that kills nothing.
  :dependencies [[org.clojure/clojure "1.12.6"]
                 [jepsen "0.3.15"]
                 [org.clojure/data.json "2.5.1"]]
  :main jepsen.fix2
  :jvm-opts ["-Xmx4g" "-Djava.awt.headless=true" "-server"])
