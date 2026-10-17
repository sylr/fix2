package binancefix

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
)

// docSignature is the signature of Binance's example, by docKey, of
// SenderCompID EXAMPLE, TargetCompID SPOT, MsgSeqNum 1 and SendingTime
// 20240627-11:17:25.223.
const docSignature = "4MHXelVVcpkdwuLbl6n73HQUXUf1dse2PCgT1DYqW9w8AVZ1RACFGM+5UdlGPrQHrgtS3CvsRURC1oj73j8gCA=="

// docAPIKey is the API key of Binance's example.
const docAPIKey = "sBRXrJx2DsOraMXOaUovEhgVRcjOvCtQwnWj8VxkOh1xqboS02SPGfKi2h8spZJb"

// logonSample is the sample FIX SBE Logon of Binance's FIX API
// documentation, from
// https://github.com/binance/binance-spot-api-docs/blob/f0ee79c8ce24dbeda6645211b9504e237fddef19/fix-api.md
// ("Sample FIX SBE Logon request message"): a little-endian SOFH, the
// 20-byte header, the message. Its sendingTime is 2024-06-27 09:17:25.223
// UTC, its version 0.
const logonSample = `d1 00 00 00 50 eb
	0e 00 28 4e 01 00 00 00 01 00 00 00 58 7a 5f 99 db 1b 06 00
	00 1e 00 00 00 01 02 ff ff ff ff ff ff ff
	07 45 58 41 4d 50 4c 45
	04 53 50 4f 54
	58 00 34 4d 48 58 65 6c 56 56 63 70 6b 64 77 75 4c 62 6c 36 6e 37 33 48 51 55
	58 55 66 31 64 73 65 32 50 43 67 54 31 44 59 71 57 39 77 38 41 56 5a 31
	52 41 43 46 47 4d 2b 35 55 64 6c 47 50 72 51 48 72 67 74 53 33 43 76 73
	52 55 52 43 31 6f 6a 37 33 6a 38 67 43 41 3d 3d
	40 00 73 42 52 58 72 4a 78 32 44 73 4f 72 61 4d 58 4f 61 55 6f 76 45 68 67 56
	52 63 6a 4f 76 43 74 51 77 6e 57 6a 38 56 78 6b 4f 68 31 78 71 62 6f 53
	30 32 53 50 47 66 4b 69 32 68 38 73 70 5a 4a 62`

