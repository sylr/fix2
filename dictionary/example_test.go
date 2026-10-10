package dictionary_test

import (
	"errors"
	"fmt"
	"log"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
)

// This example parses FIX 4.0 NewOrderSingle messages and validates them
// against a data dictionary: one valid order, one missing a required field and
// one with an enum value the dictionary does not define.
func ExampleDict_Validate() {
	dict, err := dictionary.LoadFile("../cmd/fixgen/testdata/FIX40.xml")
	if err != nil {
		log.Fatal(err)
	}

	// newOrder builds a NewOrderSingle (35=D) with the given Side (54).
	// If handlInst is empty, the required HandlInst (21) field is left out.
	newOrder := func(handlInst, side string) []byte {
		b := fix2.NewBuilder("FIX.4.0", "D").
			SetString(49, "SENDER").            // SenderCompID
			SetString(56, "TARGET").            // TargetCompID
			SetInt(34, 1).                      // MsgSeqNum
			SetString(52, "20231001-12:00:00"). // SendingTime
			SetString(11, "ORD-1")              // ClOrdID
		if handlInst != "" {
			b.SetString(21, handlInst) // HandlInst
		}
		return b.
			SetString(55, "AAPL"). // Symbol
			SetString(54, side).   // Side
			SetInt(38, 100).       // OrderQty
			SetString(40, "2").    // OrdType: LIMIT
			Build()
	}

	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"valid order", newOrder("1", "1")},
		{"missing HandlInst", newOrder("", "1")},
		{"unknown Side", newOrder("1", "9")},
	} {
		msg, err := fix2.ParseAndValidate(tc.raw, dict)

		var verr *dictionary.ValidationError
		switch {
		case err == nil:
			fmt.Printf("%s: ok, ClOrdID=%s\n", tc.name, msg.Get(11))
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
