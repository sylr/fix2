package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport"
	"github.com/sylr/fix2/session/transport/mem"
)

const (
	testKey        = "KEY"
	testSecret     = "c2VjcmV0LWtleS0xMjM0NTY=" // "secret-key-123456"
	testPassphrase = "pass"
)

// TestSignature checks the signature against one computed independently,
// with Python's hmac.
func TestSignature(t *testing.T) {
	secret, _ := base64.StdEncoding.DecodeString(testSecret)
	got := signature(secret, testPassphrase, session.LogonHeader{
		BeginString: "FIXT.1.1", SenderCompID: testKey, TargetCompID: "Coinbase",
		MsgSeqNum: 1, SendingTime: "20260612-10:00:00.000",
	})
	const want = "wfEn2XLQeZSyLTlB7WrMBXDvV2zQhvpDChBr441kcz4="
	if string(got) != want {
		t.Errorf("signature = %s, want %s", got, want)
	}
}

func TestRun_Usage(t *testing.T) {
	for _, args := range [][]string{
		nil,                   // no command
		{"nope"},              // unknown command
		{"trades"},            // no symbol
		{"trades", "BTC-USD"}, // no credentials
		{"--heartbeat", "500ms", "trades", "BTC-USD", "--api-key", "k", "--api-secret", "cw==", "--passphrase", "p"},
		{"book", "--show", "11", "BTC-USD", "--api-key", "k", "--api-secret", "cw==", "--passphrase", "p"},
		{"ticker", "--show", "3", "BTC-USD"}, // a book flag
		{"trades", "BTC/USD", "--api-key", "k", "--api-secret", "cw==", "--passphrase", "p"},
		{"trades", "BTC-USD", "--api-key", "k", "--api-key-file", "f", "--api-secret", "cw==", "--passphrase", "p"},
	} {
		var stderr bytes.Buffer
		t.Setenv("COINBASE_API_KEY", "")
		t.Setenv("COINBASE_API_SECRET", "")
		t.Setenv("COINBASE_API_PASSPHRASE", "")
		if code := run(args, io.Discard, &stderr); code != 2 {
			t.Errorf("run(%q) = %d, want 2; stderr: %s", args, code, stderr.String())
		}
	}
}

// Unreadable credentials and a secret that is not base64 fail the command.
func TestRun_Failure(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"trades", "BTC-USD", "--api-key-file", filepath.Join(dir, "missing"), "--api-secret", "cw==", "--passphrase", "p"},
		{"trades", "BTC-USD", "--api-key", "k", "--api-secret-file", empty, "--passphrase", "p"},
		{"trades", "BTC-USD", "--api-key", "k", "--api-secret", "not base64!", "--passphrase", "p"},
	} {
		var stderr bytes.Buffer
		if code := run(args, io.Discard, &stderr); code != 1 {
			t.Errorf("run(%q) = %d, want 1; stderr: %s", args, code, stderr.String())
		}
	}
}

// fakeCoinbase is a scripted Coinbase FIX market data gateway.
type fakeCoinbase struct {
	t    *testing.T
	conn transport.Conn
	msgs chan *fix2.Message
	seq  int
}

func acceptCoinbase(t *testing.T, l *mem.Listener) *fakeCoinbase {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := l.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCoinbase{t: t, conn: conn, msgs: make(chan *fix2.Message, 100), seq: 1}
	go func() {
		defer close(f.msgs)
		fr := fix2.NewFramer(conn, fix2.FramerOptions{})
		for {
			frame, err := fr.Next()
			if err != nil {
				return
			}
			m, err := fix2.Parse(bytes.Clone(frame))
			if err != nil {
				t.Errorf("client sent an invalid message: %v", err)
				return
			}
			f.msgs <- &m
		}
	}()
	return f
}

// expect returns the client's next message, of msgType.
func (f *fakeCoinbase) expect(msgType string) *fix2.Message {
	f.t.Helper()
	for {
		select {
		case m, ok := <-f.msgs:
			if !ok {
				f.t.Fatalf("connection closed, expected MsgType %s", msgType)
			}
			if string(m.MsgType()) == "0" && msgType != "0" {
				continue // heartbeats
			}
			if string(m.MsgType()) != msgType {
				f.t.Fatalf("got MsgType %s, want %s", m.MsgType(), msgType)
			}
			return m
		case <-time.After(5 * time.Second):
			f.t.Fatalf("timed out waiting for MsgType %s", msgType)
		}
	}
}