func unhex(t *testing.T, dump string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(dump), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The signature of Binance's example, its SendingTime text in the
// millisecond form of the FIX example; the client signs the microsecond
// form, which Binance checks (signedSendingTime).
func TestSignatureVector(t *testing.T) {
	at := time.Date(2024, 6, 27, 11, 17, 25, 223e6, time.UTC)
	if s := signedSendingTime(at); s != "20240627-11:17:25.223000" {
		t.Errorf("signedSendingTime = %q", s)
	}
	// Whatever the zone of t: UTC.
	if s := signedSendingTime(at.In(time.FixedZone("CEST", 2*3600))); s != "20240627-11:17:25.223000" {
		t.Errorf("signedSendingTime in CEST = %q", s)
	}
	// The testnet's evidence: the text Binance accepted (2026-10-17).
	if s := signedSendingTime(time.Date(2026, 10, 17, 18, 58, 1, 373e6, time.UTC)); s != "20261017-18:58:01.373000" {
		t.Errorf("signedSendingTime = %q", s)
	}
	if got := sign(testKey(t), SignatureTimeMillis, "EXAMPLE", "SPOT", 1, at); string(got) != docSignature {
		t.Errorf("signature %s, want %s", got, docSignature)
	}
	if p := signaturePayload(SignatureTimeMillis, "EXAMPLE", "SPOT", 1, at); string(p) != "A\x01EXAMPLE\x01SPOT\x011\x0120240627-11:17:25.223" {
		t.Errorf("payload %q", p)
	}
	// The Logon's time is truncated to the microsecond, the SBE field's
	// precision: its text and its value on the wire are one instant.
	if lt := logonTime(at.Add(456*time.Microsecond + 789)); !lt.Equal(at.Add(456 * time.Microsecond)) {
		t.Errorf("logonTime %v", lt)
	}
	if lt := logonTime(at.Add(456 * time.Microsecond)); signedSendingTime(lt) != "20240627-11:17:25.223456" ||
		time.UnixMicro(lt.UnixMicro()).UTC().Format("20060102-15:04:05.000000") != signedSendingTime(lt) {
		t.Errorf("logonTime %v: %q", lt, signedSendingTime(lt))
	}
}

// Our Logon of Binance's example is its SBE sample, but for the
// sendingTime, the instant signed (11:17:25.223 UTC) where the sample
// carries 09:17:25.223 UTC, and the version, the schema's 1 where the
// sample has 0. The signature is the documented one: the sample's
// sendingTime is not the one its signature was made of in UTC, two hours
// apart, which the assumption on the SendingTime's text cannot explain.
func TestLogonSample(t *testing.T) {
	sample := unhex(t, logonSample)
	signedAt := time.Date(2024, 6, 27, 11, 17, 25, 223e6, time.UTC)
	// The sample is signed in the FIX example's millisecond form.
	cfg := &Config{APIKey: docAPIKey, PrivateKey: testKey(t), SenderCompID: "EXAMPLE", HeartBtInt: 30 * time.Second,
		MessageHandling: fixsbe.MessageHandlingSequential, SignatureTime: SignatureTimeMillis}
	m := buildLogon(cfg, sign(cfg.PrivateKey, cfg.SignatureTime, cfg.SenderCompID, TargetCompID, 1, signedAt))
	b, err := m.AppendWithHeader(nil, MessageHeader{SeqNum: 1, SendingTime: signedAt.UnixMicro()})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := lesofh.AppendFrame(nil, b)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Clone(sample)
	binary.LittleEndian.PutUint16(want[lesofh.HeaderLen+6:], fixsbe.SchemaVersion)
	sampleAt := time.UnixMicro(int64(binary.LittleEndian.Uint64(sample[sendingTimeOffset:]))).UTC()
	if !sampleAt.Equal(time.Date(2024, 6, 27, 9, 17, 25, 223e6, time.UTC)) {
		t.Fatalf("the sample's sendingTime %v", sampleAt)
	}
	binary.LittleEndian.PutUint64(want[sendingTimeOffset:], uint64(signedAt.UnixMicro()))
	if !bytes.Equal(frame, want) {
		t.Errorf("Logon\n%x\nwant\n%x", frame, want)
	}
	if sig := sign(cfg.PrivateKey, SignatureTimeMillis, "EXAMPLE", "SPOT", 1, sampleAt); string(sig) == docSignature {
		t.Error("the sample's own sendingTime signs as documented: the assumption is to be revisited")
	}

	// The connection's Logon frame: seqNum 1 and the time given, to the
	// microsecond, stamped once.
	k := &conn{c: &Client{cfg: *cfg}}
	lf, err := k.logonFrame(signedAt.Add(789 * time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lf, want) {
		t.Errorf("logonFrame\n%x\nwant\n%x", lf, want)
	}
}

// The forms of the Logon's sendingTime in its signature, which the logon
// command tries against the testnet: the default is Binance's,
// microsecond FIX text, then the other candidates in order.
func TestSignatureTimes(t *testing.T) {
	// Whatever t's zone: UTC.
	at := time.Date(2024, 6, 27, 9, 17, 25, 223e6, time.UTC).In(time.FixedZone("CEST", 2*3600))
	want := map[SignatureTime][2]string{
		SignatureTimeMillis:     {"ms", "20240627-09:17:25.223"},
		SignatureTimeMicros:     {"us", "20240627-09:17:25.223000"},
		SignatureTimeSeconds:    {"s", "20240627-09:17:25"},
		SignatureTimeNanos:      {"ns", "20240627-09:17:25.223000000"},
		SignatureTimeUnixMicros: {"unix-us", "1719479845223000"},
		SignatureTimeUnixMillis: {"unix-ms", "1719479845223"},
	}
	fs := SignatureTimes()
	if len(fs) != len(want) || !slices.Equal(fs, []SignatureTime{SignatureTimeMicros, SignatureTimeMillis, SignatureTimeSeconds, SignatureTimeNanos, SignatureTimeUnixMicros, SignatureTimeUnixMillis}) || SignatureTime(0) != SignatureTimeMicros {
		t.Fatalf("SignatureTimes() = %v", fs)
	}
	for _, f := range fs {
		w := want[f]
		if f.String() != w[0] || f.Format(at) != w[1] {
			t.Errorf("%d: %q %q, want %q", f, f, f.Format(at), w)
		}
		if g, err := ParseSignatureTime(strings.ToUpper(w[0])); err != nil || g != f {
			t.Errorf("ParseSignatureTime(%q) = %v, %v", w[0], g, err)
		}
	}
	if _, err := ParseSignatureTime("auto"); err == nil {
		t.Error("ParseSignatureTime(auto) accepted")
	}
	if s := SignatureTime(42).String(); s != "SignatureTime(42)" {
		t.Errorf("String of an unknown form %q", s)
	}
	if signedSendingTime(at) != SignatureTimeMicros.Format(at) {
		t.Error("signedSendingTime is not the default form")
	}
	cfg := Config{APIKey: "k", PrivateKey: testKey(t), SenderCompID: "S", SignatureTime: numSignatureTimes}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "SignatureTime") {
		t.Errorf("New with an unknown SignatureTime: %v", err)
	}
}

// The Logon is signed with Config.SignatureTime's form, the default's when
// zero.
func TestLogonSignatureTime(t *testing.T) {
	for _, f := range SignatureTimes() {
		t.Run(f.String(), func(t *testing.T) {
			fk := newFake(t)
			cfg := testConfig(fk, newRecorder())
			cfg.SignatureTime, cfg.HeartBtInt = f, DefaultHeartBtInt
			start(t, cfg, 0)
			p := fk.accept()
			checkLogonAs(t, p.expect(t, fixsbe.LogonTemplateID), &cfg, f)
		})
	}
}

func TestLoadPrivateKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(path, []byte(docKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := LoadPrivateKey(path); err != nil || !k.Equal(testKey(t)) {
		t.Errorf("LoadPrivateKey: %v", err)
	}
	for _, data := range []string{
		"",
		"-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n",
		"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n",
	} {
		if _, err := ParsePrivateKey([]byte(data)); err == nil {
			t.Errorf("ParsePrivateKey(%q) accepted", data)
		}
	}
	if _, err := LoadPrivateKey(filepath.Join(dir, "none")); err == nil {
		t.Error("LoadPrivateKey of no file")
	}
}

func TestPacer(t *testing.T) {
	p := newPacer(Reconnect{MinBackoff: time.Second, MaxBackoff: 4 * time.Second, MaxAttempts: 3, Window: 30 * time.Second})
	t0 := time.Unix(1000, 0)
	if w := p.wait(t0); w != 0 {
		t.Errorf("first wait %v", w)
	}
	// The backoff doubles while attempts fail, up to MaxBackoff, and is
	// reset by a success.
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		p.failed(t0)
		if w := p.wait(t0); w != want {
			t.Errorf("wait %v, want %v", w, want)
		}
		p.notBefore = time.Time{}
	}
	p.succeeded()
	p.failed(t0)
	if w := p.wait(t0); w != time.Second {
		t.Errorf("wait after success %v", w)
	}
	p.notBefore = time.Time{}
	// MaxAttempts in any Window.
	for i := range 3 {
		p.attempt(t0.Add(time.Duration(i) * time.Second))
	}
	if w := p.wait(t0.Add(3 * time.Second)); w != 27*time.Second {
		t.Errorf("wait after 3 attempts %v, want 27s", w)
	}
	if w := p.wait(t0.Add(31 * time.Second)); w != 0 {
		t.Errorf("wait once the first attempt left the window: %v", w)
	}
	// The cooldown.
	p.cooldown(t0.Add(100 * time.Second))
	if w := p.wait(t0.Add(40 * time.Second)); w != 60*time.Second {
		t.Errorf("wait in the cooldown %v", w)
	}
}

func TestStateString(t *testing.T) {
	for s := Queued; s <= PossiblyTransmitted; s++ {
		if s.String() == "invalid" || s.Final() != (s >= Answered) {
			t.Errorf("%d: %q, final %v", s, s, s.Final())
		}
	}
	if State(0).String() != "invalid" {
		t.Error("State(0)")
	}
}
