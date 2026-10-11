package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

// docKey is the example private key of Binance's FIX API documentation.
const docKey = `-----BEGIN PRIVATE KEY-----
MC4CAQAwBQYDK2VwBCIEIIJEYWtGBrhACmb9Dvy+qa8WEf0lQOl1s4CLIAB9m89u
-----END PRIVATE KEY-----
`

func writeKey(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, []byte(docKey), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := loadKey(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, key
}

// TestSignature checks the signature against the documentation's example.
func TestSignature(t *testing.T) {
	_, key := writeKey(t)
	got := signature(key, session.LogonHeader{
		BeginString: "FIX.4.4", SenderCompID: "EXAMPLE", TargetCompID: "SPOT",
		MsgSeqNum: 1, SendingTime: "20240627-11:17:25.223",
	})
	const want = "4MHXelVVcpkdwuLbl6n73HQUXUf1dse2PCgT1DYqW9w8AVZ1RACFGM+5UdlGPrQHrgtS3CvsRURC1oj73j8gCA=="
	if string(got) != want {
		t.Errorf("signature = %s, want %s", got, want)
	}
}

func TestLoadKey_NotEd25519(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, []byte("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKey(path); err == nil || !strings.Contains(err.Error(), "PKCS #8") {
		t.Errorf("loadKey = %v", err)
	}
}

func TestRun_Usage(t *testing.T) {
	var stderr bytes.Buffer
	for _, args := range [][]string{
		nil,                   // no command
		{"--heartbeat", "2s"}, // no command
		{"nope"},
		{"trades"}, // no symbol
		{"--heartbeat", "2s", "trades", "BTCUSDT"},                                     // too short
		{"limits", "BTCUSDT"},                                                          // no symbol taken
		{"book", "--levels", "1", "BTCUSDT"},                                           // too few levels
		{"ticker", "--show", "3", "BTCUSDT"},                                           // a book flag
		{"--sender", "TOO-LONG-ID", "ticker", "BTCUSDT"},                               // CompID too long
		{"trades", "--api-key", "k", "BTC/USDT", "ETHBTC"},                             // invalid symbol
		{"trades", "--api-key", "k", "--api-key-file", "f", "BTCUSDT"},                 // both
		{"limits", "--api-key=", "--api-key-file=/missing", "--key-file=/missing.pem"}, // both, one empty
		{"limits", "--api-key=K", "--api-key-file=", "--key-file=/missing.pem"},        // both, one empty
		{"trades", "--key", "k.pem", "BTCUSDT"},                                        // renamed --key-file
	} {
		stderr.Reset()
		t.Setenv("BINANCE_API_KEY", "")
		t.Setenv("BINANCE_PRIVATE_KEY", "")
		if code := run(args, io.Discard, &stderr); code != 2 {
			t.Errorf("run(%q) = %d, want 2; stderr: %s", args, code, stderr.String())
		}
	}
}

func TestRun_Help(t *testing.T) {
	var stdout bytes.Buffer
	if code := run([]string{"--help"}, &stdout, io.Discard); code != 0 {
		t.Fatalf("--help: exit %d", code)
	}
	for _, want := range []string{"trades", "ticker", "depth", "book", "instruments", "limits", "--api-key", "--testnet"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("--help lacks %q:\n%s", want, stdout.String())
		}
	}
	stdout.Reset()
	if code := run([]string{"--version"}, &stdout, io.Discard); code != 0 || !strings.Contains(stdout.String(), Version) {
		t.Errorf("--version: exit %d, %q", code, stdout.String())
	}
}

// A failing command exits 1, with its error.
func TestRun_Failure(t *testing.T) {
	var stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.pem")
	if code := run([]string{"trades", "--api-key", "k", "--key-file", missing, "BTCUSDT"}, io.Discard, &stderr); code != 1 {
		t.Errorf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "missing.pem") {
		t.Errorf("stderr: %s", stderr.String())
	}
}

func TestLogger(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf, false).Info("logged on", "session", "FIX.4.4:A->SPOT", "in", 1500*time.Millisecond)
	newLogger(&buf, false).Debug("hidden")
	got := buf.String()
	if !strings.Contains(got, "INF") || !strings.Contains(got, "logged on") || !strings.Contains(got, "session=FIX.4.4:A->SPOT") ||
		!strings.Contains(got, "in=1.5s") || strings.Contains(got, "hidden") {
		t.Errorf("console log %q", got)
	}
}