// logon checks the client's Logon as Coinbase does and answers it.
func (f *fakeCoinbase) logon() {
	f.t.Helper()
	m := f.expect("A")
	for tag, want := range map[int]string{8: "FIXT.1.1", 49: testKey, 56: "Coinbase", 34: "1", 98: "0", 141: "Y",
		553: testKey, 554: testPassphrase, 1137: "9", 95: strconv.Itoa(len(m.Get(96)))} {
		if got := string(m.Get(tag)); got != want {
			f.t.Errorf("Logon tag %d = %q, want %q", tag, got, want)
		}
	}
	sendingTime := string(m.Get(52))
	if len(sendingTime) != len("20060102-15:04:05.000") {
		f.t.Errorf("SendingTime %s: Coinbase signs exactly 3 fractional digits", sendingTime)
	}
	secret, _ := base64.StdEncoding.DecodeString(testSecret)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strings.Join([]string{sendingTime, "A", string(m.Get(34)), string(m.Get(49)), string(m.Get(56)), testPassphrase}, "\x01")))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); string(m.Get(96)) != want {
		f.t.Fatalf("Logon signature %s, want %s", m.Get(96), want)
	}
	f.send("A", "98", "0", "108", string(m.Get(108)), "1137", "9")
}

// send sends a message of msgType with fields after the header.
func (f *fakeCoinbase) send(msgType string, fields ...string) {
	f.t.Helper()
	b := fix2.NewBuilder("FIXT.1.1", msgType).
		SetString(49, "Coinbase").SetString(56, testKey).SetInt(34, f.seq).
		SetString(52, string(fix2.AppendUTCTimestamp(nil, time.Now(), fix2.Milliseconds)))
	f.seq++
	for i := 0; i+1 < len(fields); i += 2 {
		tag, _ := strconv.Atoi(fields[i])
		b.Append(tag, []byte(fields[i+1]))
	}
	if _, err := f.conn.Write(b.Build()); err != nil {
		f.t.Fatal(err)
	}
}

// startClient runs cmd against l, returning its output and its result.
func startClient(t *testing.T, l *mem.Listener, change func(o *options), cmd string, symbols ...string) (*safeBuffer, context.CancelFunc, chan error) {
	t.Helper()
	o := &options{
		apiKey:     secret{name: "api-key", value: testKey},
		apiSecret:  secret{name: "api-secret", value: testSecret},
		passphrase: secret{name: "passphrase", value: testPassphrase},
		endpoint:   "mem", heartbeat: 30 * time.Second, show: 3, json: true,
	}
	if change != nil {
		change(o)
	}
	out := &safeBuffer{}
	log := slog.New(slog.DiscardHandler)
	if os.Getenv("FIX2_TEST_LOG") != "" {
		log = newLogger(os.Stderr, true)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- streamWith(ctx, o, commands[cmd], symbols, newOutput(out, o.json, false), log, l) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("client did not stop")
		}
	})
	return out, cancel, done
}

