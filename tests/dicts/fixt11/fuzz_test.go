package fixt11

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
		dicttest.Body(BuildLogon(Version.BeginString).SetHeartBtInt(30).SetDefaultApplVerID("9").Build()),
		dicttest.Body(BuildReject(Version.BeginString).SetRefSeqNum(2).Build()),
		dicttest.SOH("35=A|" + hdr + "98=0|108=30|1137=9|384=2|372=D|385=R|372=8|385=S|"),
		dicttest.SOH("35=A|" + hdr + "98=0|108=-1|1137=|384=99999999999999999999|372=D|"),
		dicttest.SOH("35=A|" + hdr + "95=5|96=ab|c|98=x|93=1|89=a|"),
		dicttest.SOH("35=A|627=2|628=a|629=20231001-12:00:00|630=1|628=b|629=x|" + hdr + "98=0|108=30|"),
		dicttest.SOH("35=2|" + hdr + "7=1|16=0|"),
		dicttest.SOH("35=4|" + hdr + "123=Y|36=99999999999999999999|"),
		dicttest.SOH("35=3|" + hdr + "45=1|371=1|372=D|373=99|58=bad|"),
	} {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var msg fix2.Message
		if err := fix2.ParseInto(&msg, dicttest.Frame(Version.BeginString, body), Dictionary().Profile()); err != nil {
			return
		}
		dicttest.Exercise(WrapHeartbeat(&msg))
		dicttest.Exercise(WrapResendRequest(&msg))
		dicttest.Exercise(WrapLogon(&msg))
		dicttest.Exercise(WrapLogout(&msg))
		dicttest.Exercise(WrapTestRequest(&msg))
		dicttest.Exercise(WrapSequenceReset(&msg))
		dicttest.Exercise(WrapReject(&msg))
	})
}

// TestFuzzGettersCoversEveryMessage fails when FuzzGetters misses a message
// type of the package.
func TestFuzzGettersCoversEveryMessage(t *testing.T) {
	if n := len(Dictionary().Messages); n != 7 {
		t.Errorf("dictionary has %d messages: wrap them all in FuzzGetters", n)
	}
}