// fakeBinance is a scripted Binance FIX market data server.
type fakeBinance struct {
	t    *testing.T
	pub  ed25519.PublicKey
	conn transport.Conn
	msgs chan *fix2.Message
	seq  int
	cid  string // the client's SenderCompID
}

func acceptBinance(t *testing.T, l *mem.Listener, pub ed25519.PublicKey) *fakeBinance {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := l.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeBinance{t: t, pub: pub, conn: conn, msgs: make(chan *fix2.Message, 100), seq: 1}
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
func (f *fakeBinance) expect(msgType string) *fix2.Message {
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

// logon checks the client's Logon as Binance does and answers it.
func (f *fakeBinance) logon() {
	f.t.Helper()
	m := f.expect("A")
	f.cid = string(m.Get(49))
	payload := strings.Join([]string{"A", f.cid, string(m.Get(56)), string(m.Get(34)), string(m.Get(52))}, "\x01")
	sig, err := base64.StdEncoding.DecodeString(string(m.Get(96)))
	if err != nil || !ed25519.Verify(f.pub, []byte(payload), sig) {
		f.t.Fatalf("invalid Logon signature %q", m.Get(96))
	}
	for tag, want := range map[int]string{56: "SPOT", 34: "1", 98: "0", 141: "Y", 553: "APIKEY", 25035: "2", 95: strconv.Itoa(len(m.Get(96)))} {
		if got := string(m.Get(tag)); got != want {
			f.t.Errorf("Logon tag %d = %q, want %q", tag, got, want)
		}
	}
	f.send("A", "98", "0", "108", string(m.Get(108)), "25037", "4d4c6c6d-0000-4000-8000-000000000000")
}

// send sends a message of msgType with fields after the header.
func (f *fakeBinance) send(msgType string, fields ...string) {
	f.t.Helper()
	b := fix2.NewBuilder("FIX.4.4", msgType).
		SetString(49, "SPOT").SetString(56, f.cid).SetInt(34, f.seq).
		SetString(52, string(fix2.AppendUTCTimestamp(nil, time.Now(), fix2.Microseconds)))
	f.seq++
	for i := 0; i+1 < len(fields); i += 2 {
		tag, _ := strconv.Atoi(fields[i])
		b.Append(tag, []byte(fields[i+1]))
	}
	if _, err := f.conn.Write(b.Build()); err != nil {
		f.t.Fatal(err)
	}
}

// startClient runs args' command against l, returning its output and its
// result.
func startClient(t *testing.T, l *mem.Listener, args func(o *options), cmd string, symbols ...string) (*safeBuffer, context.CancelFunc, chan error) {
	t.Helper()
	keyFile, key := writeKey(t)
	o := &options{apiKey: "APIKEY", keyFile: keyFile, sender: "TESTER", endpoint: "mem", heartbeat: 30 * time.Second, levels: 10, show: 3, json: true}
	if args != nil {
		args(o)
	}
	out := &safeBuffer{}
	log := slog.New(slog.DiscardHandler)
	if os.Getenv("FIX2_TEST_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- streamWith(ctx, o, commands[cmd], symbols, key, newOutput(out, o.json, false), log, l) }()
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

func pub(t *testing.T) ed25519.PublicKey {
	block, _ := pem.Decode([]byte(docKey))
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return k.(ed25519.PrivateKey).Public().(ed25519.PublicKey)
}

func TestTrades(t *testing.T) {
	l := mem.Listen("binance")
	out, cancel, done := startClient(t, l, nil, "trades", "BNBBUSD")
	f := acceptBinance(t, l, pub(t))
	f.logon()
	v := f.expect("V")
	for tag, want := range map[int]string{263: "1", 264: "1", 146: "1", 55: "BNBBUSD", 267: "1", 269: "2"} {
		if got := string(v.Get(tag)); got != want {
			t.Errorf("MarketDataRequest tag %d = %q, want %q", tag, got, want)
		}
	}
	id := string(v.Get(262))
	// As in the documentation: fields out of the dictionary's order, the
	// symbol only on the first entry.
	f.send("X", "262", id, "268", "2",
		"279", "0", "269", "2", "270", "10.00000", "271", "0.01000", "55", "BNBBUSD", "1003", "7", "60", "20241019-05:40:11.464000", "2446", "1",
		"279", "0", "269", "2", "270", "10.10000", "271", "1.50000", "1003", "8", "60", "20241019-05:40:11.465000", "2446", "2")
	lines := waitLines(t, out, 2)
	want := []string{
		`{"type":"trade","symbol":"BNBBUSD","time":"2024-10-19T05:40:11.464Z","trade_id":7,"price":"10","qty":"0.01","aggressor":"buy"}`,
		`{"type":"trade","symbol":"BNBBUSD","time":"2024-10-19T05:40:11.465Z","trade_id":8,"price":"10.1","qty":"1.5","aggressor":"sell"}`,
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d:\n got %s\nwant %s", i, lines[i], want[i])
		}
	}
	// Stopping logs out.
	cancel()
	f.expect("5")
	f.send("5", "58", "Logout acknowledgment.")
	if err := <-done; err != nil {
		t.Errorf("stream: %v", err)
	}
	done <- nil
}

func TestBook(t *testing.T) {
	l := mem.Listen("binance")
	out, _, _ := startClient(t, l, nil, "book", "BTCUSDT")
	f := acceptBinance(t, l, pub(t))
	f.logon()
	v := f.expect("V")
	if got := string(v.Get(264)); got != "10" {
		t.Errorf("MarketDepth = %s, want 10", got)
	}
	id := string(v.Get(262))
	f.send("W", "262", id, "55", "BTCUSDT", "25044", "100", "268", "4",
		"269", "0", "270", "99.00", "271", "1.0",
		"269", "0", "270", "98.50", "271", "2.0",
		"269", "1", "270", "100.00", "271", "3.0",
		"269", "1", "270", "101.00", "271", "4.0")
	// Covered by the snapshot: skipped.
	f.send("X", "262", id, "268", "1", "279", "1", "270", "99.00", "271", "9", "269", "0", "55", "BTCUSDT", "25043", "95", "25044", "100")
	// Straddles the snapshot: applied.
	f.send("X", "262", id, "268", "2",
		"279", "0", "270", "99.50", "271", "0.5", "269", "0", "55", "BTCUSDT", "25043", "99", "25044", "101",
		"279", "2", "270", "100.00", "269", "1")
	lines := waitLines(t, out, 2)
	want := []string{
		`"last_update_id":100,"bids":[["99","1"],["98.5","2"]],"asks":[["100","3"],["101","4"]]}`,
		`"last_update_id":101,"bids":[["99.5","0.5"],["99","1"],["98.5","2"]],"asks":[["101","4"]]}`,
	}
	for i := range want {
		if !strings.HasSuffix(lines[i], want[i]) {
			t.Errorf("line %d:\n got %s\nwant ...%s", i, lines[i], want[i])
		}
	}
	// A gap: the client subscribes again, with a new MDReqID.
	f.send("X", "262", id, "268", "1", "279", "0", "270", "98.00", "271", "1", "269", "0", "55", "BTCUSDT", "25043", "103", "25044", "104")
	unsub := f.expect("V")
	if string(unsub.Get(263)) != "2" || string(unsub.Get(262)) != id {
		t.Errorf("expected an unsubscription of %s, got 263=%s 262=%s", id, unsub.Get(263), unsub.Get(262))
	}
	resub := f.expect("V")
	if string(resub.Get(263)) != "1" || string(resub.Get(262)) == id {
		t.Errorf("expected a new subscription, got 263=%s 262=%s", resub.Get(263), resub.Get(262))
	}
}

func TestTicker(t *testing.T) {
	l := mem.Listen("binance")
	out, _, _ := startClient(t, l, func(o *options) { o.json = false }, "ticker", "BTCUSDT")
	f := acceptBinance(t, l, pub(t))
	f.logon()
	id := string(f.expect("V").Get(262))
	f.send("W", "262", id, "55", "BTCUSDT", "25044", "5", "268", "2",
		"269", "0", "270", "99.00", "271", "1.0", "269", "1", "270", "100.00", "271", "3.0")
	f.send("X", "262", id, "268", "1", "279", "1", "270", "99.10", "271", "2", "269", "0", "55", "BTCUSDT", "25044", "6")
	lines := waitLines(t, out, 2)
	for i, want := range []string{"BTCUSDT bid 1 @ 99  ask 3 @ 100", "BTCUSDT bid 2 @ 99.1  ask 3 @ 100"} {
		if !strings.HasSuffix(lines[i], want) {
			t.Errorf("line %d: got %q, want ...%q", i, lines[i], want)
		}
	}
}

func TestInstruments(t *testing.T) {
	l := mem.Listen("binance")
	out, _, done := startClient(t, l, nil, "instruments", "BTCUSDT")
	f := acceptBinance(t, l, pub(t))
	f.logon()
	x := f.expect("x")
	if string(x.Get(559)) != "0" || string(x.Get(55)) != "BTCUSDT" {
		t.Errorf("InstrumentListRequest 559=%s 55=%s", x.Get(559), x.Get(55))
	}
	f.send("y", "320", string(x.Get(320)), "146", "1", "55", "BTCUSDT", "15", "USDT", "562", "0.00001000",
		"1140", "9000.00000000", "25039", "0.00001000", "969", "0.01000000")
	// Answered: the client logs out on its own.
	f.expect("5")
	f.send("5")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		done <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("instruments did not complete")
	}
	var e instrumentEvent
	if err := json.Unmarshal([]byte(waitLines(t, out, 1)[0]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Symbol != "BTCUSDT" || e.Currency != "USDT" || e.MinQty != "0.00001" || e.MaxQty != "9000" || e.TickSize != "0.01" {
		t.Errorf("instrument %+v", e)
	}
}

func TestLogonRefused(t *testing.T) {
	l := mem.Listen("binance")
	_, _, done := startClient(t, l, nil, "trades", "BTCUSDT")
	f := acceptBinance(t, l, pub(t))
	m := f.expect("A")
	f.cid = string(m.Get(49))
	f.send("5", "58", "Invalid signature.")
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Invalid signature.") {
			t.Errorf("stream = %v, want the Logout's Text", err)
		}
		done <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("a refused logon did not end the command")
	}
}

func TestSubscriptionRefused(t *testing.T) {
	l := mem.Listen("binance")
	_, _, done := startClient(t, l, nil, "trades", "NOPE")
	f := acceptBinance(t, l, pub(t))
	f.logon()
	id := string(f.expect("V").Get(262))
	f.send("Y", "262", id, "25016", "-1121", "58", "Invalid symbol.")
	f.expect("5")
	f.send("5")
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Invalid symbol.") || !strings.Contains(err.Error(), "-1121") {
			t.Errorf("stream = %v", err)
		}
		done <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("a refused subscription did not end the command")
	}
}

