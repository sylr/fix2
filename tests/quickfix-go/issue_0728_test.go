package quickfixgo

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/728
//
// A NewOrderSingle ending with an empty field and a stray SOH ("77=\x01\x01")
// failed to parse and was dropped, and the orders sent after it were not
// processed any more. A message with an empty field must be rejected and
// counted as received, and the session must go on with the next ones.
func TestIssue728_EmptyFieldRejectedNextMessagesProcessed(t *testing.T) {
	// Price (44) stands for PositionEffect (77), which the test dictionary
	// lacks.
	for _, tc := range []struct{ name, tail, reason string }{
		{"empty field", "44=\x01", "4"},
		{"empty field and stray SOH", "44=\x01\x01", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rec, p := acceptedPeer(t, func(c *session.Config) { c.Dictionaries.Session = dict(t, "FIX44") })
			body := "35=D\x0134=2\x0149=INI\x0152=" + i728Now() + "\x0156=ACC\x0111=1\x0155=100000\x0154=1\x0160=" + i728Now() + "\x0138=100\x0140=2\x01" + tc.tail
			p.seq++
			p.write(i728Frame("FIX.4.4", body))
			r := p.expect("3", "45", "2")
			if tc.reason != "" {
				if got := string(r.Get(373)); got != tc.reason {
					t.Errorf("SessionRejectReason %q, want %q", got, tc.reason)
				}
			}
			for _, id := range []string{"2", "3"} {
				p.send("D", 0, "11", id, "55", "100000", "54", "1", "60", i728Now(), "38", "100", "40", "2", "44", "18.65")
			}
			rec.wait(t, "the orders after the bad one", func() bool { return len(rec.apps) == 2 })
			if got := rec.appValues(11); got[0] != "2" || got[1] != "3" {
				t.Fatalf("delivered ClOrdIDs %q, want [2 3]", got)
			}
			p.expectNothing(50 * time.Millisecond)
		})
	}
}

// The message of the issue, with its BodyLength and CheckSum made right as
// they were on the wire: the Framer frames it, Parse fails on it, and the
// message after it is framed and parsed.
func TestIssue728_FramerGoesOnAfterUnparsableMessage(t *testing.T) {
	bad := i728Frame("FIX.4.2", "35=D\x0149=GG\x0156=GH\x01115=ABC\x0134=3945\x0143=Y\x0152=20250909-04:26:50.730\x01122=20250909-04:26:47.316\x0111=173794933364992\x011=15501\x0121=1\x0155=100000\x01167=CS\x01207=SS\x0154=1\x0160=20250909-04:26:47.197\x0138=100\x0140=2\x0144=18.65\x0115=CNY\x0159=0\x0177=\x01\x01")
	good := raw("8=FIX.4.2|35=D|49=GG|56=GH|34=3946|52=20250909-04:26:51.730|11=173794933364993|21=1|55=100000|54=1|60=20250909-04:26:51.197|38=100|40=2")
	f := fix2.NewFramer(bytes.NewReader(append(bytes.Clone(bad), good...)), fix2.FramerOptions{})
	for i, want := range [][]byte{bad, good} {
		frame, err := f.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(frame, want) {
			t.Fatalf("frame %d is %s, want %s", i, visible(frame), visible(want))
		}
	}
	if _, err := fix2.Parse(bad); err == nil {
		t.Errorf("Parse(%s) succeeded", visible(bad))
	}
	if _, err := fix2.Parse(good); err != nil {
		t.Errorf("Parse(%s): %v", visible(good), err)
	}
}

func i728Now() string {
	return string(fix2.AppendUTCTimestamp(nil, time.Now(), fix2.Milliseconds))
}

// i728Frame returns a message of beginString with body, the fields between
// BodyLength and CheckSum, as is.
func i728Frame(beginString, body string) []byte {
	return fix2.AppendTrailer([]byte("8="+beginString+"\x019="+strconv.Itoa(len(body))+"\x01"+body), 0)
}
