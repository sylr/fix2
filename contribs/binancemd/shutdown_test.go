package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sylr/fix2/session/transport/mem"
)

// blockedWriter blocks every Write until release is closed.
type blockedWriter struct {
	first   chan struct{} // closed once a Write starts
	once    sync.Once
	release chan struct{}
	writing atomic.Int32
	overlap atomic.Bool // two Writes ran at once

	mu  sync.Mutex
	buf bytes.Buffer
}

func newBlockedWriter(t *testing.T) *blockedWriter {
	w := &blockedWriter{first: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(w.unblock)
	return w
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	if w.writing.Add(1) > 1 {
		w.overlap.Store(true)
	}
	defer w.writing.Add(-1)
	w.once.Do(func() { close(w.first) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// unblock lets every Write through.
func (w *blockedWriter) unblock() {
	select {
	case <-w.release:
	default:
		close(w.release)
	}
}

func (w *blockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// startWriting runs the ticker command for BTCUSDT against l, writing JSON
// lines to w, and returns the gateway logged on and the MDReqID.
func startWriting(t *testing.T, l *mem.Listener, w io.Writer) (*fakeBinance, string, output, context.CancelFunc, chan error) {
	t.Helper()
	keyFile, key := writeKey(t)
	o := &options{apiKey: "APIKEY", keyFile: keyFile, sender: "TESTER", endpoint: "mem", heartbeat: 30 * time.Second, levels: 10, show: 1, json: true}
	log := slog.New(slog.DiscardHandler)
	if os.Getenv("FIX2_TEST_LOG") != "" {
		log = newLogger(os.Stderr, true)
	}
	out := newOutput(w, true, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- streamWith(ctx, o, commands["ticker"], []string{"BTCUSDT"}, key, out, log, l) }()
	t.Cleanup(cancel)
	f := acceptBinance(t, l, pub(t))
	f.logon()
	id := string(f.expect("V").Get(262))
	return f, id, out, cancel, done
}

// snapshot sends a snapshot of BTCUSDT whose best bid is px.
func (f *fakeBinance) snapshot(id string, px int) {
	f.send("W", "262", id, "55", "BTCUSDT", "25044", strconv.Itoa(px), "268", "1", "269", "0", "270", strconv.Itoa(px), "271", "1.0")
}

// setOutputTimeout sets outputTimeout to d for the test.
func setOutputTimeout(t *testing.T, d time.Duration) {
	saved := outputTimeout
	outputTimeout = d
	t.Cleanup(func() { outputTimeout = saved })
}

// waitQueueFull waits for the output's queue to fill: FromApp then blocks.
func waitQueueFull(t *testing.T, out output) {
	t.Helper()
	p := out.(*jsonOutput).pipe
	deadline := time.Now().Add(5 * time.Second)
	for len(p.chunks) < cap(p.chunks) {
		if time.Now().After(deadline) {
			t.Fatalf("output queue holds %d, want %d", len(p.chunks), cap(p.chunks))
		}
		time.Sleep(time.Millisecond)
	}
}

// checkTickers checks that out holds the tickers of best bids 1000 to
// 1000+n-1, in order, each once, each a whole JSON line.
func checkTickers(t *testing.T, out string, n int) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("%d lines written, want %d", len(lines), n)
	}
	for i, line := range lines {
		var e struct{ Bid [2]string }
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d: %v: %q", i, err, line)
		}
		if want := strconv.Itoa(1000 + i); e.Bid[0] != want {
			t.Fatalf("line %d: %s, want the bid at %s", i, line, want)
		}
	}
}

// waitTickers waits for w to hold n lines, then checks them.
func waitTickers(t *testing.T, w *blockedWriter, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(w.String(), "\n") < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	checkTickers(t, w.String(), n)
}

// The command stops within its bound however the gateway answers its
// Logout, though the output stays blocked: it fails with errOutputBlocked,
// and the output, once unblocked, holds every event once, never written
// by two goroutines at once.
func TestShutdownWhileOutputBlocked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events int
		peer   func(f *fakeBinance)
	}{
		{"logout answered", 1, func(f *fakeBinance) { f.send("5") }},
		{"gateway hangs up", 1, func(f *fakeBinance) { f.conn.Close() }},
		{"gateway silent", 1, func(*fakeBinance) {}},
		// FromApp blocks on the full queue: the session's Stop gives up on
		// it, the output is never ended.
		{"queue full, logout answered", outputQueue + 10, func(f *fakeBinance) { f.send("5") }},
		{"queue full, gateway hangs up", outputQueue + 10, func(f *fakeBinance) { f.conn.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setOutputTimeout(t, 500*time.Millisecond)
			w := newBlockedWriter(t)
			f, id, out, cancel, done := startWriting(t, mem.Listen("binance"), w)
			for i := range tc.events {
				f.snapshot(id, 1000+i)
			}
			select {
			case <-w.first:
			case <-time.After(5 * time.Second):
				t.Fatal("nothing written")
			}
			if tc.events > outputQueue {
				waitQueueFull(t, out)
			}
			start := time.Now()
			cancel()
			f.expect("5")
			tc.peer(f)
			// The session's Stop, bounded by LogoutTimeout plus a second,
			// then the output's.
			select {
			case err := <-done:
				if !errors.Is(err, errOutputBlocked) {
					t.Errorf("command failed with %v, want errOutputBlocked", err)
				}
				t.Logf("stopped in %v", time.Since(start))
			case <-time.After(3*time.Second + outputTimeout + 2*time.Second):
				t.Fatal("the command did not stop")
			}
			w.unblock()
			waitTickers(t, w, tc.events)
			if w.overlap.Load() {
				t.Error("two Writes ran at once")
			}
		})
	}
}

