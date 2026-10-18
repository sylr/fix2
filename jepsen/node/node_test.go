package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fix/session/store"
	"github.com/sylr/fix2/fix/session/store/memstore"
)

// started is a node running until stop is called.
type started struct {
	*Node
	stop func()
}

// start runs a node with opts, filling in what a test leaves out.
func start(t *testing.T, opts Options) *started {
	t.Helper()
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:0"
	}
	if opts.HeartBtInt == 0 {
		opts.HeartBtInt = time.Second
	}
	n, err := NewNode(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); n.Run(ctx) }()
	var once sync.Once
	s := &started{Node: n, stop: func() { once.Do(func() { cancel(); <-done }) }}
	t.Cleanup(s.stop)
	return s
}

// pair starts an acceptor and an initiator on SQLite stores in dir, and
// waits for them to log on.
func pair(t *testing.T, dir string, mutants ...string) (acc, ini *started) {
	t.Helper()
	acc = start(t, Options{Role: "acceptor", Store: "sqlite:" + filepath.Join(dir, "acc.db"),
		Events: filepath.Join(dir, "acc"), Incarnation: "acc-1", Mutants: mutants})
	ini = start(t, Options{Role: "initiator", Connect: acc.Addr().String(), Store: "sqlite:" + filepath.Join(dir, "ini.db"),
		Events: filepath.Join(dir, "ini"), Incarnation: "ini-1", Mutants: mutants})
	waitFor(t, "logged on", func() bool { return acc.Snapshot().LoggedOn && ini.Snapshot().LoggedOn })
	return acc, ini
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sendAll sends ids from n, 8 at a time, and fails the test on any send
// not acknowledged.
func sendAll(t *testing.T, n *started, ids []string) {
	t.Helper()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	errs := make(chan string, len(ids))
	for _, id := range ids {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if r := n.Send(id, 5*time.Second); r.Type != "ok" {
				errs <- fmt.Sprintf("%s: %+v", id, r)
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// settle drains both nodes and waits for each to deliver the other's
// marker.
func settle(t *testing.T, a, b *started) {
	t.Helper()
	for _, n := range []*started{a, b} {
		if !n.Drain(5 * time.Second) {
			t.Fatal("not drained")
		}
	}
	ma, ra := a.Barrier(5 * time.Second)
	mb, rb := b.Barrier(5 * time.Second)
	if ra.Type == "fail" || rb.Type == "fail" {
		t.Fatalf("barrier: %+v, %+v", ra, rb)
	}
	has := func(n *started, id string) bool { return strings.Contains(strings.Join(n.Snapshot().Markers, " "), id) }
	waitFor(t, "markers delivered", func() bool { return has(b, ma) && has(a, mb) })
}

// events reads every event log in dir.
func events(t *testing.T, dir string) []Event {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "events-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var evs []Event
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var ev Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			evs = append(evs, ev)
		}
	}
	return evs
}

func kind(evs []Event, k string) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}

func ids(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

// checkDelivered checks that the deliveries in evs are every one of sent,
// once each, in rising MsgSeqNum, and nothing else but markers.
func checkDelivered(t *testing.T, evs []Event, sent []string) {
	t.Helper()
	want := map[string]bool{}
	for _, id := range sent {
		want[id] = true
	}
	seen := map[string]bool{}
	var last uint64
	for _, ev := range kind(evs, "deliver") {
		if ev.Seq <= last {
			t.Errorf("%s delivered at %d after %d", ev.ClOrdID, ev.Seq, last)
		}
		last = ev.Seq
		if seen[ev.ClOrdID] {
			t.Errorf("%s delivered twice", ev.ClOrdID)
		}
		seen[ev.ClOrdID] = true
		if !want[ev.ClOrdID] && !strings.HasPrefix(ev.ClOrdID, markerPrefix) {
			t.Errorf("%s delivered, never sent", ev.ClOrdID)
		}
	}
	for _, id := range sent {
		if !seen[id] {
			t.Errorf("%s never delivered", id)
		}
	}
}

// Orders sent both ways are acknowledged and delivered once each, in order;
// the event logs say so, and that each was synced before it was written.
func TestNode_Delivered(t *testing.T) {
	dir := t.TempDir()
	acc, ini := pair(t, dir)
	fromIni, fromAcc := ids("I", 100), ids("A", 100)
	var wg sync.WaitGroup
	wg.Go(func() { sendAll(t, ini, fromIni) })
	wg.Go(func() { sendAll(t, acc, fromAcc) })
	wg.Wait()
	settle(t, acc, ini)
	acc.stop()
	ini.stop()

	accEvs, iniEvs := events(t, filepath.Join(dir, "acc")), events(t, filepath.Join(dir, "ini"))
	checkDelivered(t, accEvs, fromIni)
	checkDelivered(t, iniEvs, fromAcc)

	// Every order the initiator wrote was durable first: appended before a
	// Sync that returned nil began.
	var synced uint64
	for _, ev := range iniEvs {
		switch {
		case ev.Kind == "store" && ev.Call == "sync" && ev.Phase == "end" && ev.Err == "":
			synced = max(synced, ev.Cutoff)
		case ev.Kind == "out" && ev.MsgType == "D" && ev.Seq > synced:
			t.Errorf("%s written at %d, synced up to %d", ev.ClOrdID, ev.Seq, synced)
		}
	}
	// The acceptor saw each order with the digest the initiator stored.
	stored := map[uint64]string{}
	for _, ev := range iniEvs {
		if ev.Kind == "store" && ev.Call == "append" && ev.Phase == "end" && ev.ClOrdID != "" {
			stored[ev.Seq] = ev.Digest
		}
	}
	for _, ev := range kind(accEvs, "deliver") {
		if d, ok := stored[ev.Seq]; !ok || d != ev.Digest {
			t.Errorf("%s delivered at %d with digest %s, stored %s", ev.ClOrdID, ev.Seq, ev.Digest, d)
		}
	}
}

// A node restarted on its store carries on where it was: sequence numbers
// go on, nothing is delivered twice in a process, and the barrier still
// closes.
func TestNode_Restart(t *testing.T) {
	dir := t.TempDir()
	acc, ini := pair(t, dir)
	sendAll(t, ini, ids("X", 20))
	ini.stop()
	ini = start(t, Options{Role: "initiator", Connect: acc.Addr().String(), Store: "sqlite:" + filepath.Join(dir, "ini.db"),
		Events: filepath.Join(dir, "ini"), Incarnation: "ini-2"})
	waitFor(t, "logged on again", func() bool { return ini.Snapshot().LoggedOn })
	sendAll(t, ini, ids("Y", 20))
	settle(t, acc, ini)
	acc.stop()
	ini.stop()

	checkDelivered(t, events(t, filepath.Join(dir, "acc")), append(ids("X", 20), ids("Y", 20)...))
	var opened []Event
	for _, ev := range events(t, filepath.Join(dir, "ini")) {
		if ev.Kind == "store" && ev.Call == "open" && ev.Phase == "end" && ev.Inc == "ini-2" {
			opened = append(opened, ev)
		}
	}
	if len(opened) != 1 || opened[0].Next < 22 {
		t.Errorf("restarted on %+v, want NextSender past the Logon and 20 orders", opened)
	}
}

// The control API answers over HTTP.
func TestNode_HTTP(t *testing.T) {
	dir := t.TempDir()
	acc, ini := pair(t, dir)
	srv := httptest.NewServer(ini.Handler())
	defer srv.Close()
	post := func(path, body string, v any) {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %s", path, resp.Status)
		}
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	}
	var r SendResult
	post("/send", `{"id":"H1","timeout_ms":5000}`, &r)
	if r.Type != "ok" || r.Level != "acknowledged" || r.Seq == 0 {
		t.Errorf("send: %+v", r)
	}
	var st Status
	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if !st.LoggedOn || st.State != "logged on" {
		t.Errorf("status: %+v", st)
	}
	var d map[string]bool
	post("/drain", `{"timeout_ms":1000}`, &d)
	if !d["drained"] {
		t.Errorf("drain: %v", d)
	}
	post("/send", `{"id":"H2"}`, &r)
	if r.Type != "fail" {
		t.Errorf("send while draining: %+v, want fail", r)
	}
	var b struct {
		ID string `json:"id"`
		SendResult
	}
	post("/barrier", `{}`, &b)
	waitFor(t, "marker delivered", func() bool { return strings.Contains(strings.Join(acc.Snapshot().Markers, " "), b.ID) })
	post("/clock", `{"offset_ms":3600000}`, &struct{}{})
	if d := ini.now().Sub(time.Now()); d < 59*time.Minute {
		t.Errorf("clock offset %v, want an hour", d)
	}
}

// The mutants break what they say: redeliver logs every tenth delivery
// twice, drop-deliver not at all.
func TestNode_Mutants(t *testing.T) {
	for _, tc := range []struct {
		mutant    string
		wantTimes int
	}{{mutantRedeliver, 2}, {mutantDropDeliver, 0}} {
		t.Run(tc.mutant, func(t *testing.T) {
			dir := t.TempDir()
			acc, ini := pair(t, dir, tc.mutant)
			sendAll(t, ini, ids("M", 20))
			settle(t, acc, ini)
			acc.stop()
			ini.stop()
			times := map[string]int{}
			for _, ev := range kind(events(t, filepath.Join(dir, "acc")), "deliver") {
				times[ev.ClOrdID]++
			}
			off := 0
			for _, id := range ids("M", 20) {
				if times[id] == tc.wantTimes {
					off++
				}
			}
			if off == 0 {
				t.Errorf("no delivery logged %d times: %v", tc.wantTimes, times)
			}
		})
	}
}

// gated is a memstore whose Appends can be held, or failed.
type gated struct {
	*memstore.Store
	hold    atomic.Bool
	release chan struct{}
	fail    atomic.Pointer[error]
}

func newGated() *gated { return &gated{Store: memstore.New(), release: make(chan struct{})} }

func (g *gated) Append(ctx context.Context, gen, seq uint64, raw []byte) error {
	if g.hold.Load() {
		<-g.release
	}
	if err := g.fail.Load(); err != nil {
		return *err
	}
	return g.Store.Append(ctx, gen, seq, raw)
}

// gatedPair is pair with the initiator on st.
func gatedPair(t *testing.T, st store.Store) (acc, ini *started) {
	t.Helper()
	dir := t.TempDir()
	acc = start(t, Options{Role: "acceptor", Store: "memstore", Events: filepath.Join(dir, "acc"), Incarnation: "acc-1"})
	ini = start(t, Options{Role: "initiator", Connect: acc.Addr().String(), Store: "gated",
		Events: filepath.Join(dir, "ini"), Incarnation: "ini-1", store: st})
	waitFor(t, "logged on", func() bool { return acc.Snapshot().LoggedOn && ini.Snapshot().LoggedOn })
	return acc, ini
}

func (n *Node) unresolvedNow() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.unresolved
}

