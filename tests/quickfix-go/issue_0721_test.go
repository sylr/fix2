package quickfixgo

import (
	"context"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/721
//
// The reject reasons ValueIsIncorrect and ConditionallyRequiredFieldMissing
// were both 5, taken for a mistake and the latter changed to 8: they are
// values of different fields, SessionRejectReason (373) and
// BusinessRejectReason (380), and 5 is right for each, where 380=8 is
// Throttle limit exceeded. SessionRejectReason has no reason for a
// conditionally required field: a session Reject says Required tag missing
// (1), not 8 (Signature problem); a BusinessMessageReject says 380=5.
func TestIssue721_ConditionallyRequiredFieldMissingReasons(t *testing.T) {
	h := i721Handler{}
	_, _, l := startAcceptor(t, func(c *session.Config) {
		c.ID = session.SessionID{BeginString: session.FIXT11, SenderCompID: "ACC", TargetCompID: "INI"}
		c.Handler = h
		c.Dictionaries = session.Dictionaries{
			Session:          dict(t, "FIXT11"),
			Apps:             map[string]*dictionary.Dict{"9": dictXML(t, i721FIX50SP2)},
			DefaultApplVerID: "9",
		}
	})
	p := dialPeer(t, l)
	p.beginString = session.FIXT11
	p.send("A", 0, "98", "0", "108", "30", "1137", "9")
	p.expect("A")

	// DiscretionOffsetValue (389) without the DiscretionInst (388) its
	// component requires.
	p.send("D", 0, "11", "1", "389", "0.5")
	p.expect("3", "45", "2", "371", "388", "373", "1")

	// Refused by the application for a field it requires.
	p.send("D", 0, "11", "2", "388", "0", "389", "0.5")
	p.expect("j", "45", "3", "380", "5")
}

// i721Handler rejects every order: a field it requires is missing.
type i721Handler struct{ session.BaseHandler }

func (i721Handler) FromApp(context.Context, *session.Session, *fix2.Message, session.Inbound) error {
	return &session.BusinessRejectError{Reason: session.BusinessRejectConditionallyRequiredMissing, Text: "Account (1) missing"}
}

// i721FIX50SP2 has a NewOrderSingle with an optional component one field of
// which is required.
const i721FIX50SP2 = `<fix type="FIX" major="5" minor="0" servicepack="2">
  <header/>
  <trailer/>
  <messages>
    <message name="NewOrderSingle" msgtype="D" msgcat="app">
      <field name="ClOrdID" required="Y"/>
      <component name="DiscretionInstructions" required="N"/>
    </message>
  </messages>
  <components>
    <component name="DiscretionInstructions">
      <field name="DiscretionInst" required="Y"/>
      <field name="DiscretionOffsetValue" required="N"/>
    </component>
  </components>
  <fields>
    <field number="11" name="ClOrdID" type="STRING"/>
    <field number="388" name="DiscretionInst" type="CHAR"/>
    <field number="389" name="DiscretionOffsetValue" type="FLOAT"/>
  </fields>
</fix>`
