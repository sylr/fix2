package tcp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/sylr/fix2/session/transport"
)

func listen(t *testing.T, cfg *tls.Config) *Listener {
	t.Helper()
	l, err := Listen("127.0.0.1:0", cfg)
	if errors.Is(err, syscall.EPERM) {
		t.Skipf("cannot listen in this environment: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// echo accepts one connection and echoes what it reads.
func echo(t *testing.T, l *Listener) <-chan transport.Conn {
	t.Helper()
	ch := make(chan transport.Conn, 1)
	go func() {
		c, err := l.Accept(context.Background())
		if err != nil {
			t.Errorf("Accept: %v", err)
			close(ch)
			return
		}
		ch <- c
		io.Copy(c, c)
	}()
	return ch
}

func roundTrip(t *testing.T, c transport.Conn) {
	t.Helper()
	if _, err := c.Write([]byte("8=FIX.4.4\x01")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "8=FIX.4.4\x01" {
		t.Fatalf("read %q, %v", buf, err)
	}
}

func TestPlain(t *testing.T) {
	l := listen(t, nil)
	echo(t, l)
	c, err := (&Dialer{Addresses: []string{l.Addr().String()}}).Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c)
}

// closedAddr returns an address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if errors.Is(err, syscall.EPERM) {
		t.Skipf("cannot listen in this environment: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestDialerFailover(t *testing.T) {
	l := listen(t, nil)
	go func() {
		for {
			c, err := l.Accept(context.Background())
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	dead := closedAddr(t)
	d := &Dialer{Addresses: []string{dead, l.Addr().String()}}
	c, err := d.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial with a dead first address: %v", err)
	}
	c.Close()
	if d.next != 1 {
		t.Errorf("next = %d, want the address that answered", d.next)
	}
	// All dead: the error names every address.
	d = &Dialer{Addresses: []string{dead, dead}}
	if _, err := d.Dial(context.Background()); err == nil {
		t.Fatal("Dial to dead addresses succeeded")
	}
	if _, err := (&Dialer{}).Dial(context.Background()); err == nil {
		t.Error("Dial without addresses succeeded")
	}
}

func TestAcceptContext(t *testing.T) {
	l := listen(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := l.Accept(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Accept = %v, want the context's error", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Accept returned after %v", d)
	}
	// The listener still works after a cancelled Accept.
	echo(t, l)
	c, err := (&Dialer{Addresses: []string{l.Addr().String()}}).Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c)
}

// testCert returns a self-signed certificate for 127.0.0.1 and localhost.
func testCert(t *testing.T, cn string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, pool
}

func TestMutualTLS(t *testing.T) {
	serverCert, serverPool := testCert(t, "acceptor")
	clientCert, clientPool := testCert(t, "INITIATOR-1")
	l := listen(t, &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientPool,
		MinVersion:   tls.VersionTLS12,
	})
	accepted := echo(t, l)
	d := &Dialer{
		Addresses: []string{l.Addr().String()},
		TLS:       &tls.Config{RootCAs: serverPool, Certificates: []tls.Certificate{clientCert}, MinVersion: tls.VersionTLS12},
	}
	c, err := d.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c)
	sc, ok := (<-accepted).(transport.TLSConn)
	if !ok {
		t.Fatal("accepted connection is not a TLSConn")
	}
	if err := sc.HandshakeContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if certs := sc.ConnectionState().PeerCertificates; len(certs) == 0 || certs[0].Subject.CommonName != "INITIATOR-1" {
		t.Errorf("peer certificates = %v, want the initiator's", certs)
	}
	// A client without the server's CA is refused.
	go func() {
		c, err := l.Accept(context.Background())
		if err != nil {
			return
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.(transport.TLSConn).HandshakeContext(ctx)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bad := &Dialer{Addresses: []string{l.Addr().String()}, TLS: &tls.Config{MinVersion: tls.VersionTLS12}}
	if _, err := bad.Dial(ctx); err == nil {
		t.Error("TLS dial without trusting the server succeeded")
	}
}
