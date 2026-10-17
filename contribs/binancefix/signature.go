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
// to the millisecond, so that its text in the signature payload
// (signedSendingTime) and its value on the wire, in microseconds, are the
// same instant.
func logonTime(t time.Time) time.Time { return t.Truncate(time.Millisecond) }

// signedSendingTime is the text of the Logon's SendingTime in the payload
// it signs.
//
// ASSUMPTION, to be settled against the testnet (M0): Binance documents
// the payload with FIX's text fields, SendingTime being a UTCTIMESTAMP
// such as "20240627-11:17:25.223", but not how the SBE Logon's
// sendingTime, microseconds since the epoch, is turned into text to check
// it. This function takes the FIX form at millisecond precision, in UTC,
// the Logon's time being truncated to the millisecond (logonTime) so that
// no digit is lost whichever precision Binance prints. Binance's own SBE
// sample does not settle it: its sendingTime is 2024-06-27 09:17:25.223
// UTC while its signature, the FIX example's, is that of
// "20240627-11:17:25.223", two hours later, likely a local time (CEST) in
// the making of the example.
func signedSendingTime(t time.Time) string {
	return t.UTC().Format("20060102-15:04:05.000")
}

// signaturePayload returns the text a Logon signs: its MsgType, "A",
// SenderCompID, TargetCompID, MsgSeqNum and SendingTime, separated by SOH.
func signaturePayload(sender, target string, seq uint32, sendingTime time.Time) []byte {
	return []byte(strings.Join([]string{"A", sender, target, strconv.FormatUint(uint64(seq), 10), signedSendingTime(sendingTime)}, "\x01"))
}

// sign returns the RawData of a Logon: the base64 Ed25519 signature of its
// payload.
func sign(key ed25519.PrivateKey, sender, target string, seq uint32, sendingTime time.Time) []byte {
	return base64.StdEncoding.AppendEncode(nil, ed25519.Sign(key, signaturePayload(sender, target, seq, sendingTime)))
}
