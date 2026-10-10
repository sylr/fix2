package fix40

import (
	"bytes"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
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

	msg, err := fix2.Parse(raw)
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
