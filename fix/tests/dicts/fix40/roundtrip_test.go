package fix40

import (
	"bytes"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
)

// TestTimeAndDateRoundTrip checks the FIX 4.0 TIME (SendingTime, ExpireTime)
// and DATE (FutSettDate) fields: TIME is written as YYYYMMDD-HH:MM:SS without
// milliseconds, DATE as YYYYMMDD, and both parse back to time.Time.
func TestTimeAndDateRoundTrip(t *testing.T) {
	sending := time.Date(2023, 10, 1, 12, 30, 45, 0, time.UTC)
	settle := time.Date(2023, 10, 3, 0, 0, 0, 0, time.UTC)

	raw := BuildNewOrderSingle("FIX.4.0").
		SetSenderCompID("S").
		SetTargetCompID("T").
		SetMsgSeqNum(1).
		SetSendingTime(sending.Add(678 * time.Millisecond)). // truncated to seconds
		SetClOrdID("ORD-1").
		SetHandlInst(HandlInstAutomatedExecutionOrderPrivateNoBrokerIntervention).
		SetSymbol("AAPL").
		SetSide(SideBuy).
		SetOrderQty(100).
		SetOrdType(OrdTypeLimit).
		SetFutSettDate(settle).
		Build()

	for _, want := range []string{"\x0152=20231001-12:30:45\x01", "\x0164=20231003\x01"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("message %q does not contain %q", raw, want)
		}
	}

	msg, err := fix.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	nos := WrapNewOrderSingle(&msg)
	if got, ok, err := nos.SendingTime(); err != nil || !ok || !got.Equal(sending) {
		t.Errorf("SendingTime = (%v, %v, %v), want %v", got, ok, err, sending)
	}
	if got, ok, err := nos.FutSettDate(); err != nil || !ok || !got.Equal(settle) {
		t.Errorf("FutSettDate = (%v, %v, %v), want %v", got, ok, err, settle)
	}
	if _, ok, err := nos.ExpireTime(); err != nil || ok {
		t.Errorf("ExpireTime = (_, %v, %v), want absent", ok, err)
	}
}

// TestTimeZones checks that DATE (a local market date) keeps the civil date it
// is given in any time zone, while TIME (UTC) is converted to UTC.
func TestTimeZones(t *testing.T) {
	plus2 := time.FixedZone("UTC+2", 2*3600)
	minus5 := time.FixedZone("UTC-5", -5*3600)

	for _, tc := range []struct {
		name     string
		sending  time.Time
		settle   time.Time
		wantWire []string
	}{
		{
			name:     "east of UTC",
			sending:  time.Date(2023, 10, 1, 1, 0, 0, 0, plus2), // 2023-09-30 23:00 UTC
			settle:   time.Date(2023, 10, 3, 0, 0, 0, 0, plus2), // still 2023-10-03 locally
			wantWire: []string{"\x0152=20230930-23:00:00\x01", "\x0164=20231003\x01"},
		},
		{
			name:     "west of UTC",
			sending:  time.Date(2023, 10, 1, 20, 0, 0, 0, minus5), // 2023-10-02 01:00 UTC
			settle:   time.Date(2023, 10, 3, 23, 0, 0, 0, minus5), // still 2023-10-03 locally
			wantWire: []string{"\x0152=20231002-01:00:00\x01", "\x0164=20231003\x01"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both the plain and WithFormat setters must behave the same.
			for _, raw := range [][]byte{
				BuildNewOrderSingle("FIX.4.0").
					SetSendingTime(tc.sending).
					SetFutSettDate(tc.settle).
					Build(),
				BuildNewOrderSingle("FIX.4.0").
					SetSendingTimeWithFormat(tc.sending, fix.TimeFormatTimestampSec).
					SetFutSettDateWithFormat(tc.settle, fix.TimeFormatDate).
					Build(),
			} {
				for _, want := range tc.wantWire {
					if !bytes.Contains(raw, []byte(want)) {
						t.Errorf("message %q does not contain %q", raw, want)
					}
				}
			}
		})
	}
}

// TestTrailerSignature sets the trailer's Signature through the typed
// builder: it is written last, with its SignatureLength, and read back.
func TestTrailerSignature(t *testing.T) {
	sig := []byte("s\x01g")
	raw := BuildHeartbeat("FIX.4.0").
		SetSenderCompID("S").SetTargetCompID("T").SetMsgSeqNum(1).
		SetSendingTime(time.Date(2023, 10, 1, 12, 0, 0, 0, time.UTC)).
		SetSignature(sig).
		Build()
	if !bytes.Contains(raw, []byte("\x0193=3\x0189=s\x01g\x0110=")) {
		t.Fatalf("Signature not last: %q", raw)
	}
	msg, err := fix.ParseAndValidate(raw, Dictionary())
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := WrapHeartbeat(&msg).Signature(); !ok || !bytes.Equal(v, sig) {
		t.Errorf("Signature = %q, %v", v, ok)
	}
	if n, ok, err := WrapHeartbeat(&msg).SignatureLength(); !ok || err != nil || n != len(sig) {
		t.Errorf("SignatureLength = %d, %v, %v", n, ok, err)
	}
}

// Reset removes the repeating group entries of the previous message too.
func TestBuilderResetClearsGroups(t *testing.T) {
	b := BuildExecutionReport("FIX.4.0")
	b.SetOrderID("O1").AddNoMiscFeesEntry(NewNoMiscFeesEntry().SetMiscFeeAmt(1))
	if !bytes.Contains(b.Build(), []byte("\x01136=1\x01137=1\x01")) {
		t.Fatal("first message lacks its group")
	}
	b.Reset()
	b.SetOrderID("O2")
	if got := b.Build(); bytes.Contains(got, []byte("136=")) {
		t.Errorf("after Reset: %q still holds the previous message's group", got)
	}
}
