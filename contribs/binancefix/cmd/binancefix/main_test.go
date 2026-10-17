package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
	"github.com/sylr/fix2/contribs/binancefix/internal/fakebinance"
	"github.com/sylr/fix2/contribs/binancefix/internal/probe"
	"github.com/sylr/fix2/transport"
)

// docKey is the example private key of Binance's FIX API documentation.
const docKey = `-----BEGIN PRIVATE KEY-----
MC4CAQAwBQYDK2VwBCIEIIJEYWtGBrhACmb9Dvy+qa8WEf0lQOl1s4CLIAB9m89u
-----END PRIVATE KEY-----
`

// harness runs commands against a fake Binance, never the network.
type harness struct {
	t        *testing.T
	x        *fakebinance.Exchange
	keyFile  string
	environ  map[string]string
	mu       sync.Mutex
	dialed   []string // the endpoints dialed
	sleeps   []time.Duration
	out, err bytes.Buffer
}

func newHarness(t *testing.T, b fakebinance.Behaviour, forms ...binancefix.SignatureTime) *harness {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyFile, []byte(docKey), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := binancefix.ParsePrivateKey([]byte(docKey))
	if err != nil {
		t.Fatal(err)
	}
	x := fakebinance.New(fakebinance.Options{APIKey: "api-key", PublicKey: k.Public().(ed25519.PublicKey), SignatureTimes: forms, Behaviour: b})
	return &harness{t: t, x: x, keyFile: keyFile, environ: map[string]string{}}
}

func (h *harness) env() env {
	return env{
		stdout: &h.out, stderr: &h.err,
		getenv: func(k string) string { return h.environ[k] },
		dial: func(endpoint string) transport.Dialer {
			return transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
				h.mu.Lock()
				h.dialed = append(h.dialed, endpoint)
				h.mu.Unlock()
				return h.x.Dial(ctx)
			})
		},
		sleep: func(_ context.Context, d time.Duration) error {
			h.mu.Lock()
			h.sleeps = append(h.sleeps, d)
			h.mu.Unlock()
			return nil
		},
	}
}