// As Binance answers a Logon with an invalid API key.
func TestLogonRejected(t *testing.T) {
	l := mem.Listen("binance")
	_, _, done := startClient(t, l, nil, "trades", "BTCUSDT")
	f := acceptBinance(t, l, pub(t))
	m := f.expect("A")
	f.cid = string(m.Get(49))
	f.send("3", "45", "1", "372", "A", "373", "8", "25016", "-2014", "58", "API-key format invalid.")
	f.send("5", "58", "API-key format invalid.")
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "API-key format invalid. (error code -2014)") {
			t.Errorf("stream = %v", err)
		}
		done <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("a rejected logon did not end the command")
	}
}

// A long answer may come in several messages.
func TestInstruments_SeveralMessages(t *testing.T) {
	l := mem.Listen("binance")
	out, _, done := startClient(t, l, nil, "instruments")
	f := acceptBinance(t, l, pub(t))
	f.logon()
	x := f.expect("x")
	if string(x.Get(559)) != "4" {
		t.Errorf("InstrumentListRequestType %s, want 4 (all)", x.Get(559))
	}
	id := string(x.Get(320))
	f.send("y", "320", id, "146", "1", "55", "BTCUSDT", "15", "USDT")
	f.send("y", "320", id, "146", "1", "55", "ETHUSDT", "15", "USDT")
	f.expect("5")
	f.send("5")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	done <- nil
	if lines := waitLines(t, out, 2); len(lines) != 2 {
		t.Errorf("got %d instruments, want 2:\n%s", len(lines), out.String())
	}
}

