package session

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/tests/dicts/fix50sp2"
	"github.com/sylr/fix2/tests/dicts/fixt11"
)

// The ApplVerID is looked for in the standard header only, its DATA fields
// read by their length.
func TestApplVerIDInHeader(t *testing.T) {
	s, err := New(Config{
		ID:      SessionID{BeginString: FIXT11, SenderCompID: "A", TargetCompID: "B"},
		Handler: BaseHandler{},
		Dictionaries: Dictionaries{Session: fixt11.Dictionary(), DefaultApplVerID: "9",
			Apps: map[string]*dictionary.Dict{"9": fix50sp2.Dictionary()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	const head = "8=FIXT.1.1|9=99|35=D|49=B|56=A|34=2|52=20261012-12:00:00.000|"
	for _, tc := range []struct {
		name, frame string
		want        string
		ok          bool
	}{
		{"in the header", head + "1128=10|11=X|10=000|", "10", true},
		{"none", head + "11=X|10=000|", "", false},
		{"after a body field", head + "11=X|1128=10|10=000|", "", false},
		{"no body", head + "10=000|", "", false},
		{"after SecureData holding one", head + "90=8|91=a|1128=7|1128=10|11=X|10=000|", "10", true},
		{"SecureData shorter than its length", head + "90=20|91=a|1128=10|10=000|", "", false},
		{"SecureData length out of range", head + "90=" + strconv.Itoa(math.MaxInt) + "|91=a|1128=10|10=000|", "", false},
		{"SecureData length not a number", head + "90=x|91=a|1128=10|10=000|", "", false},
		{"SecureData without its length", head + "91=a|1128=10|10=000|", "10", true},
		{"LENGTH not followed by its DATA", head + "90=1|1128=10|10=000|", "", false},
		{"malformed tag", head + "4x=1|1128=10|10=000|", "", false},
		{"no equal sign", head + "1128", "", false},
		{"no SOH", head + "1128=10", "", false},
		{"empty", head + "1128=|11=X|10=000|", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := s.dicts.applVerID([]byte(strings.ReplaceAll(tc.frame, "|", "\x01")))
			if string(v) != tc.want || ok != tc.ok {
				t.Errorf("applVerID = %q, %v; want %q, %v", v, ok, tc.want, tc.ok)
			}
		})
	}
}