// A send held by a store write that hangs is answered unresolved at its
// deadline, and counted so until it returns; a drain waits for it, and
// admits no new send meanwhile.
func TestNode_Unresolved(t *testing.T) {
	st := newGated()
	_, ini := gatedPair(t, st)
	st.hold.Store(true)
	var once sync.Once
	release := func() { once.Do(func() { st.hold.Store(false); close(st.release) }) }
	defer release()

	began := time.Now()
	r := ini.Send("G1", 300*time.Millisecond)
	if r.Type != "info" || r.Err != errUnresolved.Error() {
		t.Fatalf("held send: %+v, want info unresolved", r)
	}
	if d := time.Since(began); d < 300*time.Millisecond || d > 2*time.Second {
		t.Errorf("answered after %v, want its deadline", d)
	}
	if n := ini.unresolvedNow(); n != 1 {
		t.Errorf("unresolved = %d, want 1", n)
	}
	if ini.Drain(200 * time.Millisecond) {
		t.Error("drained with a send held")
	}
	if r := ini.Send("G2", time.Second); r.Type != "fail" || r.Err != errDraining.Error() {
		t.Errorf("send while draining: %+v, want fail", r)
	}
	release()
	if !ini.Drain(5 * time.Second) {
		t.Error("not drained once the store answered")
	}
	if n := ini.unresolvedNow(); n != 0 {
		t.Errorf("unresolved = %d once returned, want 0", n)
	}
}

// A send whose store write is in doubt may be stored: "info", and the
// session halts.
func TestNode_UncertainSend(t *testing.T) {
	st := newGated()
	_, ini := gatedPair(t, st)
	unknown := error(store.ErrUnknownOutcome)
	st.fail.Store(&unknown)
	if r := ini.Send("U1", time.Second); r.Type != "info" {
		t.Errorf("send on a store in doubt: %+v, want info", r)
	}
	waitFor(t, "halted", func() bool { return ini.Snapshot().Err != "" })
}

// Workload sends may not use the markers' ClOrdIDs, and unknown mutants are
// refused.
func TestNode_Refusals(t *testing.T) {
	_, ini := gatedPair(t, newGated())
	if r := ini.Send(markerPrefix+"ini-1-1", time.Second); r.Type != "fail" || r.Err != errReserved.Error() {
		t.Errorf("send of a marker's ClOrdID: %+v, want fail", r)
	}
	if _, err := NewNode(Options{Role: "initiator", Store: "memstore", Events: t.TempDir(), Mutants: []string{"no-such"}}); err == nil {
		t.Error("unknown mutant accepted")
	}
}
