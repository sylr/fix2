package quickfixgo

import (
	"strconv"
	"strings"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
)

// https://github.com/quickfixgo/quickfix/issues/153
//
// A SecurityList of 50 instruments, each holding nested groups (NoInstrAttrib,
// NoUnderlyings), read as a single NoRelatedSym entry: the nested groups'
// fields ended the first entry. All 50 must be read, with their nested
// groups.
func TestIssue153_SecurityListOf50Read(t *testing.T) {
	// Entries of the three shapes of the issue's message: an option on an
	// underlying, a future, a stock.
	shapes := []struct{ sym, fields string }{
		{"UNG", "48=UNG.CBOE.20F2017.C25|22=111|461=OCXXXX|200=20170120|202=25|231=100|207=CBOE|" +
			"870=4|871=500|872=0.05|871=503|872=0.05|871=504|872=1|871=505|872=20170120|711=1|311=N/A|309=UNG.ARCA|305=111|15=USD"},
		{"ZM", "48=ZM.CBOT.Q2017|22=111|461=FXXXXX|200=201708|231=100|207=CBOT|" +
			"870=6|871=500|872=0.1|871=501|872=1632|871=502|872=1632|871=503|872=0.1|871=504|872=1|871=505|872=20170731|15=USD"},
		{"HCA", "48=HCA.NYSE|22=111|461=EXXXXX|207=NYSE|870=3|871=500|872=0.01|871=503|872=0.01|871=504|872=1|15=USD"},
	}
	var b strings.Builder
	b.WriteString("8=FIX.4.4|35=y|34=2|49=XXX|52=20160620-18:26:02.270|56=XXX|320=ID1|322=1|393=40123|560=0|146=50")
	for i := range 50 {
		s := shapes[i%len(shapes)]
		b.WriteString("|55=" + s.sym + strconv.Itoa(i) + "|" + s.fields)
	}
	m, err := fix2.Parse(raw(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if err := dictXML(t, i153Dict).ValidateWith(&m, dictionary.CheckAll); err != nil {
		t.Fatalf("SecurityList rejected: %v", err)
	}

	attribs := &fix2.GroupSpec{Counter: 870, Delim: 871, Tags: []int{871, 872}}
	underlyings := &fix2.GroupSpec{Counter: 711, Delim: 311, Tags: []int{311, 309, 305}}
	syms := m.GroupsOf(&fix2.GroupSpec{Counter: 146, Delim: 55, Tags: []int{55, 48, 22, 461, 200, 202, 231, 207, 15},
		Nested: []*fix2.GroupSpec{attribs, underlyings}})
	if len(syms) != 50 {
		t.Fatalf("got %d instruments, want 50", len(syms))
	}
	for i, e := range syms {
		s := shapes[i%len(shapes)]
		if got, want := string(e.Get(55)), s.sym+strconv.Itoa(i); got != want {
			t.Errorf("instrument %d is %s, want %s", i, got, want)
		}
		if got, want := len(e.GroupsOf(attribs, underlyings)), strings.Count(s.fields, "871="); got != want {
			t.Errorf("instrument %d has %d InstrAttribs, want %d", i, got, want)
		}
		if got, want := len(e.GroupsOf(underlyings, attribs)), strings.Count(s.fields, "311="); got != want {
			t.Errorf("instrument %d has %d underlyings, want %d", i, got, want)
		}
		if string(e.GetTopLevel(15, attribs, underlyings)) != "USD" {
			t.Errorf("instrument %d has no Currency", i)
		}
	}
}

// i153Dict is FIX 4.4's SecurityList cut down to the fields of the message.
const i153Dict = `<fix type="FIX" major="4" minor="4" servicepack="0">
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
    <message name="SecurityList" msgtype="y" msgcat="app">
      <field name="SecurityReqID" required="N"/>
      <field name="SecurityResponseID" required="N"/>
      <field name="SecurityRequestResult" required="N"/>
      <field name="TotNoRelatedSym" required="N"/>
      <group name="NoRelatedSym" required="N">
        <component name="Instrument" required="N"/>
        <component name="InstrumentExtension" required="N"/>
        <group name="NoUnderlyings" required="N">
          <component name="UnderlyingInstrument" required="N"/>
        </group>
        <field name="Currency" required="N"/>
      </group>
    </message>
  </messages>
  <components>
    <component name="Instrument">
      <field name="Symbol" required="N"/>
      <field name="SecurityID" required="N"/>
      <field name="SecurityIDSource" required="N"/>
      <field name="CFICode" required="N"/>
      <field name="MaturityMonthYear" required="N"/>
      <field name="StrikePrice" required="N"/>
      <field name="ContractMultiplier" required="N"/>
      <field name="SecurityExchange" required="N"/>
    </component>
    <component name="InstrumentExtension">
      <group name="NoInstrAttrib" required="N">
        <field name="InstrAttribType" required="N"/>
        <field name="InstrAttribValue" required="N"/>
      </group>
    </component>
    <component name="UnderlyingInstrument">
      <field name="UnderlyingSymbol" required="N"/>
      <field name="UnderlyingSecurityID" required="N"/>
      <field name="UnderlyingSecurityIDSource" required="N"/>
    </component>
  </components>
  <fields>
    <field number="8" name="BeginString" type="STRING"/>
    <field number="9" name="BodyLength" type="LENGTH"/>
    <field number="10" name="CheckSum" type="STRING"/>
    <field number="15" name="Currency" type="CURRENCY"/>
    <field number="22" name="SecurityIDSource" type="STRING"/>
    <field number="34" name="MsgSeqNum" type="SEQNUM"/>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="48" name="SecurityID" type="STRING"/>
    <field number="49" name="SenderCompID" type="STRING"/>
    <field number="52" name="SendingTime" type="UTCTIMESTAMP"/>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="56" name="TargetCompID" type="STRING"/>
    <field number="146" name="NoRelatedSym" type="NUMINGROUP"/>
    <field number="200" name="MaturityMonthYear" type="MONTHYEAR"/>
    <field number="202" name="StrikePrice" type="PRICE"/>
    <field number="207" name="SecurityExchange" type="EXCHANGE"/>
    <field number="231" name="ContractMultiplier" type="FLOAT"/>
    <field number="305" name="UnderlyingSecurityIDSource" type="STRING"/>
    <field number="309" name="UnderlyingSecurityID" type="STRING"/>
    <field number="311" name="UnderlyingSymbol" type="STRING"/>
    <field number="320" name="SecurityReqID" type="STRING"/>
    <field number="322" name="SecurityResponseID" type="STRING"/>
    <field number="393" name="TotNoRelatedSym" type="INT"/>
    <field number="461" name="CFICode" type="STRING"/>
    <field number="560" name="SecurityRequestResult" type="INT"/>
    <field number="711" name="NoUnderlyings" type="NUMINGROUP"/>
    <field number="870" name="NoInstrAttrib" type="NUMINGROUP"/>
    <field number="871" name="InstrAttribType" type="INT"/>
    <field number="872" name="InstrAttribValue" type="STRING"/>
  </fields>
</fix>`
