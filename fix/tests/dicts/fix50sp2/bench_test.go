package fix50sp2

import (
	"fmt"
	"testing"

	"github.com/sylr/fix2/fix"
)

// BenchmarkNoPartyIDsEntries reads the entries of a top-level group through
// the generated accessor.
func BenchmarkNoPartyIDsEntries(b *testing.B) {
	for _, n := range []int{1, 3, 10, 50} {
		bld := BuildExecutionReport("FIXT.1.1").
			SetOrderID("O-1").
			SetExecID("E-1").
			SetExecType(ExecTypeNew).
			SetOrdStatus(OrdStatusNew).
			SetSymbol("SYM").
			SetSide(SideBuy).
			SetLeavesQty(10).
			SetCumQty(0)
		for i := range n {
			bld.AddNoPartyIDsEntry(NewNoPartyIDsEntry().
				SetPartyID(fmt.Sprintf("P-%d", i)).
				SetPartyIDSource(PartyIDSourceKoreanInvestorId).
				SetPartyRole(PartyRoleExecutingFirm))
		}
		msg, err := fix.Parse(bld.Build())
		if err != nil {
			b.Fatalf("Parse: %v", err)
		}
		er := WrapExecutionReport(&msg)
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := len(er.NoPartyIDsEntries()); got != n {
					b.Fatalf("%d entries, want %d", got, n)
				}
			}
		})
	}
}