// Administrative messages are answered while FromApp waits for a blocked
// output: FromAdmin never waits on it.
func TestAdminWhileOutputBlocked(t *testing.T) {
	w := newBlockedWriter(t)
	f, id, out, cancel, done := startWriting(t, mem.Listen("binance"), w)
	for i := range outputQueue + 10 {
		f.snapshot(id, 1000+i)
	}
	waitQueueFull(t, out)
	for _, testReqID := range []string{"T1", "T2"} {
		f.send("1", "112", testReqID)
		if got := string(f.expect("0").Get(112)); got != testReqID {
			t.Fatalf("Heartbeat TestReqID %q, want %q", got, testReqID)
		}
	}
	w.unblock()
	waitTickers(t, w, outputQueue+10)
	cancel()
	f.expect("5")
	f.send("5")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not stop")
	}
}

// A consumer that stalls during the shutdown, longer than the session's
// Stop waits, then reads again before outputTimeout, gets every event once,
// in order.
func TestShutdownStalledPipe(t *testing.T) {
	setOutputTimeout(t, 10*time.Second)
	r, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer pw.Close()
	f, id, _, cancel, done := startWriting(t, mem.Listen("binance"), pw)
	const n = 1000 // over 100 bytes each: more than a pipe holds
	for i := range n {
		f.snapshot(id, 1000+i)
	}
	// Received once the TestRequest after them is answered.
	f.send("1", "112", "SYNC")
	f.expect("0")
	cancel()
	f.expect("5")
	f.conn.Close()
	select {
	case err := <-done:
		t.Fatalf("the command stopped (%v) though its output is blocked", err)
	case <-time.After(4 * time.Second): // LogoutTimeout plus a second, and more
	}
	var got bytes.Buffer
	read := make(chan error, 1)
	go func() {
		_, err := got.ReadFrom(r)
		read <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(outputTimeout + 5*time.Second):
		t.Fatal("the command did not stop")
	}
	pw.Close()
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	checkTickers(t, got.String(), n)
}

// failingWriter accepts its first ok Writes; the next ones wait until fail,
// then fail with io.ErrClosedPipe.
type failingWriter struct {
	ok      int
	waiting chan struct{} // closed once a Write waits
	once    sync.Once
	release chan struct{}

	mu     sync.Mutex
	writes int
	buf    bytes.Buffer
}

