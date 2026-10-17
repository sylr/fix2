package binancefix_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
	"github.com/sylr/fix2/contribs/binancefix/internal/probe"
)

// The testnet tests check, against Binance's Spot testnet, what the client
// relies on, running the probes of cmd/binancefix (internal/probe): all
// confirmed on 2026-10-17 (README, "Settled on the testnet"), they are
// the regression check should Binance change. They never run by default,
// nor in CI: they need
//
//	BINANCEFIX_TESTNET=1
//	BINANCE_API_KEY=<an Ed25519 API key of the testnet, FIX_API permission>
//	BINANCE_PRIVATE_KEY=<the PEM file of its private key>
//	BINANCEFIX_PRICE_FAR=<the price of the limit orders> (TestTestnetProbes)
//
// BINANCEFIX_PRICE_FAR has no default, the market moving: below the market,
// inside both PERCENT_PRICE_BY_SIDE bands (probe.PriceFarHelp), such as
// 70000 for BTCUSDT at about 81,500. Optionally BINANCEFIX_SIGNATURE_TIME
// (us, the default, ms, s, ns, unix-us, unix-ms), BINANCEFIX_SYMBOL and
// BINANCEFIX_QTY (see the README):
//
//	BINANCEFIX_TESTNET=1 BINANCEFIX_PRICE_FAR=70000 go test -run Testnet -v .
//
// A contradicted assumption fails its test; an inconclusive one is
// skipped, with why. They connect to the testnet only.

// testnet returns the Options of the testnet tests, or skips the test.
func testnet(t *testing.T) probe.Options {
	t.Helper()
	if os.Getenv("BINANCEFIX_TESTNET") != "1" {
		t.Skip("against the Spot testnet: set BINANCEFIX_TESTNET=1, BINANCE_API_KEY and BINANCE_PRIVATE_KEY")
	}
	apiKey, keyFile := os.Getenv("BINANCE_API_KEY"), os.Getenv("BINANCE_PRIVATE_KEY")
	if apiKey == "" || keyFile == "" {
		t.Skip("BINANCE_API_KEY and BINANCE_PRIVATE_KEY (the PEM file of its Ed25519 private key) are required")
	}
	key, err := binancefix.LoadPrivateKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	o := probe.Options{
		Endpoint:   binancefix.TestnetEndpoint,
		APIKey:     apiKey,
		PrivateKey: key,
		Symbol:     os.Getenv("BINANCEFIX_SYMBOL"),
		PriceFar:   os.Getenv("BINANCEFIX_PRICE_FAR"),
		Qty:        os.Getenv("BINANCEFIX_QTY"),
		Logf:       t.Logf,
	}
	if s := os.Getenv("BINANCEFIX_SIGNATURE_TIME"); s != "" {
		if o.SignatureTime, err = binancefix.ParseSignatureTime(s); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

// The Logon is accepted, its SendingTime signed in the client's default
// form, us (M0, settled on 2026-10-17), and Binance's LogonAck numbered 1.
func TestTestnetLogon(t *testing.T) {
	o := testnet(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	lr := probe.Logon(ctx, o)
	for _, m := range lr.Messages {
		t.Log(m)
	}
	switch {
	case lr.Refusal != nil:
		t.Fatalf("Logon refused (%s %s): run binancefix logon --signature-time auto to find the form Binance signs (signed %q)",
			lr.Refusal.Message, lr.Refusal.Text, lr.SignedSendingTime)
	case !lr.Accepted:
		t.Fatalf("Logon failed: %s", lr.Error)
	case lr.LogonAck.SeqNum != 1:
		t.Fatalf("LogonAck numbered %d, the client requires 1", lr.LogonAck.SeqNum)
	case lr.Error != "":
		t.Fatalf("LogonAck refused by the client: %s", lr.Error)
	}
}

// Each probe, one test: confirmed passes, contradicted fails, inconclusive
// is skipped.
func TestTestnetProbes(t *testing.T) {
	o := testnet(t)
	if o.PriceFar == "" {
		t.Fatalf("BINANCEFIX_PRICE_FAR is required, the price of the limit orders: %s", probe.PriceFarHelp)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rep, err := probe.Run(ctx, o)
	if rep.Logon != nil && !rep.Logon.Accepted {
		t.Fatalf("Logon: %s", rep.Logon.Error)
	}
	for _, res := range rep.Results {
		t.Run(res.Letter+"-"+res.Probe, func(t *testing.T) {
			t.Log(res.Assumption)
			for _, m := range res.Messages {
				t.Log(m)
			}
			var why []string
			for _, c := range res.Checks {
				why = append(why, string(c.Verdict)+": "+c.Name+": "+c.Detail)
			}
			switch res.Verdict {
			case probe.Contradicted:
				t.Errorf("contradicted:\n%s", strings.Join(why, "\n"))
			case probe.Inconclusive:
				t.Skipf("inconclusive:\n%s", strings.Join(why, "\n"))
			default:
				t.Log(strings.Join(why, "\n"))
			}
		})
	}
	for _, c := range rep.Cleanup {
		t.Logf("cleanup %s (%s): %s %s", c.ClOrdID, c.Probe, c.Outcome, c.Detail)
	}
	if err != nil {
		t.Errorf("Run: %v", err)
	}
	if len(rep.Leftover) > 0 {
		t.Errorf("orders possibly left open on the testnet: %v", rep.Leftover)
	}
}
