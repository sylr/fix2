package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/session"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
	"github.com/sylr/fix2/transport/tcp"
)

func serverOpts() serverOptions {
	return serverOptions{
		flow:        fixp.Recoverable,
		clientFlows: []fixp.FlowType{fixp.Recoverable, fixp.Idempotent, fixp.Unsequenced},
		token:       "s3cret",
		keepalive:   100 * time.Millisecond,
		echoWait:    time.Second,
	}
}

func clientOpts(flow fixp.FlowType) clientOptions {
	return clientOptions{
		flow:      flow,
		token:     "s3cret",
		keepalive: 100 * time.Millisecond,
		count:     2000,
		window:    50,
		size:      64,
		timeout:   30 * time.Second,
	}
}

// startServer runs an echo server on l until the test ends.
func startServer(t *testing.T, o serverOptions, l transport.Listener) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, o, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
}

func TestEcho(t *testing.T) {
	for _, serverFlow := range []fixp.FlowType{fixp.Recoverable, fixp.Idempotent, fixp.Unsequenced} {
		for _, clientFlow := range []fixp.FlowType{fixp.Recoverable, fixp.Idempotent, fixp.Unsequenced} {
			t.Run(serverFlow.String()+"/"+clientFlow.String(), func(t *testing.T) {
				l := mem.Listen("echo")
				so := serverOpts()
				so.flow = serverFlow
				startServer(t, so, l)
				for _, size := range []int{minSize, 1024} {
					o := clientOpts(clientFlow)
					o.size = size
					r, err := load(context.Background(), o, l)
					if err != nil {
						t.Fatal(err)
					}
					if r.sent != o.count || r.echoed != o.count || len(r.rtts) != o.count || r.duplicates != 0 || r.notApplied != 0 {
						t.Fatalf("size %d: %+v", size, r)
					}
				}
			})
		}
	}
}

// A window wider than the server session's queue: its echoes wait for
// room, all of them coming.
func TestEchoBackpressure(t *testing.T) {
	l := mem.Listen("echo")
	so := serverOpts()
	so.maxPending = 1
	startServer(t, so, l)
	o := clientOpts(fixp.Recoverable)
	o.count, o.window = 5000, 1000
	r, err := load(context.Background(), o, l)
	if err != nil || r.echoed != o.count || r.duplicates != 0 {
		t.Fatalf("%+v, %v", r, err)
	}
}

// An echo waiting for room past -echo-wait terminates the connection; the
// client's Recoverable flow sends its message again, every echo coming,
// once.
func TestEchoWaitLapses(t *testing.T) {
	l := mem.Listen("echo")
	so := serverOpts()
	so.maxPending, so.echoWait = 1, time.Nanosecond
	startServer(t, so, l)
	o := clientOpts(fixp.Recoverable)
	o.count, o.window, o.reconnect = 200, 100, 10*time.Millisecond
	r, err := load(context.Background(), o, l)
	if err != nil || r.echoed != o.count || r.establishments < 2 {
		t.Fatalf("%d echoed on %d connections, %v", r.echoed, r.establishments, err)
	}
}