func newFailingWriter(t *testing.T, ok int) *failingWriter {
	w := &failingWriter{ok: ok, waiting: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(w.fail)
	return w
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	if w.writes <= w.ok {
		defer w.mu.Unlock()
		return w.buf.Write(p)
	}
	w.mu.Unlock()
	w.once.Do(func() { close(w.waiting) })
	<-w.release
	return 0, io.ErrClosedPipe
}

// fail fails the Writes waiting, and those to come.
func (w *failingWriter) fail() {
	select {
	case <-w.release:
	default:
		close(w.release)
	}
}

func (w *failingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// A failed write stops the command promptly, though no event follows to be
// handed to the output: the command fails with the write's error, and the
// events written before it are the first ones, in order, each once.
func TestOutputFailureStops(t *testing.T) {
	for _, tc := range []struct {
		name    string
		written int // events written before the failure
		queued  int // events waiting for the output when it fails
	}{
		{"idle, first event", 0, 0},
		{"busy, mid-stream", 2, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFailingWriter(t, tc.written)
			f, id, out, _, done := startWriting(t, mem.Listen("binance"), w)
			// One Write each: the output's queue is empty.
			for i := range tc.written {
				f.snapshot(id, 1000+i)
				deadline := time.Now().Add(5 * time.Second)
				for strings.Count(w.String(), "\n") <= i {
					if time.Now().After(deadline) {
						t.Fatalf("event %d not written", i)
					}
					time.Sleep(time.Millisecond)
				}
			}
			f.snapshot(id, 1000+tc.written)
			select {
			case <-w.waiting:
			case <-time.After(5 * time.Second):
				t.Fatal("nothing written")
			}
			// Handed to the output, which waits on the Write: FromApp is done
			// with them once its queue holds them all.
			for i := range tc.queued {
				f.snapshot(id, 1001+tc.written+i)
			}
			p := out.(*jsonOutput).pipe
			deadline := time.Now().Add(5 * time.Second)
			for len(p.chunks) < tc.queued {
				if time.Now().After(deadline) {
					t.Fatalf("output queue holds %d, want %d", len(p.chunks), tc.queued)
				}
				time.Sleep(time.Millisecond)
			}
			w.fail()
			select {
			case m, ok := <-f.msgs:
				if !ok || string(m.MsgType()) != "5" {
					t.Fatalf("got %v, want a Logout", m)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the output failed, but the command did not stop")
			}
			f.send("5")
			select {
			case err := <-done:
				if !errors.Is(err, io.ErrClosedPipe) {
					t.Errorf("command failed with %v, want io.ErrClosedPipe", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the command did not stop")
			}
			if tc.written == 0 {
				if s := w.String(); s != "" {
					t.Errorf("written %q, want nothing", s)
				}
				return
			}
			checkTickers(t, w.String(), tc.written)
		})
	}
}

// A second signal terminates the process while it shuts down, as signals
// do by default.
func TestSecondSignalTerminates(t *testing.T) {
	if os.Getenv("BINANCEMD_SIGNAL_CHILD") != "" {
		ctx, stop := signalContext()
		defer stop()
		os.Stdout.WriteString("ready\n")
		<-ctx.Done()
		os.Stdout.WriteString("stopping\n")
		time.Sleep(time.Minute) // a shutdown that hangs
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("signals cannot be sent on Windows")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSecondSignalTerminates$")
	cmd.Env = append(os.Environ(), "BINANCEMD_SIGNAL_CHILD=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	expect := func(want string) {
		t.Helper()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("the process ended before writing %q", want)
				}
				if line == want {
					return
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("the process did not write %q", want)
			}
		}
	}
	expect("ready")
	cmd.Process.Signal(os.Interrupt)
	expect("stopping")
	exited := make(chan error, 1)
	go func() {
		for range lines {
		}
		exited <- cmd.Wait()
	}()
	// The signals are unregistered just after the context is done: one sent
	// in between is swallowed, so they are sent until one terminates.
	deadline := time.After(10 * time.Second)
wait:
	for {
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
			break wait
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("a second signal did not terminate the process")
		}
	}
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Errorf("the process ended with %v, want terminated by SIGINT", cmd.ProcessState)
	}
}
