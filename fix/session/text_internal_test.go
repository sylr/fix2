package session

import (
	"strconv"
	"testing"

	"github.com/sylr/fix2/fix"
)

// A text is withheld if it holds the beginning of a sensitive value, 4 bytes
// of it at least, however long, as is or quoted: wherever it was cut.
func TestWithhold(t *testing.T) {
	const secret = "S3CRET\tVALUE-0123456789-0123456789-0123456789-0123456789-0123456789"
	m, err := fix.Parse(fix.NewBuilder(FIX44, "D").SetString(11, "O1").SetString(554, secret).SetString(925, "abc").Build())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, text string
		withheld   bool
	}{
		{"whole value", "value " + secret, true},
		{"first 64 bytes, quoted", "value " + quoteValue([]byte(secret)), true},
		{"first 4 bytes", "value S3CR", true},
		{"cut after 8 bytes, quoted", "value " + strconv.Quote(secret[:8]), true},
		{"cut after 20 bytes", "value " + secret[:20] + "...", true},
		{"first 3 bytes", "value S3C", false},
		{"from the middle", "value VALUE-0123", false},
		{"a short value", "value abc", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withhold(&m, noTag, tc.text, "withheld")
			if (got == "withheld") != tc.withheld {
				t.Errorf("withhold(%q) = %q, want withheld %v", tc.text, got, tc.withheld)
			}
		})
	}
}