// Echoes and NotApplied account for the messages, a duplicate or a
// message never sent, a NotApplied overlapping, beyond or around what is
// outstanding not.
func TestLoaderAccounting(t *testing.T) {
	l := newLoader(clientOptions{count: 5, window: 5})
	ctx := context.Background()
	// Messages 1 to 4 sent, as sequence numbers 11 to 14; 5 not yet.
	for id := uint64(1); id <= 4; id++ {
		l.window <- struct{}{}
		l.sending(id)
		l.sent(id, id+10)
	}
	echo := func(id uint64) error {
		b, _ := probe{id: id, sent: time.Now(), size: minSize}.AppendTo(nil)
		return l.OnMessage(ctx, nil, b, session.Inbound{Received: time.Now()})
	}
	notApplied := func(from uint64, count uint32) {
		if err := l.OnNotApplied(ctx, nil, from, count, session.Inbound{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uint64{0, 5, 6} {
		if echo(id) == nil {
			t.Fatalf("an echo of message %d, never sent", id)
		}
	}
	notApplied(11, 2)           // messages 1 and 2
	notApplied(12, 1)           // 2 again: counted once
	notApplied(1, 10)           // before what was sent
	notApplied(100, 1<<32-1)    // beyond, huge: what is outstanding is looked at, not the range
	notApplied(1<<64-10, 1<<31) // wrapping
	if echo(3) != nil || echo(3) != nil || echo(1) != nil {
		t.Fatal("echo")
	}
	select {
	case <-l.all:
		t.Fatal("accounted for before the last")
	default:
	}
	notApplied(0, 1<<32-1) // message 4, its sequence number 14 among
	select {
	case <-l.all:
		t.Fatal("accounted for before message 5 is sent")
	default:
	}
	// Message 5 takes 16, the engine's own NotApplied having taken 15; a
	// NotApplied naming it comes before Send returns.
	l.window <- struct{}{}
	l.sending(5)
	notApplied(16, 1)
	select {
	case <-l.all:
		t.Fatal("accounted for before Send returned")
	default:
	}
	l.sent(5, 16)
	<-l.all
	if len(l.window) != 0 || l.echoes != 1 || l.duplicates != 2 || l.notApplied != 4 || len(l.bySeq) != 0 {
		t.Fatalf("window %d, echoes %d, duplicates %d, not applied %d, outstanding %d", len(l.window), l.echoes, l.duplicates, l.notApplied, len(l.bySeq))
	}
	if echo(5) != nil || l.duplicates != 3 {
		t.Fatal("an echo of a message lost, not a duplicate")
	}
}

// A NotApplied range reaching past the largest sequence number does not
// wrap to the smallest.
func TestLoaderNotAppliedWraps(t *testing.T) {
	l := newLoader(clientOptions{count: 3, window: 3})
	for id := uint64(1); id <= 3; id++ {
		l.window <- struct{}{}
		l.sending(id)
		l.sent(id, id)
	}
	if err := l.OnNotApplied(context.Background(), nil, 1<<64-1, 3, session.Inbound{}); err != nil {
		t.Fatal(err)
	}
	if l.notApplied != 0 || len(l.bySeq) != 3 {
		t.Fatalf("not applied %d, outstanding %d", l.notApplied, len(l.bySeq))
	}
}

func TestBadToken(t *testing.T) {
	l := mem.Listen("echo")
	startServer(t, serverOpts(), l)
	o := clientOpts(fixp.Idempotent)
	o.token = "wrong"
	_, err := load(context.Background(), o, l)
	var nr *fixp.NegotiationRejectedError
	if !errors.As(err, &nr) || nr.Code != fixp.NegotiationCredentials {
		t.Fatalf("%v", err)
	}
}

// The server retires the session while the client sends: the client's
// run ends at once, with the rejection of its next establishment.
func TestSessionRetired(t *testing.T) {
	l := mem.Listen("echo")
	srv, err := newServer(serverOpts())
	if err != nil {
		t.Fatal(err)
	}
	srv.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	defer func() { cancel(); <-done }()

	o := clientOpts(fixp.Idempotent)
	o.count, o.timeout, o.reconnect = 1_000_000, time.Minute, 10*time.Millisecond
	loaded := make(chan error, 1)
	start := time.Now()
	go func() { _, err := load(context.Background(), o, l); loaded <- err }()
	var sessions []*session.Session
	for len(sessions) == 0 {
		time.Sleep(10 * time.Millisecond)
		sessions = srv.Sessions()
	}
	if err := srv.Retire(context.Background(), sessions[0].ID()); err != nil {
		t.Fatal(err)
	}
	err = <-loaded
	var rej *fixp.EstablishmentRejectedError
	if !errors.As(err, &rej) || rej.Code != fixp.EstablishmentUnnegotiated || time.Since(start) > 10*time.Second {
		t.Fatalf("after %v: %v", time.Since(start), err)
	}
}

// A configuration refused closes the listener.
func TestServeRefusedCloses(t *testing.T) {
	l, err := tcp.Listen("127.0.0.1:0", nil)
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	so := serverOpts()
	so.flow = fixp.None
	if err := serve(context.Background(), so, l); err == nil {
		t.Fatal("a None flow served")
	}
	again, err := net.Listen("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("listener not closed: %v", err)
	}
	again.Close()
}

func TestClientTimeout(t *testing.T) {
	o := clientOpts(fixp.Idempotent)
	o.timeout = 200 * time.Millisecond
	_, err := load(context.Background(), o, mem.Listen("nobody"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

// The server stops while a client sends: its run ends, the client's with
// an error, neither hanging.
func TestServerStops(t *testing.T) {
	l := mem.Listen("echo")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, serverOpts(), l) }()
	o := clientOpts(fixp.Idempotent)
	o.count, o.timeout = 1_000_000, 5*time.Second
	loaded := make(chan error, 1)
	go func() { _, err := load(context.Background(), o, l); loaded <- err }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if err := <-loaded; err == nil {
		t.Fatal("load: no error")
	}
}

func TestTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir)
	stls, err := serverTLS(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	l, err := tcp.Listen("127.0.0.1:0", stls)
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	startServer(t, serverOpts(), l)
	ctls, err := clientTLS(true, certFile, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	r, err := load(context.Background(), clientOpts(fixp.Idempotent), &tcp.Dialer{Addresses: []string{l.Addr().String()}, TLS: ctls})
	if err != nil || r.echoed != 2000 {
		t.Fatalf("%d echoed, %v", r.echoed, err)
	}
	// Without the CA: the certificate is not trusted.
	o := clientOpts(fixp.Idempotent)
	o.timeout = 500 * time.Millisecond
	ctls, _ = clientTLS(true, "", "localhost")
	if _, err := load(context.Background(), o, &tcp.Dialer{Addresses: []string{l.Addr().String()}, TLS: ctls}); err == nil {
		t.Fatal("an untrusted certificate accepted")
	}
}

func TestTLSConfigErrors(t *testing.T) {
	if _, err := serverTLS("cert.pem", ""); err == nil {
		t.Error("-tls-cert without -tls-key")
	}
	if _, err := serverTLS("missing.pem", "missing.key"); err == nil {
		t.Error("missing files")
	}
	if _, err := clientTLS(false, "ca.pem", ""); err == nil {
		t.Error("-tls-ca without -tls")
	}
	empty := filepath.Join(t.TempDir(), "empty.pem")
	os.WriteFile(empty, nil, 0o600)
	if _, err := clientTLS(true, empty, ""); err == nil {
		t.Error("a CA file without a certificate")
	}
	if c, err := serverTLS("", ""); c != nil || err != nil {
		t.Errorf("no TLS: %v, %v", c, err)
	}
}

// writeCert writes a self-signed certificate for localhost and 127.0.0.1.
func writeCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestProbe(t *testing.T) {
	sent := time.Unix(0, 1_700_000_000_123_456_789)
	b, err := probe{id: 42, sent: sent, size: 100}.AppendTo([]byte("x"))
	if err != nil || len(b) != 101 {
		t.Fatalf("%d bytes, %v", len(b), err)
	}
	id, got, err := readProbe(b[1:])
	if err != nil || id != 42 || !got.Equal(sent) {
		t.Fatalf("%d %v %v", id, got, err)
	}
	for _, size := range []int{minSize - 1, maxSize + 1} {
		if _, err := (probe{size: size}).AppendTo(nil); err == nil {
			t.Errorf("size %d accepted", size)
		}
	}
	for _, msg := range [][]byte{b[1:10], append([]byte{0xff, 0xff}, b[3:]...), append(b[1:5:5], 7, 0, 0, 0)} {
		if _, _, err := readProbe(msg); err == nil {
			t.Errorf("% x read", msg)
		}
	}
}

func TestReport(t *testing.T) {
	var b bytes.Buffer
	report(&b, result{sent: 3, echoed: 3, elapsed: time.Second, rtts: []time.Duration{1, 2, 3}})
	if !strings.Contains(b.String(), "echoed 3") || !strings.Contains(b.String(), "max 3ns") {
		t.Fatal(b.String())
	}
	b.Reset()
	report(&b, result{})
	if strings.Contains(b.String(), "round trip") {
		t.Fatal(b.String())
	}
}

func TestRun(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
	}{
		{nil, 2},
		{[]string{"nope"}, 2},
		{[]string{"version"}, 0},
		{[]string{"help"}, 0},
		{[]string{"server", "-h"}, 0},
		{[]string{"server", "-flow", "Sideways"}, 2},
		{[]string{"server", "-flow", "None"}, 1},
		{[]string{"server", "-client-flows", "Idempotent,Nope"}, 2},
		{[]string{"server", "-echo-wait", "0s"}, 2},
		{[]string{"server", "-tls-cert", "c.pem"}, 2},
		{[]string{"server", "-log-level", "loud"}, 2},
		{[]string{"server", "extra"}, 2},
		{[]string{"server", "-listen", "256.0.0.1:1"}, 1},
		{[]string{"client", "-count", "0"}, 2},
		{[]string{"client", "-window", "0"}, 2},
		{[]string{"client", "-size", "3"}, 2},
		{[]string{"client", "-timeout", "0s"}, 2},
		{[]string{"client", "-flow", "None", "-timeout", "1s"}, 1},
		{[]string{"client", "-tls-ca", "ca.pem"}, 2},
		{[]string{"client", "-server-flows", "x"}, 2},
		{[]string{"client", "extra"}, 2},
	} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), c.args, &out, &errOut); code != c.code {
			t.Errorf("%q: exit %d, want %d: %s%s", c.args, code, c.code, out.String(), errOut.String())
		}
	}
}

// The commands, over TCP, until the server is stopped.
func TestRunCommands(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var serverErr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"server", "-listen", addr, "-token", "t", "-keepalive", "100ms", "-log-level", "warn"}, &bytes.Buffer{}, &serverErr)
	}()
	var out, errOut bytes.Buffer
	code := 1
	for range 50 {
		out.Reset()
		errOut.Reset()
		code = run(context.Background(), []string{"client", "-connect", addr, "-token", "t", "-count", "500", "-keepalive", "100ms", "-timeout", "5s", "-log-level", "warn"}, &out, &errOut)
		if code == 0 || !strings.Contains(errOut.String(), "not established") {
			break
		}
	}
	cancel()
	if code != 0 || !strings.Contains(out.String(), "echoed 500") {
		t.Errorf("client: exit %d: %s%s", code, out.String(), errOut.String())
	}
	if code := <-done; code != 0 {
		t.Errorf("server: exit %d: %s", code, serverErr.String())
	}
}
