package quickfixgo

import (
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
)

// https://github.com/quickfixgo/quickfix/issues/460
//
// A MarketDataSnapshotFullRefresh with its 8 MDEntries, each with fields an
// entry may leave out, was rejected with Incorrect NumInGroup count.
func TestIssue460_SnapshotEntriesCounted(t *testing.T) {
	d := dictXML(t, i460Dict)
	// The issue's message.
	m, err := fix.Parse(raw("8=FIX.4.2|35=W|34=1652|49=XYZ|52=20210901-09:48:20.444|56=xyz|262=ABCUSD|5000=0|109=abc|55=BTCUSD|268=8|" +
		"269=0|270=47553.20|278=36|271=5.00000000|290=1|269=0|270=47550.20|278=32|271=7.00000000|290=2|" +
		"269=0|270=47547.20|278=2|271=10.00000000|290=3|269=0|270=47544.20|278=102|271=50.00000000|290=4|" +
		"269=1|270=47597.24|278=72|271=5.00000000|290=1|269=1|270=47600.24|278=81|271=7.00000000|290=2|" +
		"269=1|270=47603.24|278=26|271=10.00000000|290=3|269=1|270=47606.24|278=50|271=50.00000000|290=4"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ValidateWith(&m, dictionary.CheckAll); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}
	entries := m.GroupsOf(&fix.GroupSpec{Counter: 268, Delim: 269, Tags: []int{269, 270, 278, 271, 290}})
	if len(entries) != 8 || string(entries[7].Get(278)) != "50" {
		t.Fatalf("got %d MDEntries, want 8", len(entries))
	}
}

// i460Dict is the counterparty's FIX 4.2 MarketDataSnapshotFullRefresh: its
// entries have an MDEntryID (278), and it has a field of its own, 5000.
const i460Dict = `<fix type="FIX" major="4" minor="2" servicepack="0">
  <header>
    <field name="BeginString" required="Y"/>
    <field name="BodyLength" required="Y"/>
    <field name="MsgType" required="Y"/>
    <field name="SenderCompID" required="Y"/>
    <field name="TargetCompID" required="Y"/>
    <field name="MsgSeqNum" required="Y"/>
    <field name="SendingTime" required="Y"/>
  </header>
  <trailer>
    <field name="CheckSum" required="Y"/>
  </trailer>
  <messages>
    <message name="MarketDataSnapshotFullRefresh" msgtype="W" msgcat="app">
      <field name="MDReqID" required="N"/>
      <field name="UserField5000" required="N"/>
      <field name="ClientID" required="N"/>
      <field name="Symbol" required="Y"/>
      <group name="NoMDEntries" required="Y">
        <field name="MDEntryType" required="Y"/>
        <field name="MDEntryPx" required="Y"/>
        <field name="MDEntryID" required="N"/>
        <field name="MDEntrySize" required="N"/>
        <field name="MDEntryPositionNo" required="N"/>
      </group>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="8" name="BeginString" type="STRING"/>
    <field number="9" name="BodyLength" type="LENGTH"/>
    <field number="10" name="CheckSum" type="STRING"/>
    <field number="34" name="MsgSeqNum" type="SEQNUM"/>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="49" name="SenderCompID" type="STRING"/>
    <field number="52" name="SendingTime" type="UTCTIMESTAMP"/>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="56" name="TargetCompID" type="STRING"/>
    <field number="109" name="ClientID" type="STRING"/>
    <field number="262" name="MDReqID" type="STRING"/>
    <field number="268" name="NoMDEntries" type="NUMINGROUP"/>
    <field number="269" name="MDEntryType" type="CHAR">
      <value enum="0" description="BID"/>
      <value enum="1" description="OFFER"/>
    </field>
    <field number="270" name="MDEntryPx" type="PRICE"/>
    <field number="271" name="MDEntrySize" type="QTY"/>
    <field number="278" name="MDEntryID" type="STRING"/>
    <field number="290" name="MDEntryPositionNo" type="INT"/>
    <field number="5000" name="UserField5000" type="INT"/>
  </fields>
</fix>`
