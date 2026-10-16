package quickfixgo

import (
	"testing"

	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/102
//
// A FIXT MarketDataRequest was rejected with Tag not defined for this message
// type, MDEntryType (269), which belongs to the group of the MDReqGrp
// component: the validator, back from the NoRelatedSym group, skipped the
// counter of the next one, NoMDEntryTypes (267), and took its entries for
// the message's own fields. It must be delivered.
func TestIssue102_GroupOfComponentAfterGroupDelivered(t *testing.T) {
	_, rec, l := startAcceptor(t, func(c *session.Config) {
		c.ID = session.SessionID{BeginString: session.FIXT11, SenderCompID: "ACC", TargetCompID: "INI"}
		c.Dictionaries = session.Dictionaries{
			Session:          dict(t, "FIXT11"),
			Apps:             map[string]*dictionary.Dict{"8": dictXML(t, i102FIX50SP1)},
			DefaultApplVerID: "8",
		}
	})
	p := dialPeer(t, l)
	p.beginString = session.FIXT11
	p.send("A", 0, "98", "0", "108", "30", "1137", "8")
	p.expect("A")
	// The issue's message, its NoRelatedSym entry starting with Symbol (55)
	// as FIX requires an entry to start with its first field (see #103).
	p.send("V", 0, "262", "req_A", "263", "0", "264", "5", "146", "1", "55", "DORZ17", "48", "DORZ17", "22", "99",
		"267", "3", "269", "0", "269", "1", "269", "2")
	rec.wait(t, "MarketDataRequest", func() bool { return len(rec.apps) == 1 })
}

// i102FIX50SP1 is FIX 5.0 SP1's MarketDataRequest cut down to the fields of
// the message.
const i102FIX50SP1 = `<fix type="FIX" major="5" minor="0" servicepack="1">
  <header/>
  <trailer/>
  <messages>
    <message name="MarketDataRequest" msgtype="V" msgcat="app">
      <field name="MDReqID" required="Y"/>
      <field name="SubscriptionRequestType" required="Y"/>
      <field name="MarketDepth" required="Y"/>
      <component name="MDReqGrp" required="Y"/>
      <component name="InstrmtMDReqGrp" required="Y"/>
    </message>
  </messages>
  <components>
    <component name="MDReqGrp">
      <group name="NoMDEntryTypes" required="Y">
        <field name="MDEntryType" required="Y"/>
      </group>
    </component>
    <component name="InstrmtMDReqGrp">
      <group name="NoRelatedSym" required="Y">
        <component name="Instrument" required="Y"/>
      </group>
    </component>
    <component name="Instrument">
      <field name="Symbol" required="N"/>
      <field name="SecurityID" required="N"/>
      <field name="SecurityIDSource" required="N"/>
    </component>
  </components>
  <fields>
    <field number="22" name="SecurityIDSource" type="STRING"/>
    <field number="48" name="SecurityID" type="STRING"/>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="146" name="NoRelatedSym" type="NUMINGROUP"/>
    <field number="262" name="MDReqID" type="STRING"/>
    <field number="263" name="SubscriptionRequestType" type="CHAR">
      <value enum="0" description="SNAPSHOT"/>
      <value enum="1" description="SNAPSHOT_PLUS_UPDATES"/>
      <value enum="2" description="DISABLE_PREVIOUS_SNAPSHOT_PLUS_UPDATE_REQUEST"/>
    </field>
    <field number="264" name="MarketDepth" type="INT"/>
    <field number="267" name="NoMDEntryTypes" type="NUMINGROUP"/>
    <field number="269" name="MDEntryType" type="CHAR">
      <value enum="0" description="BID"/>
      <value enum="1" description="OFFER"/>
      <value enum="2" description="TRADE"/>
    </field>
  </fields>
</fix>`