// run runs args, keys given unless noKeys, and returns the exit code.
func (h *harness) run(ctx context.Context, args ...string) int {
	h.t.Helper()
	h.out.Reset()
	h.err.Reset()
	code := run(ctx, h.env(), args)
	h.t.Logf("binancefix %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, h.out.String(), h.err.String())
	return code
}

// keys are the flags of the fake's keys.
func (h *harness) keys() []string { return []string{"--api-key", "api-key", "--key-file", h.keyFile} }

// fast are the flags of short waits.
var fast = []string{"--price-far", "70000", "--heartbeat", "5s", "--wait", "1s", "--settle", "20ms", "--interval", "10ms"}

func (h *harness) probeArgs(extra ...string) []string {
	return slices.Concat([]string{"probe"}, h.keys(), fast, extra)
}

func TestUsage(t *testing.T) {
	h := newHarness(t, fakebinance.Behaviour{})
	ctx := context.Background()
	if code := h.run(ctx); code != 2 || !strings.Contains(h.err.String(), "usage:") {
		t.Errorf("no command: %d", code)
	}
	if code := h.run(ctx, "nope"); code != 2 {
		t.Errorf("unknown command: %d", code)
	}
	if code := h.run(ctx, "help"); code != 0 || !strings.Contains(h.out.String(), "binancefix probe") {
		t.Errorf("help: %d", code)
	}
	if code := h.run(ctx, "version"); code != 0 || h.out.String() != "dev\n" {
		t.Errorf("version: %d %q", code, h.out.String())
	}
	if code := h.run(ctx, "logon", "-h"); code != 0 || !strings.Contains(h.err.String(), "-signature-time") {
		t.Errorf("logon -h: %d", code)
	}
	for _, args := range [][]string{
		{"logon", "extra"},
		{"logon", "--signature-time", "minutes"},
		{"logon", "--heartbeat", "3s"},
		{"logon", "--sender", "much-too-long"},
		{"probe", "--interval", "1ms"},
		{"probe", "--signature-time", "auto"},
		{"probe", "nope"},
		{"probe", "--bogus"},
	} {
		if code := h.run(ctx, slices.Concat(args[:1], h.keys(), args[1:])...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if code := h.run(ctx, "probes"); code != 0 || !strings.Contains(h.out.String(), "(e) list-cancel") {
		t.Errorf("probes: %d", code)
	}
	if code := h.run(ctx, "probe", "--list"); code != 0 || strings.Count(h.out.String(), "\n(") != len(probe.Probes())-1 {
		t.Errorf("probe --list: %d", code)
	}
	if len(h.dialed) > 0 {
		t.Errorf("dialed %v", h.dialed)
	}
}

// The API key from a flag, a file or the environment; the private key from
// a PEM file named by a flag or the environment.
func TestKeys(t *testing.T) {
	h := newHarness(t, fakebinance.Behaviour{})
	ctx := context.Background()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "api-key")
	os.WriteFile(keyFile, []byte("  api-key\n"), 0o600)
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte("\n"), 0o600)
	badPEM := filepath.Join(dir, "bad.pem")
	os.WriteFile(badPEM, []byte("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"), 0o600)

	if code := h.run(ctx, "logon"); code != 2 || !strings.Contains(h.err.String(), "no API key") || !strings.Contains(h.err.String(), "no private key") {
		t.Errorf("no keys: %d", code)
	}
	if code := h.run(ctx, "logon", "--api-key", "api-key", "--api-key-file", keyFile, "--key-file", h.keyFile); code != 2 {
		t.Errorf("--api-key and --api-key-file: %d", code)
	}
	if code := h.run(ctx, "logon", "--api-key-file", keyFile, "--key-file", h.keyFile); code != 0 || !strings.Contains(h.out.String(), "accepted") {
		t.Errorf("--api-key-file: %d", code)
	}
	if code := h.run(ctx, "logon", "--api-key-file", empty, "--key-file", h.keyFile); code != 1 || !strings.Contains(h.err.String(), "empty") {
		t.Errorf("empty --api-key-file: %d", code)
	}
	if code := h.run(ctx, "logon", "--api-key-file", filepath.Join(dir, "none"), "--key-file", h.keyFile); code != 1 {
		t.Errorf("missing --api-key-file: %d", code)
	}
	if code := h.run(ctx, "logon", "--api-key", "api-key", "--key-file", badPEM); code != 1 || !strings.Contains(h.err.String(), "PKCS #8") {
		t.Errorf("bad PEM: %d", code)
	}
	h.environ["BINANCE_API_KEY"], h.environ["BINANCE_PRIVATE_KEY"] = "api-key", h.keyFile
	if code := h.run(ctx, "logon"); code != 0 {
		t.Errorf("keys from the environment: %d", code)
	}
	// The flag wins over the environment.
	if code := h.run(ctx, "logon", "--api-key", "another"); code != 1 || !strings.Contains(h.out.String(), "-2015") {
		t.Errorf("--api-key over BINANCE_API_KEY: %d", code)
	}
	// The API key is never shown.
	if code := h.run(ctx, "logon", "--json"); code != 0 || strings.Contains(h.out.String(), "api-key") {
		t.Errorf("JSON logon: %d, %s", code, h.out.String())
	}
}

// probe never leaves the testnet, logon only with --production.
func TestEndpoints(t *testing.T) {
	h := newHarness(t, fakebinance.Behaviour{})
	ctx := context.Background()
	for _, args := range [][]string{
		{"logon", "--endpoint", binancefix.Endpoint},
		{"logon", "--endpoint", "127.0.0.1:9002"},
		{"probe", "--production"},
		{"probe", "--production", "--endpoint", binancefix.TestnetEndpoint},
		{"probe", "--endpoint", binancefix.Endpoint},
		{"probe", "--endpoint", "fix-oe.testnet.binance.vision.example.com:9002"},
	} {
		if code := h.run(ctx, slices.Concat(args[:1], h.keys(), args[1:])...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if len(h.dialed) > 0 {
		t.Fatalf("dialed %v", h.dialed)
	}
	if code := h.run(ctx, slices.Concat([]string{"logon"}, h.keys(), []string{"--production"})...); code != 0 {
		t.Errorf("logon --production: %d", code)
	}
	if code := h.run(ctx, slices.Concat([]string{"logon"}, h.keys())...); code != 0 {
		t.Errorf("logon: %d", code)
	}
	if !slices.Equal(h.dialed, []string{binancefix.Endpoint, binancefix.TestnetEndpoint}) {
		t.Errorf("dialed %v", h.dialed)
	}
}

func TestLogon(t *testing.T) {
	h := newHarness(t, fakebinance.Behaviour{})
	ctx := context.Background()
	if code := h.run(ctx, slices.Concat([]string{"logon"}, h.keys(), []string{"--sender", "S1"})...); code != 0 {
		t.Fatalf("exit %d", code)
	}
	out := h.out.String()
	for _, want := range []string{"as S1, signature time us", "→ 1 Logon", "← 1 LogonAck", "UUID=fake-S1", "accepted: LogonAck seq 1, HeartBtInt 10s"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q", want)
		}
	}
	if code := h.run(ctx, slices.Concat([]string{"logon", "--json"}, h.keys())...); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var lr probe.LogonResult
	if err := json.Unmarshal(h.out.Bytes(), &lr); err != nil || !lr.Accepted || lr.LogonAck.SeqNum != 1 || len(lr.Messages) != 4 {
		t.Errorf("%v %+v", err, lr)
	}

	// Refused: exit 1, the refusal shown.
	h = newHarness(t, fakebinance.Behaviour{}, binancefix.SignatureTimeMillis)
	if code := h.run(ctx, slices.Concat([]string{"logon"}, h.keys())...); code != 1 || !strings.Contains(h.out.String(), "refused by a Reject, error code -1022") {
		t.Errorf("refused: %d", code)
	}
	if code := h.run(ctx, slices.Concat([]string{"logon", "--signature-time", "ms"}, h.keys())...); code != 0 {
		t.Errorf("--signature-time ms: %d", code)
	}
}

// --signature-time auto: each form in turn, twice HeartBtInt apart; exit 3
// when the one accepted is not the client's default, us.
func TestLogonAuto(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, fakebinance.Behaviour{}, binancefix.SignatureTimeMillis)
	if code := h.run(ctx, slices.Concat([]string{"logon", "--signature-time", "auto", "--heartbeat", "7s"}, h.keys())...); code != 3 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), "Binance accepted signature time ms, not us") || !slices.Equal(h.sleeps, []time.Duration{14 * time.Second}) {
		t.Errorf("sleeps %v", h.sleeps)
	}
	if logons := h.x.Logons(); len(logons) != 2 || logons[0].Accepted || !logons[1].Accepted {
		t.Errorf("logons %+v", logons)
	}

	h = newHarness(t, fakebinance.Behaviour{})
	if code := h.run(ctx, slices.Concat([]string{"logon", "--signature-time", "auto", "--json"}, h.keys())...); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var res probe.AutoResult
	if err := json.Unmarshal(h.out.Bytes(), &res); err != nil || res.Accepted != "us" || len(res.Attempts) != 1 || len(h.sleeps) != 0 {
		t.Errorf("%v %+v, sleeps %v", err, res, h.sleeps)
	}
}

