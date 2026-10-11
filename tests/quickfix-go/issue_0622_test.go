package quickfixgo

import (
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
)

// https://github.com/quickfixgo/quickfix/issues/622
//
// A valid NewOrderSingle was rejected with Tag not defined for this message
// type, RegulatoryTradeID (1903): its Parties entries do not all hold the
// same optional fields, the last one has no PartySubIDs, and the walk of the
// group went astray into the RegulatoryTradeIDGrp component that follows.
func TestIssue622_GroupOfComponentAfterUnevenEntries(t *testing.T) {
	d := dictXML(t, i622Dict)
	// The issue's message, from Bloomberg RFQ.
	m, err := fix2.Parse(raw("8=FIX.4.2|35=D|49=BLPRFQ|56=TGTCOMPID|34=685|50=SENDERSUBID|115=OBOCOMP|116=ON BEHALF OF AB|" +
		"52=20240405-03:58:43|60=20240405-03:58:43.767|120=JPY|423=1|64=20240409|38=181100|40=2|11=ORD_ETF_COMPID_123456|" +
		"131=RFQ_ETF_COMPID_123456|762=ETF|6703=1|44=553.063900|6704=2|15=JPY|75=20240405|6705=553.098100|" +
		"106=MAXIS NY Dow Industrial Averag|107=MAXIS NY DOW INDUST ETF JPYH|167=ETP|48=N.A.|21=3|22=1|54=1|55=2242|" +
		"117=Q_RFQ_ETF_COMPID_123456_1|207=JP|6087=552.900000|" +
		"453=3|448=123456A123ABC1ABCD12|447=N|452=13|802=3|523=OBOCOMP|803=1|523=26121113|803=2|523=ON BEHALF OF ABC|803=9|" +
		"448=BTBS|447=D|452=72|802=1|523=123456ABCD123456|803=4025|" +
		"448=123456ABCDEFGHI12|447=N|452=66|" +
		"1907=1|1903=123456ABCD123456BBG000000000PADDING123456ABCDEFGHI12|1905=BTBS|1906=5|" +
		"768=1|769=20240405-03:58:43.391|770=1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ValidateWith(&m, dictionary.CheckAll); err != nil {
		t.Fatalf("valid NewOrderSingle rejected: %v", err)
	}
	ids := m.GroupsOf(&fix2.GroupSpec{Counter: 1907, Delim: 1903, Tags: []int{1903, 1905, 1906}},
		&fix2.GroupSpec{Counter: 453, Delim: 448, Tags: []int{448, 447, 452},
			Nested: []*fix2.GroupSpec{{Counter: 802, Delim: 523, Tags: []int{523, 803}}}})
	if len(ids) != 1 || string(ids[0].Get(1905)) != "BTBS" {
		t.Fatalf("got %d RegulatoryTradeIDs, want the one sourced BTBS", len(ids))
	}
}

// i622Dict is the issue's dictionary (Bloomberg RFQ, on FIX 4.2) cut down to
// its NewOrderSingle and the fields the message holds.
const i622Dict = `<fix type="FIX" major="4" minor="2" servicepack="0">
  <header>
    <field name="BeginString" required="Y"/>
    <field name="BodyLength" required="Y"/>
    <field name="MsgType" required="Y"/>
    <field name="SenderCompID" required="Y"/>
    <field name="TargetCompID" required="Y"/>
    <field name="OnBehalfOfCompID" required="N"/>
    <field name="MsgSeqNum" required="Y"/>
    <field name="SenderSubID" required="N"/>
    <field name="OnBehalfOfSubID" required="N"/>
    <field name="SendingTime" required="Y"/>
  </header>
  <trailer>
    <field name="CheckSum" required="Y"/>
  </trailer>
  <messages>
    <message name="NewOrderSingle" msgtype="D" msgcat="app">
      <field name="ClOrdID" required="Y"/>
      <field name="SettlDate" required="N"/>
      <field name="Side" required="Y"/>
      <component name="Instrument_Long" required="Y"/>
      <field name="OrdType" required="Y"/>
      <field name="Price" required="N"/>
      <field name="Currency" required="N"/>
      <field name="QuoteReqID" required="Y"/>
      <field name="QuoteID" required="Y"/>
      <field name="TradeDate" required="N"/>
      <field name="OrderQty" required="Y"/>
      <field name="SettlCurrency" required="Y"/>
      <field name="TransactTime" required="Y"/>
      <component name="Parties" required="N"/>
      <component name="RegulatoryTradeIDGrp" required="N"/>
      <component name="TrdRegTimestamps" required="N"/>
      <field name="ReferencePx" required="Y"/>
      <field name="CoverCompetitorCount" required="N"/>
      <field name="CoverCompetitionStatus" required="N"/>
      <field name="CoverPrice" required="N"/>
      <field name="HandInst" required="N"/>
      <field name="PriceType" required="N"/>
    </message>
  </messages>
  <components>
    <component name="Instrument_Long">
      <field name="Symbol" required="Y"/>
      <field name="SecurityID" required="N"/>
      <field name="SecurityIDSource" required="N"/>
      <field name="SecurityType" required="N"/>
      <field name="SecuritySubType" required="N"/>
      <field name="Issuer" required="N"/>
      <field name="SecurityExchange" required="N"/>
      <field name="SecurityDescription" required="N"/>
    </component>
    <component name="Parties">
      <group name="NoPartyIDs" required="N">
        <field name="PartyID" required="N"/>
        <field name="PartyIDSource" required="N"/>
        <field name="PartyRole" required="N"/>
        <component name="PtysSubGrp" required="N"/>
      </group>
    </component>
    <component name="PtysSubGrp">
      <group name="NoPartySubIDs" required="N">
        <field name="PartySubID" required="N"/>
        <field name="PartySubIDType" required="N"/>
      </group>
    </component>
    <component name="RegulatoryTradeIDGrp">
      <group name="NoRegulatoryTradeIDs" required="N">
        <field name="RegulatoryTradeID" required="N"/>
        <field name="RegulatoryTradeIDSource" required="N"/>
        <field name="RegulatoryTradeIDType" required="N"/>
      </group>
    </component>
    <component name="TrdRegTimestamps">
      <field name="NoTrdRegTimestamps" required="N"/>
      <field name="TrdRegTimestamp" required="N"/>
      <field name="TrdRegTimestampType" required="N"/>
    </component>
  </components>
  <fields>
    <field number="8" name="BeginString" type="STRING"/>
    <field number="9" name="BodyLength" type="LENGTH"/>
    <field number="10" name="CheckSum" type="STRING"/>
    <field number="11" name="ClOrdID" type="STRING"/>
    <field number="15" name="Currency" type="CURRENCY"/>
    <field number="21" name="HandInst" type="STRING"/>
    <field number="22" name="SecurityIDSource" type="STRING"/>
    <field number="34" name="MsgSeqNum" type="SEQNUM"/>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="38" name="OrderQty" type="QTY"/>
    <field number="40" name="OrdType" type="CHAR"/>
    <field number="44" name="Price" type="PRICE"/>
    <field number="48" name="SecurityID" type="STRING"/>
    <field number="49" name="SenderCompID" type="STRING"/>
    <field number="50" name="SenderSubID" type="STRING"/>
    <field number="52" name="SendingTime" type="UTCTIMESTAMP"/>
    <field number="54" name="Side" type="CHAR"/>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="56" name="TargetCompID" type="STRING"/>
    <field number="60" name="TransactTime" type="UTCTIMESTAMP"/>
    <field number="64" name="SettlDate" type="LOCALMKTDATE"/>
    <field number="75" name="TradeDate" type="LOCALMKTDATE"/>
    <field number="106" name="Issuer" type="STRING"/>
    <field number="107" name="SecurityDescription" type="STRING"/>
    <field number="115" name="OnBehalfOfCompID" type="STRING"/>
    <field number="116" name="OnBehalfOfSubID" type="STRING"/>
    <field number="117" name="QuoteID" type="STRING"/>
    <field number="120" name="SettlCurrency" type="CURRENCY"/>
    <field number="131" name="QuoteReqID" type="STRING"/>
    <field number="167" name="SecurityType" type="STRING"/>
    <field number="207" name="SecurityExchange" type="STRING"/>
    <field number="423" name="PriceType" type="INT"/>
    <field number="447" name="PartyIDSource" type="CHAR"/>
    <field number="448" name="PartyID" type="STRING"/>
    <field number="452" name="PartyRole" type="INT"/>
    <field number="453" name="NoPartyIDs" type="NUMINGROUP"/>
    <field number="523" name="PartySubID" type="STRING"/>
    <field number="762" name="SecuritySubType" type="STRING"/>
    <field number="768" name="NoTrdRegTimestamps" type="NUMINGROUP"/>
    <field number="769" name="TrdRegTimestamp" type="UTCTIMESTAMP"/>
    <field number="770" name="TrdRegTimestampType" type="INT"/>
    <field number="802" name="NoPartySubIDs" type="NUMINGROUP"/>
    <field number="803" name="PartySubIDType" type="INT"/>
    <field number="1903" name="RegulatoryTradeID" type="STRING"/>
    <field number="1905" name="RegulatoryTradeIDSource" type="STRING"/>
    <field number="1906" name="RegulatoryTradeIDType" type="INT"/>
    <field number="1907" name="NoRegulatoryTradeIDs" type="NUMINGROUP"/>
    <field number="6087" name="ReferencePx" type="FLOAT"/>
    <field number="6703" name="CoverCompetitorCount" type="INT"/>
    <field number="6704" name="CoverCompetitionStatus" type="INT"/>
    <field number="6705" name="CoverPrice" type="PRICE"/>
  </fields>
</fix>`