// safeBuffer is a bytes.Buffer safe for concurrent use.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitLines waits for n lines of output and returns them.
func waitLines(t *testing.T, out *safeBuffer, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		if len(lines) >= n && lines[0] != "" {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d lines, want %d:\n%s", len(lines), n, out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitDone waits for the command to end and returns its error.
func waitDone(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		done <- nil // for the cleanup
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not end")
	}
	return nil
}

func TestTrades(t *testing.T) {
	l := mem.Listen("coinbase")
	out, cancel, done := startClient(t, l, nil, "trades", "BTC-USD")
	f := acceptCoinbase(t, l)
	f.logon()
	v := f.expect("V")
	for tag, want := range map[int]string{263: "1", 264: "1", 146: "1", 55: "BTC-USD"} {
		if got := string(v.Get(tag)); got != want {
			t.Errorf("MarketDataRequest tag %d = %q, want %q", tag, got, want)
		}
	}
	id := string(v.Get(262))
	// A quote change, ignored, then two trades.
	f.send("X", "262", id, "268", "1", "279", "1", "269", "0", "55", "BTC-USD", "270", "62000.00", "271", "1.5")
	f.send("X", "262", id, "268", "1", "279", "0", "269", "2", "55", "BTC-USD", "270", "62000.01", "271", "0.01000000",
		"60", "20260612-10:00:00.123456", "37", "ord-1", "5797", "1")
	f.send("X", "262", id, "268", "1", "279", "0", "269", "2", "55", "BTC-USD", "270", "61999.99", "271", "0.2", "1003", "T-2", "5797", "2")
	lines := waitLines(t, out, 2)
	var tr tradeEvent
	if err := json.Unmarshal([]byte(lines[0]), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Symbol != "BTC-USD" || tr.Price != "62000.01" || tr.Qty != "0.01" || tr.ID != "ord-1" || tr.Aggressor != "buy" ||
		!tr.Time.Equal(time.Date(2026, 6, 12, 10, 0, 0, 123456000, time.UTC)) {
		t.Errorf("trade %+v", tr)
	}
	if !strings.Contains(lines[1], `"trade_id":"T-2"`) || !strings.Contains(lines[1], `"aggressor":"sell"`) {
		t.Errorf("trade %s", lines[1])
	}
	cancel()
	f.expect("5")
	f.send("5")
	if err := waitDone(t, done); err != nil {
		t.Errorf("stream: %v", err)
	}
}

func TestTicker(t *testing.T) {
	l := mem.Listen("coinbase")
	out, _, _ := startClient(t, l, func(o *options) { o.json = false }, "ticker", "ETH-USD")
	f := acceptCoinbase(t, l)
	f.logon()
	id := string(f.expect("V").Get(262))
	f.send("W", "262", id, "893", "Y", "55", "ETH-USD", "268", "2",
		"269", "0", "270", "2500.10", "271", "3", "269", "1", "270", "2500.20", "271", "4")
	f.send("X", "262", id, "268", "1", "279", "1", "269", "0", "55", "ETH-USD", "270", "2500.15", "271", "1")
	f.send("X", "262", id, "268", "1", "279", "2", "269", "1", "55", "ETH-USD", "270", "2500.20")
	lines := waitLines(t, out, 3)
	for i, want := range []string{
		"ETH-USD bid 3 @ 2500.1  ask 4 @ 2500.2",
		"ETH-USD bid 1 @ 2500.15  ask 4 @ 2500.2",
		"ETH-USD bid 1 @ 2500.15  ask -",
	} {
		if !strings.HasSuffix(lines[i], want) {
			t.Errorf("line %d: got %q, want ...%q", i, lines[i], want)
		}
	}
}

// A snapshot in fragments makes one book.
func TestBook(t *testing.T) {
	l := mem.Listen("coinbase")
	out, _, _ := startClient(t, l, nil, "book", "BTC-USD")
	f := acceptCoinbase(t, l)
	f.logon()
	v := f.expect("V")
	if got := string(v.Get(264)); got != "10" {
		t.Errorf("MarketDepth = %s, want 10", got)
	}
	id := string(v.Get(262))
	f.send("W", "262", id, "893", "N", "55", "BTC-USD", "268", "2",
		"269", "0", "270", "99.00", "271", "1", "269", "1", "270", "101.00", "271", "4")
	f.send("W", "262", id, "893", "Y", "55", "BTC-USD", "268", "2",
		"269", "0", "270", "98.50", "271", "2", "269", "1", "270", "100.00", "271", "3")
	f.send("X", "262", id, "268", "1", "279", "0", "269", "0", "55", "BTC-USD", "270", "99.50", "271", "0.5")
	f.send("X", "262", id, "268", "1", "279", "2", "269", "1", "55", "BTC-USD", "270", "100.00")
	lines := waitLines(t, out, 3)
	want := []string{
		`"bids":[["99","1"],["98.5","2"]],"asks":[["100","3"],["101","4"]]}`,
		`"bids":[["99.5","0.5"],["99","1"],["98.5","2"]],"asks":[["100","3"],["101","4"]]}`,
		`"bids":[["99.5","0.5"],["99","1"],["98.5","2"]],"asks":[["101","4"]]}`,
	}
	for i := range want {
		if !strings.HasSuffix(lines[i], want[i]) {
			t.Errorf("line %d:\n got %s\nwant ...%s", i, lines[i], want[i])
		}
	}
}

func TestDepth(t *testing.T) {
	l := mem.Listen("coinbase")
	out, _, _ := startClient(t, l, func(o *options) { o.json = false }, "depth", "BTC-USD")
	f := acceptCoinbase(t, l)
	f.logon()
	id := string(f.expect("V").Get(262))
	f.send("W", "262", id, "893", "Y", "55", "BTC-USD", "268", "1", "269", "0", "270", "99.00", "271", "1")
	f.send("X", "262", id, "268", "1", "279", "2", "269", "0", "55", "BTC-USD", "270", "99.00")
	lines := waitLines(t, out, 2)
	if !strings.Contains(lines[0], "BTC-USD snapshot: 1 bids, 0 asks; best bid 1 @ 99") || !strings.HasSuffix(lines[1], "BTC-USD bid delete 99") {
		t.Errorf("output:\n%s", out.String())
	}
}

// The list comes in fragments; symbols asked for are kept.
func TestInstruments(t *testing.T) {
	l := mem.Listen("coinbase")
	out, _, done := startClient(t, l, nil, "instruments", "ETH-USD")
	f := acceptCoinbase(t, l)
	f.logon()
	x := f.expect("x")
	if string(x.Get(559)) != "4" {
		t.Errorf("SecurityListRequestType %s, want 4 (all)", x.Get(559))
	}
	id := string(x.Get(320))
	f.send("y", "320", id, "322", "R1", "560", "0", "393", "2", "893", "N", "146", "1",
		"55", "BTC-USD", "15", "USD", "562", "1", "969", "0.01", "29003", "0.00000001", "1682", "full_trading")
	f.send("y", "320", id, "322", "R1", "560", "0", "393", "2", "893", "Y", "146", "1",
		"55", "ETH-USD", "15", "USD", "562", "1.00", "969", "0.01000", "29003", "0.00000001", "1682", "full_trading")
	f.expect("5")
	f.send("5")
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	lines := waitLines(t, out, 1)
	if len(lines) != 1 {
		t.Fatalf("got %d instruments, want 1:\n%s", len(lines), out.String())
	}
	var e instrumentEvent
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Symbol != "ETH-USD" || e.Currency != "USD" || e.MinNotional != "1" || e.TickSize != "0.01" || e.SizeStep != "0.00000001" || e.Status != "full_trading" {
		t.Errorf("instrument %+v", e)
	}
}

func TestInstruments_NotListed(t *testing.T) {
	l := mem.Listen("coinbase")
	_, _, done := startClient(t, l, nil, "instruments", "NOPE-USD")
	f := acceptCoinbase(t, l)
	f.logon()
	id := string(f.expect("x").Get(320))
	f.send("y", "320", id, "322", "R1", "560", "0", "393", "1", "893", "Y", "146", "1", "55", "BTC-USD", "15", "USD")
	f.expect("5")
	f.send("5")
	if err := waitDone(t, done); err == nil || !strings.Contains(err.Error(), "not listed") {
		t.Errorf("stream = %v", err)
	}
}

func TestLogonRejected(t *testing.T) {
	l := mem.Listen("coinbase")
	_, _, done := startClient(t, l, nil, "trades", "BTC-USD")
	f := acceptCoinbase(t, l)
	f.expect("A")
	f.send("3", "45", "1", "372", "A", "373", "8", "58", "invalid signature")
	if err := waitDone(t, done); err == nil || !strings.Contains(err.Error(), "invalid signature") {
		t.Errorf("stream = %v", err)
	}
}

func TestSubscriptionRefused(t *testing.T) {
	l := mem.Listen("coinbase")
	_, _, done := startClient(t, l, nil, "ticker", "NOPE-USD")
	f := acceptCoinbase(t, l)
	f.logon()
	id := string(f.expect("V").Get(262))
	f.send("Y", "262", id, "281", "0", "58", "Unknown symbol")
	f.expect("5")
	f.send("5")
	if err := waitDone(t, done); err == nil || !strings.Contains(err.Error(), "unknown symbol") || !strings.Contains(err.Error(), "NOPE-USD") {
		t.Errorf("stream = %v", err)
	}
}

// With --validate, messages are checked against Coinbase's dictionaries.
func TestValidate(t *testing.T) {
	l := mem.Listen("coinbase")
	out, _, _ := startClient(t, l, func(o *options) { o.validate = true }, "depth", "BTC-USD")
	f := acceptCoinbase(t, l)
	f.logon()
	id := string(f.expect("V").Get(262))
	// MDUpdateAction 9 is no action: rejected, not shown.
	f.send("X", "262", id, "268", "1", "279", "9", "269", "0", "55", "BTC-USD", "270", "99.00")
	if r := f.expect("3"); string(r.Get(371)) != "279" {
		t.Errorf("Reject RefTagID %s, want 279", r.Get(371))
	}
	f.send("X", "262", id, "268", "1", "279", "0", "269", "0", "55", "BTC-USD", "270", "99.00", "271", "1")
	lines := waitLines(t, out, 1)
	if len(lines) != 1 || !strings.Contains(lines[0], `"action":"new"`) {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestTLSConfig(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "roots.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tlsConfig(notPEM, log); err == nil {
		t.Error("a file without certificates was accepted")
	}
	if _, err := tlsConfig(filepath.Join(dir, "missing.pem"), log); err == nil {
		t.Error("a missing file was accepted")
	}
	cfg, err := tlsConfig("", log)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion %x", cfg.MinVersion)
	}
	if _, statErr := os.Stat(macOSRoots); runtime.GOOS == "darwin" && statErr == nil && cfg.RootCAs == nil {
		t.Error("on macOS, Go's verifier is not used by default")
	}
}
