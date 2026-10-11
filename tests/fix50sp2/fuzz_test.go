package fix50sp2

import (
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/internal/dicttest"
)

// FuzzGetters checks that no getter of any message type, or of the group
// entries it reads, panics on a message a counterparty could send: whatever
// its MsgType, values and group counts.
func FuzzGetters(f *testing.F) {
	const hdr = "49=S|56=T|34=1|52=20231001-12:00:00.000|"
	for _, body := range [][]byte{
		dicttest.Body(BuildNewOrderSingle(Version.BeginString).SetClOrdID("O1").
			AddNoPartyIDsEntry(NewNoPartyIDsEntry().SetPartyID("P1").
				AddNoPartySubIDsEntry(NewNoPartySubIDsEntry().SetPartySubID("s"))).
			AddNoAllocsEntry(NewNoAllocsEntry().SetAllocAccount("A")).Build()),
		dicttest.Body(BuildExecutionReport(Version.BeginString).SetOrderID("O").Build()),
		dicttest.SOH("35=D|" + hdr + "11=O|54=1|60=20231001-12:00:00|40=2|55=X|453=2|448=P1|447=D|452=1|802=1|523=s|803=1|448=P2|"),
		dicttest.SOH("35=D|" + hdr + "11=O|453=99999999999999999999|448=P|38=1e999|44=--1|60=2023|54=99|40=x|"),
		dicttest.SOH("35=D|" + hdr + "453=3|448=P1|802=2|523=a|803=1|523=b|448=P2|448=P3|539=1|524=n|804=1|545=x|"),
		dicttest.SOH("35=8|" + hdr + "37=O|17=E|150=F|39=2|14=-5|6=1.5.5|151=|32=999999999999999999999|136=1|137=1|138=1|"),
		dicttest.SOH("35=8|" + hdr + "136=-4|137=a|899=|555=2|600=X|"),
		dicttest.SOH("35=8|" + hdr + "95=3|96=abc|212=2|213=ab|"),
	} {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var msg fix2.Message
		if err := fix2.ParseInto(&msg, dicttest.Frame(Version.BeginString, body), Dictionary().Profile()); err != nil {
			return
		}
		dicttest.Exercise(WrapNewOrderSingle(&msg))
		dicttest.Exercise(WrapExecutionReport(&msg))
	})
}

// TestFuzzGettersCoversEveryMessage fails when FuzzGetters misses a message
// type of the package.
func TestFuzzGettersCoversEveryMessage(t *testing.T) {
	if n := len(Dictionary().Messages); n != 2 {
		t.Errorf("dictionary has %d messages: wrap them all in FuzzGetters", n)
	}
}
