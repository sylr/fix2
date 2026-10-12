package fixlatest

import (
	"testing"

	"github.com/sylr/fix2/dictionary/xmldict"
	"github.com/sylr/fix2/internal/dicttest"
)

// TestDictionaryMatchesXML checks that the generated dictionary is the one
// xmldict.Load reads from the Orchestra repository it was generated from.
func TestDictionaryMatchesXML(t *testing.T) {
	want, err := xmldict.LoadFile("../../../cmd/fixgen/testdata/OrchestraFIXLatest.xml")
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
