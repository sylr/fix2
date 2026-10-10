package fix40_test

import (
	"errors"
	"fmt"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/tests/fix40"
)

// This example validates FIX 4.0 NewOrderSingle messages with the dictionary
// generated into the package: no XML is read at run time.
func ExampleDictionary() {
	dict := fix40.Dictionary()

	order := func() *fix40.NewOrderSingleBuilder {
		return fix40.BuildNewOrderSingle("FIX.4.0").
			SetSenderCompID("SENDER").
			SetTargetCompID("TARGET").
			SetMsgSeqNum(1).
			SetSendingTime(time.Date(2023, 10, 1, 12, 0, 0, 0, time.UTC)).
			SetClOrdID("ORD-1").
			SetSymbol("AAPL").
			SetSide(fix40.SideBuy).
			SetOrderQty(100).
			SetOrdType(fix40.OrdTypeLimit)
	}

	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"valid order", order().SetHandlInst(fix40.HandlInstAutomatedExecutionOrderPrivateNoBrokerIntervention).Build()},
		{"missing HandlInst", order().Build()},
		{"unknown Side", order().SetHandlInst(fix40.HandlInstManualOrderBestExecution).SetSide("9").Build()},
	} {
		msg, err := fix2.ParseAndValidate(tc.raw, dict)
		var verr *dictionary.ValidationError
		switch {
		case err == nil:
			fmt.Printf("%s: ok, ClOrdID=%s\n", tc.name, msg.Get(fix40.TagClOrdID))
		case errors.As(err, &verr):
			fmt.Printf("%s: tag %d: %v\n", tc.name, verr.Tag, err)
		default:
			fmt.Printf("%s: parse error: %v\n", tc.name, err)
		}
	}

	// Output:
	// valid order: ok, ClOrdID=ORD-1
	// missing HandlInst: tag 21: fix2: required tag 21 missing for message NewOrderSingle
	// unknown Side: tag 54: fix2: invalid value "9" for tag 54 (Side)
}
