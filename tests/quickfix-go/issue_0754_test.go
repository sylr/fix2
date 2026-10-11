package quickfixgo

import (
	"errors"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
)

// https://github.com/quickfixgo/quickfix/issues/754
//
// A MULTIPLECHARVALUE field holding several valid enums, ExecInst (18) "0 1 a",
// was rejected as Value is incorrect: the whole space-separated value was
// looked up among the enums. Each value must be checked on its own.
func TestIssue754_MultipleValueEnumsCheckedOneByOne(t *testing.T) {
	fixt := dictionary.Compose(dict(t, "FIXT11"), dict(t, "FIX50SP2"))
	fix44 := dictXML(t, i754FIX44)
	for _, tc := range []struct {
		name  string
		d     *dictionary.Dict
		begin string
		appl  string // ApplVerID (1128) field, for FIXT
		value string
		valid bool
	}{
		{"MULTIPLECHARVALUE", fixt, "FIXT.1.1", "1128=9|", "0 1 a", true},
		{"MULTIPLECHARVALUE one", fixt, "FIXT.1.1", "1128=9|", "G", true},
		{"MULTIPLECHARVALUE invalid", fixt, "FIXT.1.1", "1128=9|", "0 # a", false},
		{"MULTIPLEVALUESTRING", fix44, "FIX.4.4", "", "1 5 G", true},
		{"MULTIPLEVALUESTRING invalid", fix44, "FIX.4.4", "", "1 Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := fix2.Parse(raw("8=" + tc.begin + "|35=D|49=INI|56=ACC|34=2|52=20240101-00:00:00.000|" + tc.appl +
				"11=1|55=EUR/USD|54=1|60=20240101-00:00:00.000|38=100|40=1|18=" + tc.value))
			if err != nil {
				t.Fatal(err)
			}
			err = tc.d.ValidateWith(&m, dictionary.CheckAll)
			if tc.valid {
				if err != nil {
					t.Fatalf("ExecInst %q rejected: %v", tc.value, err)
				}
				return
			}
			var ve *dictionary.ValidationError
			if !errors.As(err, &ve) || !errors.Is(err, dictionary.ErrInvalidEnumValue) || ve.Tag != 18 {
				t.Fatalf("ExecInst %q: got %v, want an invalid enum value of tag 18", tc.value, err)
			}
		})
	}
}

// i754FIX44 has ExecInst (18) as FIX 4.4 types it, MULTIPLEVALUESTRING.
const i754FIX44 = `<fix type="FIX" major="4" minor="4" servicepack="0">
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
    <message name="NewOrderSingle" msgtype="D" msgcat="app">
      <field name="ClOrdID" required="Y"/>
      <field name="Symbol" required="Y"/>
      <field name="Side" required="Y"/>
      <field name="TransactTime" required="Y"/>
      <field name="OrderQty" required="N"/>
      <field name="OrdType" required="Y"/>
      <field name="ExecInst" required="N"/>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="8" name="BeginString" type="STRING"/>
    <field number="9" name="BodyLength" type="LENGTH"/>
    <field number="10" name="CheckSum" type="STRING"/>
    <field number="11" name="ClOrdID" type="STRING"/>
    <field number="18" name="ExecInst" type="MULTIPLEVALUESTRING">
      <value enum="1" description="NOT_HELD"/>
      <value enum="5" description="HELD"/>
      <value enum="G" description="ALL_OR_NONE"/>
    </field>
    <field number="34" name="MsgSeqNum" type="SEQNUM"/>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="38" name="OrderQty" type="QTY"/>
    <field number="40" name="OrdType" type="CHAR"/>
    <field number="49" name="SenderCompID" type="STRING"/>
    <field number="52" name="SendingTime" type="UTCTIMESTAMP"/>
    <field number="54" name="Side" type="CHAR"/>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="56" name="TargetCompID" type="STRING"/>
    <field number="60" name="TransactTime" type="UTCTIMESTAMP"/>
  </fields>
</fix>`