// Once logged on, a refused Logon is retried, maxRefusals times in a row.
func TestLogonRefusedAfterReconnect(t *testing.T) {
	saved := initiatorOptions
	initiatorOptions.ReconnectMin, initiatorOptions.ReconnectMax = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { initiatorOptions = saved })
	l := mem.Listen("binance")
	_, _, done := startClient(t, l, nil, "trades", "BTCUSDT")
	f := acceptBinance(t, l, pub(t))
	f.logon()
	f.expect("V")
	f.conn.Close()
	for i := range maxRefusals {
		f = acceptBinance(t, l, pub(t))
		m := f.expect("A")
		f.cid = string(m.Get(49))
		if i%2 == 0 {
			f.send("5", "58", "Invalid signature.")
		} else {
			f.send("3", "45", "1", "372", "A", "373", "8", "25016", "-1022", "58", "Signature for this request is not valid.")
		}
		select {
		case err := <-done:
			if i < maxRefusals-1 {
				t.Fatalf("gave up after %d refusals: %v", i+1, err)
			}
			if err == nil || !strings.Contains(err.Error(), "Invalid signature.") {
				t.Errorf("stream = %v", err)
			}
			done <- nil
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Invalid signature.") {
			t.Errorf("stream = %v", err)
		}
		done <- nil
	case <-time.After(5 * time.Second):
		t.Fatalf("still retrying after %d refusals", maxRefusals)
	}
}

