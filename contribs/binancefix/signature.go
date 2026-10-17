package binancefix

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// LoadPrivateKey reads the Ed25519 private key of PEM file path, an
// unencrypted PKCS #8 "PRIVATE KEY" block.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	k, err := ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("private key %s: %w", path, err)
	}
	return k, nil
}

// ParsePrivateKey parses the Ed25519 private key of PEM data, an
// unencrypted PKCS #8 "PRIVATE KEY" block.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("PEM block %q, want an unencrypted PKCS #8 \"PRIVATE KEY\"", block.Type)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("a %T, the FIX API takes Ed25519 keys only", k)
	}
	return ek, nil
}

// logonTime returns the SendingTime a Logon sent at t carries: t truncated
// to the microsecond, the SBE field's precision, so that its text in the
// signature payload (signedSendingTime) and its value on the wire are the
// same instant.
func logonTime(t time.Time) time.Time { return t.Truncate(time.Microsecond) }

// SignatureTime is the text form of the Logon's sendingTime in the payload
// its signature is made of (Config.SignatureTime).
//
// Binance documents the payload with FIX's text fields, SendingTime being
// a UTCTIMESTAMP, but not how the SBE Logon's sendingTime, microseconds
// since the epoch, is turned into text to check it; its own SBE sample
// does not tell either (its sendingTime, 2024-06-27 09:17:25.223 UTC, is
// two hours before the "20240627-11:17:25.223" its signature was made of).
//
// SETTLED against the Spot testnet on 2026-10-17 (M0, cmd/binancefix logon
// --signature-time auto): Binance checks the FIX form in UTC at
// microsecond precision, the SBE field's own, SignatureTimeMicros, the
// zero value. It accepted the Logon signing "20261017-18:58:01.373000",
// refused the same signing the millisecond form with error -1022, and
// numbered its LogonAck 1.
//
// The other forms stay as diagnostics, for cmd/binancefix's logon command
// (--signature-time) to try should Binance change.
type SignatureTime uint8

// The forms of the Logon's sendingTime in its signature, the instant
// 2024-06-27 11:17:25.223 UTC as example.
const (
	// SignatureTimeMicros is "20240627-11:17:25.223000": FIX's
	// UTCTIMESTAMP at microsecond precision, that of Binance's own
	// timestamps and of the SBE field. The default: Binance's.
	SignatureTimeMicros SignatureTime = iota
	// SignatureTimeMillis is "20240627-11:17:25.223": at millisecond
	// precision, the FIX example's. Binance refuses it (-1022).
	SignatureTimeMillis
	// SignatureTimeSeconds is "20240627-11:17:25".
	SignatureTimeSeconds
	// SignatureTimeNanos is "20240627-11:17:25.223000000".
	SignatureTimeNanos
	// SignatureTimeUnixMicros is "1719487045223000": the SBE sendingTime,
	// microseconds since the epoch, in decimal.
	SignatureTimeUnixMicros
	// SignatureTimeUnixMillis is "1719487045223": milliseconds since the
	// epoch, in decimal.
	SignatureTimeUnixMillis
	numSignatureTimes
)

// SignatureTimes returns the forms, the default first, then the other
// candidates, the likelier first.
func SignatureTimes() []SignatureTime {
	fs := make([]SignatureTime, numSignatureTimes)
	for i := range fs {
		fs[i] = SignatureTime(i)
	}
	return fs
}

var signatureTimeNames = [numSignatureTimes]string{"us", "ms", "s", "ns", "unix-us", "unix-ms"}

// String returns the form's name: us, ms, s, ns, unix-us or unix-ms.
func (f SignatureTime) String() string {
	if f < numSignatureTimes {
		return signatureTimeNames[f]
	}
	return "SignatureTime(" + strconv.Itoa(int(f)) + ")"
}

// ParseSignatureTime returns the form named name (see String).
func ParseSignatureTime(name string) (SignatureTime, error) {
	for i, n := range signatureTimeNames {
		if strings.EqualFold(name, n) {
			return SignatureTime(i), nil
		}
	}
	return 0, fmt.Errorf("signature time %q: one of %s", name, strings.Join(signatureTimeNames[:], ", "))
}

// Format returns the text of t in form f, t in UTC.
func (f SignatureTime) Format(t time.Time) string {
	t = t.UTC()
	switch f {
	case SignatureTimeMillis:
		return t.Format("20060102-15:04:05.000")
	case SignatureTimeSeconds:
		return t.Format("20060102-15:04:05")
	case SignatureTimeNanos:
		return t.Format("20060102-15:04:05.000000000")
	case SignatureTimeUnixMicros:
		return strconv.FormatInt(t.UnixMicro(), 10)
	case SignatureTimeUnixMillis:
		return strconv.FormatInt(t.UnixMilli(), 10)
	}
	return t.Format("20060102-15:04:05.000000")
}

// signedSendingTime is the text of the Logon's SendingTime in the payload
// it signs, in the default form, Binance's (see SignatureTime).
func signedSendingTime(t time.Time) string { return SignatureTimeMicros.Format(t) }

// signaturePayload returns the text a Logon signs: its MsgType, "A",
// SenderCompID, TargetCompID, MsgSeqNum and SendingTime in form f,
// separated by SOH.
func signaturePayload(f SignatureTime, sender, target string, seq uint32, sendingTime time.Time) []byte {
	return []byte(strings.Join([]string{"A", sender, target, strconv.FormatUint(uint64(seq), 10), f.Format(sendingTime)}, "\x01"))
}

// sign returns the RawData of a Logon: the base64 Ed25519 signature of its
// payload, its SendingTime in form f.
func sign(key ed25519.PrivateKey, f SignatureTime, sender, target string, seq uint32, sendingTime time.Time) []byte {
	return base64.StdEncoding.AppendEncode(nil, ed25519.Sign(key, signaturePayload(f, sender, target, seq, sendingTime)))
}