func TestProbe(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, fakebinance.Behaviour{})
	if code := h.run(ctx, h.probeArgs()...); code != 0 {
		t.Fatalf("exit %d", code)
	}
	out := h.out.String()
	for _, p := range probe.Probes() {
		if want := "(" + p.Letter + ") " + p.Name + ": CONFIRMED"; !strings.Contains(out, want) {
			t.Errorf("no %q", want)
		}
	}
	if !strings.Contains(out, "summary: 8 confirmed, 0 contradicted, 0 inconclusive") || strings.Contains(out, "WARNING") {
		t.Errorf("summary")
	}
	if open := h.x.Open(); len(open) > 0 {
		t.Errorf("left open: %v", open)
	}

	// Some probes, JSON.
	if code := h.run(ctx, h.probeArgs("--json", "--symbol", "ethusdt", "limits", "b")...); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var rep probe.Report
	if err := json.Unmarshal(h.out.Bytes(), &rep); err != nil || len(rep.Results) != 2 || rep.Results[0].Probe != "order" ||
		rep.Results[1].Probe != "limits" || rep.Symbol != "ETHUSDT" || len(rep.Results[0].Messages) == 0 {
		t.Errorf("%v %+v", err, rep)
	}

	// Contradicted: exit 3.
	h = newHarness(t, fakebinance.Behaviour{Amend: "new", Fill: "trade-only"})
	if code := h.run(ctx, h.probeArgs("amend", "fill", "limits")...); code != 3 {
		t.Fatalf("exit %d", code)
	}
	if out := h.out.String(); !strings.Contains(out, "(c) amend: CONTRADICTED") || !strings.Contains(out, "(f) fill: CONTRADICTED") ||
		!strings.Contains(out, "summary: 1 confirmed, 2 contradicted, 0 inconclusive") {
		t.Errorf("verdicts")
	}
	if open := h.x.Open(); len(open) > 0 {
		t.Errorf("left open: %v", open)
	}

	// The Logon refused: exit 1.
	h = newHarness(t, fakebinance.Behaviour{}, binancefix.SignatureTimeNanos)
	if code := h.run(ctx, h.probeArgs()...); code != 1 || !strings.Contains(h.out.String(), "refused by a Reject") {
		t.Errorf("refused: %d", code)
	}
}

