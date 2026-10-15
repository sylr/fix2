package dictionary_test

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/dictionary/xmldict"
)

// loadHighTagPairs loads a dictionary of n LENGTH/DATA pairs around tag
// 900000, with one more pair beyond the profile's range if outlier is set. It
// returns the dictionary and the bytes allocated loading it.
func loadHighTagPairs(t *testing.T, n int, outlier bool) (*dictionary.Dict, uint64) {
	t.Helper()
	var x strings.Builder
	x.WriteString(`<fix type="FIX" major="4" minor="4"><fields>`)
	for i := range n {
		fmt.Fprintf(&x, `<field number="%d" name="Blob%dLen" type="LENGTH"/><field number="%d" name="Blob%d" type="DATA"/>`, 900000+2*i, i, 900001+2*i, i)
	}
	if outlier {
		x.WriteString(`<field number="1048576" name="OutlierLen" type="LENGTH"/><field number="1048577" name="Outlier" type="DATA"/>`)
	}
	x.WriteString(`</fields></fix>`)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	d, err := xmldict.Load(strings.NewReader(x.String()))
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	return d, after.TotalAlloc - before.TotalAlloc
}

// A pair out of the profile's range costs the load no more than the table
// the other pairs need: it used to cost one table per pair.
func TestDictProfile_OutOfRangePairCost(t *testing.T) {
	const n = 128
	_, plain := loadHighTagPairs(t, n, false)
	d, outlier := loadHighTagPairs(t, n, true)
	if outlier > 3*plain {
		t.Errorf("loading with an out-of-range pair allocated %d bytes, %d without", outlier, plain)
	}

	p := d.Profile()
	if got := len(p.DataPairs()); got != n {
		t.Fatalf("%d pairs, want %d", got, n)
	}
	if got := p.DataLengthTag(1048577); got != 0 {
		t.Errorf("the out-of-range DATA field has LENGTH %d, want none", got)
	}
	// The supported pairs still parse as DATA, with SOH in the value.
	length, data := 900000+2*(n-1), 900001+2*(n-1)
	if got := p.DataLengthTag(data); got != length {
		t.Errorf("LENGTH of DATA field %d = %d, want %d", data, got, length)
	}
	body := "35=M\x01" + strconv.Itoa(length) + "=3\x01" + strconv.Itoa(data) + "=a\x01b\x01"
	raw := fix2.AppendTrailer([]byte("8=FIX.4.4\x019="+strconv.Itoa(len(body))+"\x01"+body), 0)
	var m fix2.Message
	if err := fix2.ParseInto(&m, raw, p); err != nil {
		t.Fatalf("ParseInto: %v", err)
	}
	if fix2.ParseInto(&m, raw, nil) == nil {
		t.Error("the default profile parsed a custom DATA field holding SOH")
	}
}