func TestValidate(t *testing.T) {
	l := mem.Listen("binance")
	out, _, _ := startClient(t, l, func(o *options) { o.validate = true }, "depth", "BTCUSDT")
	f := acceptBinance(t, l, pub(t))
	f.logon()
	id := string(f.expect("V").Get(262))
	f.send("W", "262", id, "55", "BTCUSDT", "25044", "7", "268", "1", "269", "0", "270", "99.00", "271", "1.0")
	// Invalid: rejected, not shown.
	f.send("X", "262", id, "268", "1", "279", "9", "270", "99.10", "269", "0")
	r := f.expect("3")
	if string(r.Get(371)) != "279" {
		t.Errorf("Reject RefTagID %s, want 279", r.Get(371))
	}
	f.send("X", "262", id, "268", "1", "279", "1", "270", "99.10", "271", "2", "269", "0", "25043", "8", "25044", "8")
	lines := waitLines(t, out, 2)
	if !strings.Contains(lines[0], `"type":"snapshot"`) || !strings.Contains(lines[1], `"first_update_id":8`) {
		t.Errorf("output:\n%s", out.String())
	}
}

// A Logon that succeeds is no refusal, even when Binance logs out at once,
// before OnLogon runs.
func TestLogoutRightAfterLogonIsNoRefusal(t *testing.T) {
	saved := initiatorOptions
	initiatorOptions.ReconnectMin, initiatorOptions.ReconnectMax = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { initiatorOptions = saved })
	l := mem.Listen("binance")
	_, _, done := startClient(t, l, nil, "trades", "BTCUSDT")
	f := acceptBinance(t, l, pub(t))
	f.logon()
	f.expect("V")
	f.conn.Close()
	for range maxRefusals - 1 {
		f = acceptBinance(t, l, pub(t))
		f.cid = string(f.expect("A").Get(49))
		f.send("5", "58", "Invalid signature.")
	}
	f = acceptBinance(t, l, pub(t))
	f.logon()
	f.send("5", "58", "Server maintenance")
	// Not given up: it connects again.
	f = acceptBinance(t, l, pub(t))
	f.expect("A")
	select {
	case err := <-done:
		t.Fatalf("gave up: %v", err)
	default:
	}
}

// A query failing after others were answered still writes their answers.
func TestInstruments_PartialFailure(t *testing.T) {
	l := mem.Listen("binance")
	out, _, done := startClient(t, l, func(o *options) { o.json = false }, "instruments", "BTCUSDT", "NOPE")
	f := acceptBinance(t, l, pub(t))
	f.logon()
	ok := f.expect("x")
	bad := f.expect("x")
	f.send("y", "320", string(ok.Get(320)), "146", "1", "55", "BTCUSDT", "15", "USDT")
	f.send("3", "45", string(bad.Get(34)), "372", "x", "373", "5", "25016", "-1121", "58", "Invalid symbol.")
	f.expect("5")
	f.send("5")
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Invalid symbol.") {
			t.Errorf("stream = %v", err)
		}
		done <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("instruments did not end")
	}
	if !strings.Contains(out.String(), "BTCUSDT  USDT") {
		t.Errorf("BTCUSDT's row not written:\n%s", out.String())
	}
}

func TestAPIKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api-key")
	if err := os.WriteFile(path, []byte("  KEY123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BINANCE_API_KEY", "FROM-ENV")
	o := &options{apiKeyFile: path, keyFile: "k.pem", heartbeat: 30 * time.Second, levels: 10, show: 1}
	if err := o.check(command{kindLimits}, nil); err != nil {
		t.Fatal(err)
	}
	if err := o.readAPIKey(); err != nil || o.apiKey != "KEY123" {
		t.Errorf("API key %q, %v; want the file's, over the environment's", o.apiKey, err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{empty, filepath.Join(dir, "missing")} {
		var stderr bytes.Buffer
		if code := run([]string{"limits", "--api-key-file", f, "--key-file", "k.pem"}, io.Discard, &stderr); code != 1 {
			t.Errorf("--api-key-file %s: exit %d, want 1; stderr: %s", f, code, stderr.String())
		}
	}
}