// --price-far has no default: a usage error, telling how to pick it,
// whenever a probe resting limit orders is selected; logon-seqnum, fill
// and limits run without it.
func TestProbePriceRequired(t *testing.T) {
	h := newHarness(t, fakebinance.Behaviour{})
	ctx := context.Background()
	noPrice := []string{"--heartbeat", "5s", "--wait", "1s", "--settle", "20ms", "--interval", "10ms"}
	for _, names := range [][]string{nil, {"order"}, {"amend"}, {"d"}, {"list-cancel"}, {"mass-cancel"}, {"limits", "b"}} {
		if code := h.run(ctx, slices.Concat([]string{"probe"}, h.keys(), noPrice, names)...); code != 2 ||
			!strings.Contains(h.err.String(), "--price-far is required") || !strings.Contains(h.err.String(), "bidMultiplierDown×avg") {
			t.Errorf("%v: exit %d", names, code)
		}
	}
	if len(h.dialed) > 0 {
		t.Fatalf("dialed %v", h.dialed)
	}
	if code := h.run(ctx, slices.Concat([]string{"probe"}, h.keys(), noPrice, []string{"logon-seqnum", "fill", "limits"})...); code != 0 ||
		!strings.Contains(h.out.String(), "market only (no --price-far)") {
		t.Errorf("a, f, h without --price-far: exit %d", code)
	}
	// probe -h tells how to pick it.
	if code := h.run(ctx, "probe", "-h"); code != 0 || !strings.Contains(h.err.String(), "PERCENT_PRICE_BY_SIDE") {
		t.Errorf("probe -h: %d", code)
	}
}

// Interrupted, probe cancels what it placed and exits 1.
func TestProbeInterrupted(t *testing.T) {
	h := newHarness(t, fakebinance.Behaviour{Amend: "silent"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for !slices.ContainsFunc(h.x.Requests(), func(r fakebinance.Request) bool { return r.Name == "OrderAmendKeepPriorityRequest" }) {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	args := slices.Concat([]string{"probe"}, h.keys(), []string{"--price-far", "70000", "--wait", "30s", "--settle", "20ms", "--interval", "10ms", "amend", "limits"})
	if code := h.run(ctx, args...); code != 1 || !strings.Contains(h.err.String(), "interrupted") {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), "cleanup:") || !strings.Contains(h.out.String(), ": canceled") {
		t.Errorf("no cleanup shown")
	}
	if open := h.x.Open(); len(open) > 0 {
		t.Errorf("left open: %v", open)
	}
}

// What the market order bought and could not be sold back is a warning,
// of the exact quantity, and a failure.
func TestProbePositionLeft(t *testing.T) {
	h := newHarness(t, fakebinance.Behaviour{RejectSells: 99})
	if code := h.run(context.Background(), h.probeArgs("fill")...); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), "WARNING: position not traded back, to trade by hand (https://testnet.binance.vision): BTCUSDT 0.001 bought, not sold back") {
		t.Error("no warning")
	}
}

// Requests go out one per --interval at most.
func TestProbePaced(t *testing.T) {
	h := newHarness(t, fakebinance.Behaviour{})
	args := slices.Concat([]string{"probe"}, h.keys(), []string{"--price-far", "70000", "--wait", "1s", "--settle", "10ms", "--interval", "50ms", "order", "mass-cancel"})
	if code := h.run(context.Background(), args...); code != 0 {
		t.Fatalf("exit %d", code)
	}
	reqs := h.x.Requests()
	if len(reqs) < 4 {
		t.Fatalf("%d requests", len(reqs))
	}
	for i := 1; i < len(reqs); i++ {
		if d := reqs[i].At.Sub(reqs[i-1].At); d < 40*time.Millisecond {
			t.Errorf("request %d %v after the last", i, d)
		}
	}
}
