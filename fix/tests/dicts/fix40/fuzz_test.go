package fix40

import (
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/internal/dicttest"
)

// exercising is a Handler that reads every getter of the messages it gets.
type exercising struct{ handled int }

func (h *exercising) read(m any) error {
	h.handled++
	dicttest.Exercise(m)
	return nil
}

func (h *exercising) OnHeartbeat(m *Heartbeat) error             { return h.read(m) }
func (h *exercising) OnTestRequest(m *TestRequest) error         { return h.read(m) }
func (h *exercising) OnResendRequest(m *ResendRequest) error     { return h.read(m) }
func (h *exercising) OnReject(m *Reject) error                   { return h.read(m) }
func (h *exercising) OnSequenceReset(m *SequenceReset) error     { return h.read(m) }
func (h *exercising) OnLogout(m *Logout) error                   { return h.read(m) }
func (h *exercising) OnExecutionReport(m *ExecutionReport) error { return h.read(m) }
func (h *exercising) OnLogon(m *Logon) error                     { return h.read(m) }
func (h *exercising) OnNewOrderSingle(m *NewOrderSingle) error   { return h.read(m) }

var _ Handler = (*exercising)(nil)

// FuzzGetters checks that no getter of any message type, or of the group
// entries it reads, panics on a message a counterparty could send: whatever
// its MsgType, values and group counts. Dispatch must route it to its
// handler, or report it unsupported.
func FuzzGetters(f *testing.F) {
	const hdr = "49=S|56=T|34=1|52=20231001-12:00:00|"
	for _, body := range [][]byte{
		dicttest.Body(BuildNewOrderSingle(Version.BeginString).SetClOrdID("O1").Build()),
		dicttest.Body(BuildLogon(Version.BeginString).SetHeartBtInt(30).Build()),
		dicttest.SOH("35=8|" + hdr + "37=O|11=O1|17=E|20=0|39=0|55=IBM|54=1|38=1|14=0|6=0|136=2|137=1|138=EUR|139=1|137=2|138=USD|139=2|"),
		dicttest.SOH("35=8|" + hdr + "136=99999999999999999999|137=1e999|138=|139=99|"),
		dicttest.SOH("35=8|" + hdr + "136=3|137=1|137=2|"),
		dicttest.SOH("35=D|" + hdr + "11=O|21=1|55=X|54=1|60=20231001-12:00:00|38=1.2.3|40=2|44=--5|"),
		dicttest.SOH("35=A|" + hdr + "98=0|108=30|95=5|96=ab\x01cd|"),
		dicttest.SOH("35=A|" + hdr + "95=99|96=ab|98=9|108=x|"),
		dicttest.SOH("35=2|" + hdr + "7=1|16=0|"),
		dicttest.SOH("35=Z|" + hdr),
	} {
		f.Add(body)
	}
	// Every message type with every field, for each getter to find its own.
	for _, body := range dicttest.FullAll(Dictionary()) {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var msg fix.Message
		if err := fix.ParseInto(&msg, dicttest.Frame(Version.BeginString, body), Dictionary().Profile()); err != nil {
			return
		}
		dicttest.Exercise(WrapHeartbeat(&msg))
		dicttest.Exercise(WrapTestRequest(&msg))
		dicttest.Exercise(WrapResendRequest(&msg))
		dicttest.Exercise(WrapReject(&msg))
		dicttest.Exercise(WrapSequenceReset(&msg))
		dicttest.Exercise(WrapLogout(&msg))
		dicttest.Exercise(WrapExecutionReport(&msg))
		dicttest.Exercise(WrapLogon(&msg))
		dicttest.Exercise(WrapNewOrderSingle(&msg))

		h := &exercising{}
		err := Dispatch(&msg, h)
		_, known := Dictionary().Messages[string(msg.MsgType())]
		switch {
		case known && (err != nil || h.handled != 1):
			t.Fatalf("Dispatch of known %q = %v, handled %d", msg.MsgType(), err, h.handled)
		case !known && (err == nil || h.handled != 0):
			t.Fatalf("Dispatch of unknown %q = %v, handled %d", msg.MsgType(), err, h.handled)
		}
	})
}

// TestFuzzGettersCoversEveryMessage fails when FuzzGetters misses a message
// type of the package.
func TestFuzzGettersCoversEveryMessage(t *testing.T) {
	if n := len(Dictionary().Messages); n != 9 {
		t.Errorf("dictionary has %d messages: wrap them all in FuzzGetters", n)
	}
}
