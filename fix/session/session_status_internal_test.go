package session

import (
	"testing"

	"github.com/sylr/fix2/fix"
)

// dropField drops the fields of a tag, read as AppendBody writes them: a
// DATA value holding what looks like the tag is kept whole.
func TestDropField(t *testing.T) {
	p := fix.DefaultProfile()
	for _, tc := range []struct{ name, body, want string }{
		{"none", "58=a|20001=X|", "58=a|20001=X|"},
		{"one", "1409=4|20001=X|", "20001=X|"},
		{"last", "20001=X|1409=4|", "20001=X|"},
		{"in DATA", "95=10|96=a|1409=4|b|1409=4|", "95=10|96=a|1409=4|b|"},
		{"empty", "", ""},
		{"DATA length too long", "95=1000|96=abc|1409=4|", "95=1000|96=abc|"},
		{"DATA length too short", "95=1|96=abc|1409=4|", "95=1|96=abc|"},
		{"DATA length negative", "95=-1|96=abc|1409=4|", "95=-1|96=abc|"},
		{"DATA length not a number", "95=x|96=abc|1409=4|", "95=x|96=abc|"},
		{"LENGTH last", "1409=4|95=3|", "95=3|"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(soh(tc.body))
			if got := string(dropField(body, tagSessionStatus, p)); got != soh(tc.want) {
				t.Errorf("dropField(%q) = %q, want %q", tc.body, got, soh(tc.want))
			}
		})
	}
}

// soh returns s with | as SOH.
func soh(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] == '|' {
			b[i] = 1
		}
	}
	return string(b)
}
