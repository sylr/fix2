package fix44

import (
	"testing"

	"github.com/sylr/fix2/fix/dictionary/xmldict"
	"github.com/sylr/fix2/fix/internal/dicttest"
)

// TestDictionaryMatchesXML checks that the generated dictionary is the one
// xmldict.Load reads from the XML it was generated from.
func TestDictionaryMatchesXML(t *testing.T) {
	want, err := xmldict.LoadFile("../../../cmd/fixgen/testdata/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	if diff := dicttest.Diff(Dictionary(), want); diff != "" {
		t.Error(diff)
	}
	if Dictionary() != Dictionary() {
		t.Error("Dictionary returned two dictionaries")
	}
}
