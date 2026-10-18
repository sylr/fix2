package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Handler serves the node's control API to the Jepsen client:
//
//	POST /send     {"id": ClOrdID, "timeout_ms": n}  → SendResult
//	GET  /status                                     → Status
//	POST /drain    {"timeout_ms": n}                 → {"drained": bool}
//	POST /barrier  {"timeout_ms": n}                 → {"id": marker, ...SendResult}
//	POST /clock    {"offset_ms": n}                  → {}
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID        string `json:"id"`
			TimeoutMS int64  `json:"timeout_ms"`
		}
		if !decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		reply(w, n.Send(req.ID, timeout(req.TimeoutMS)))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, n.Snapshot())
	})
	mux.HandleFunc("POST /drain", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TimeoutMS int64 `json:"timeout_ms"`
		}
		if !decode(w, r, &req) {
			return
		}
		reply(w, map[string]bool{"drained": n.Drain(timeout(req.TimeoutMS))})
	})
	mux.HandleFunc("POST /barrier", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TimeoutMS int64 `json:"timeout_ms"`
		}
		if !decode(w, r, &req) {
			return
		}
		id, res := n.Barrier(timeout(req.TimeoutMS))
		reply(w, struct {
			ID string `json:"id"`
			SendResult
		}{id, res})
	})
	mux.HandleFunc("POST /clock", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			OffsetMS int64 `json:"offset_ms"`
		}
		if !decode(w, r, &req) {
			return
		}
		n.SetClock(time.Duration(req.OffsetMS) * time.Millisecond)
		reply(w, struct{}{})
	})
	return mux
}

// Handler serves each session's control API, the k-th's with ?session=k, the
// first's without. A clock is set for all of them: they share the process's.
func (h *Host) Handler() http.Handler {
	handlers := make([]http.Handler, len(h.Nodes))
	for k, n := range h.Nodes {
		handlers[k] = n.Handler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/clock" {
			var req struct {
				OffsetMS int64 `json:"offset_ms"`
			}
			if !decode(w, r, &req) {
				return
			}
			for _, n := range h.Nodes {
				n.SetClock(time.Duration(req.OffsetMS) * time.Millisecond)
			}
			reply(w, struct{}{})
			return
		}
		k := 0
		if s := r.URL.Query().Get("session"); s != "" {
			var err error
			if k, err = strconv.Atoi(s); err != nil || k < 0 || k >= len(handlers) {
				http.Error(w, "no session "+s, http.StatusNotFound)
				return
			}
		}
		handlers[k].ServeHTTP(w, r)
	})
}

// timeout is ms milliseconds, 5s if not positive.
func timeout(ms int64) time.Duration {
	if ms <= 0 {
		return 5 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
