package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sylr/fix2/dictionary"
)

func TestGenerate(t *testing.T) {
	dict, err := dictionary.LoadFile("testdata/FIX44.xml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	outDir := t.TempDir()
	pkg := "fix44"
	if err := generate(dict, pkg, outDir); err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Verify expected files exist.
	want := []string{
		"doc.go",
		"fields.go",
		"msg_heartbeat.go",
		"msg_new_order_single.go",
	}
	for _, name := range want {
		path := filepath.Join(outDir, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %s: %v", name, err)
		}
	}

	// Verify generated code compiles.
	// We create a temporary go.mod that points fix2 at our local source.
	modRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	goMod := "module testgen\ngo 1.26\nrequire github.com/sylr/fix2 v0.0.0\nreplace github.com/sylr/fix2 => " + modRoot + "\n"
	if err := os.WriteFile(filepath.Join(outDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	// Create a main_test.go that exercises the generated code.
	testCode := `package fix44

import (
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
)

func TestRoundTrip(t *testing.T) {
	raw := BuildNewOrderSingle("FIX.4.4").
		SetSenderCompID("SENDER").
		SetTargetCompID("TARGET").
		SetMsgSeqNum(1).
		SetSendingTime(time.Date(2023, 10, 1, 12, 0, 0, 0, time.UTC)).
		SetClOrdID("ORD-001").
		SetSide(SideBuy).
		SetTransactTime(time.Date(2023, 10, 1, 12, 0, 0, 0, time.UTC)).
		SetOrdType(OrdTypeLimit).
		SetSymbol("AAPL").
		SetPrice(150.5).
		SetOrderQty(100).
		AddNoAllocsEntry(NewNoAllocsEntry().
			SetAllocAccount("ACC1").
			SetAllocQty(500).
			SetAllocPrice(149.75).
			SetIndividualAllocID("ALLOC-001").
			AddNoDlvyInstEntry(NewNoDlvyInstEntry().
				SetDlvyInst("INST-A1").
				SetDlvyInstCode("CODE-A1")).
			AddNoDlvyInstEntry(NewNoDlvyInstEntry().
				SetDlvyInst("INST-A2").
				SetDlvyInstCode("CODE-A2"))).
		AddNoAllocsEntry(NewNoAllocsEntry().
			SetAllocAccount("ACC2").
			SetAllocQty(300).
			SetAllocPrice(150.25).
			SetIndividualAllocID("ALLOC-002").
			AddNoDlvyInstEntry(NewNoDlvyInstEntry().
				SetDlvyInst("INST-B1").
				SetDlvyInstCode("CODE-B1"))).
		Build()

	msg, err := fix2.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	nos := WrapNewOrderSingle(&msg)
	if got, ok := nos.ClOrdID(); !ok || got != "ORD-001" {
		t.Errorf("ClOrdID = (%q, %v), want (%q, true)", got, ok, "ORD-001")
	}
	if got, ok := nos.Side(); !ok || got != SideBuy {
		t.Errorf("Side = (%q, %v), want (%q, true)", got, ok, SideBuy)
	}
	if got, ok := nos.Symbol(); !ok || got != "AAPL" {
		t.Errorf("Symbol = (%q, %v), want (%q, true)", got, ok, "AAPL")
	}
	if got, ok := nos.Price(); !ok || got != 150.5 {
		t.Errorf("Price = (%v, %v), want (%v, true)", got, ok, 150.5)
	}
	if got, ok := nos.OrderQty(); !ok || got != 100 {
		t.Errorf("OrderQty = (%v, %v), want (%v, true)", got, ok, 100.0)
	}
	if got, ok := nos.SenderCompID(); !ok || got != "SENDER" {
		t.Errorf("SenderCompID = (%q, %v), want (%q, true)", got, ok, "SENDER")
	}
	if got, ok := nos.MsgSeqNum(); !ok || got != 1 {
		t.Errorf("MsgSeqNum = (%d, %v), want (%d, true)", got, ok, 1)
	}

	// Repeating group via typed API.
	if got, ok := nos.NoAllocs(); !ok || got != 2 {
		t.Errorf("NoAllocs = (%d, %v), want (%d, true)", got, ok, 2)
	}

	entries := nos.NoAllocsEntries()
	if len(entries) != 2 {
		t.Fatalf("NoAllocsEntries len = %d, want 2", len(entries))
	}

	// First entry
	if got, ok := entries[0].AllocAccount(); !ok || got != "ACC1" {
		t.Errorf("entry[0].AllocAccount = (%q, %v), want (%q, true)", got, ok, "ACC1")
	}
	if got, ok := entries[0].AllocQty(); !ok || got != 500 {
		t.Errorf("entry[0].AllocQty = (%v, %v), want (%v, true)", got, ok, 500.0)
	}
	if got, ok := entries[0].AllocPrice(); !ok || got != 149.75 {
		t.Errorf("entry[0].AllocPrice = (%v, %v), want (%v, true)", got, ok, 149.75)
	}
	if got, ok := entries[0].IndividualAllocID(); !ok || got != "ALLOC-001" {
		t.Errorf("entry[0].IndividualAllocID = (%q, %v), want (%q, true)", got, ok, "ALLOC-001")
	}

	// Nested group: first alloc entry has 2 delivery instructions.
	if got, ok := entries[0].NoDlvyInst(); !ok || got != 2 {
		t.Errorf("entry[0].NoDlvyInst = (%d, %v), want (%d, true)", got, ok, 2)
	}
	dlvy0 := entries[0].NoDlvyInstEntries()
	if len(dlvy0) != 2 {
		t.Fatalf("entry[0].NoDlvyInstEntries len = %d, want 2", len(dlvy0))
	}
	if got, ok := dlvy0[0].DlvyInst(); !ok || got != "INST-A1" {
		t.Errorf("dlvy0[0].DlvyInst = (%q, %v), want (%q, true)", got, ok, "INST-A1")
	}
	if got, ok := dlvy0[0].DlvyInstCode(); !ok || got != "CODE-A1" {
		t.Errorf("dlvy0[0].DlvyInstCode = (%q, %v), want (%q, true)", got, ok, "CODE-A1")
	}
	if got, ok := dlvy0[1].DlvyInst(); !ok || got != "INST-A2" {
		t.Errorf("dlvy0[1].DlvyInst = (%q, %v), want (%q, true)", got, ok, "INST-A2")
	}
	if got, ok := dlvy0[1].DlvyInstCode(); !ok || got != "CODE-A2" {
		t.Errorf("dlvy0[1].DlvyInstCode = (%q, %v), want (%q, true)", got, ok, "CODE-A2")
	}

	// Second entry
	if got, ok := entries[1].AllocAccount(); !ok || got != "ACC2" {
		t.Errorf("entry[1].AllocAccount = (%q, %v), want (%q, true)", got, ok, "ACC2")
	}
	if got, ok := entries[1].AllocQty(); !ok || got != 300 {
		t.Errorf("entry[1].AllocQty = (%v, %v), want (%v, true)", got, ok, 300.0)
	}
	if got, ok := entries[1].AllocPrice(); !ok || got != 150.25 {
		t.Errorf("entry[1].AllocPrice = (%v, %v), want (%v, true)", got, ok, 150.25)
	}
	if got, ok := entries[1].IndividualAllocID(); !ok || got != "ALLOC-002" {
		t.Errorf("entry[1].IndividualAllocID = (%q, %v), want (%q, true)", got, ok, "ALLOC-002")
	}

	// Nested group: second alloc entry has 1 delivery instruction.
	if got, ok := entries[1].NoDlvyInst(); !ok || got != 1 {
		t.Errorf("entry[1].NoDlvyInst = (%d, %v), want (%d, true)", got, ok, 1)
	}
	dlvy1 := entries[1].NoDlvyInstEntries()
	if len(dlvy1) != 1 {
		t.Fatalf("entry[1].NoDlvyInstEntries len = %d, want 1", len(dlvy1))
	}
	if got, ok := dlvy1[0].DlvyInst(); !ok || got != "INST-B1" {
		t.Errorf("dlvy1[0].DlvyInst = (%q, %v), want (%q, true)", got, ok, "INST-B1")
	}
	if got, ok := dlvy1[0].DlvyInstCode(); !ok || got != "CODE-B1" {
		t.Errorf("dlvy1[0].DlvyInstCode = (%q, %v), want (%q, true)", got, ok, "CODE-B1")
	}

	tt, ok, err := nos.TransactTime()
	if err != nil {
		t.Fatalf("TransactTime: %v", err)
	}
	if !ok {
		t.Fatal("TransactTime ok = false, want true")
	}
	if tt.Year() != 2023 {
		t.Errorf("TransactTime year = %d, want 2023", tt.Year())
	}
}

func TestHeartbeat(t *testing.T) {
	raw := BuildHeartbeat("FIX.4.4").
		SetSenderCompID("S").
		SetTargetCompID("T").
		SetMsgSeqNum(1).
		SetSendingTime(time.Date(2023, 10, 1, 12, 0, 0, 0, time.UTC)).
		SetTestReqID("TEST123").
		Build()

	msg, err := fix2.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	hb := WrapHeartbeat(&msg)
	if got, ok := hb.TestReqID(); !ok || got != "TEST123" {
		t.Errorf("TestReqID = (%q, %v), want (%q, true)", got, ok, "TEST123")
	}
}

func TestEnumConstants(t *testing.T) {
	if SideBuy != "1" {
		t.Errorf("SideBuy = %q, want %q", SideBuy, "1")
	}
	if SideSell != "2" {
		t.Errorf("SideSell = %q, want %q", SideSell, "2")
	}
	if OrdTypeMarket != "1" {
		t.Errorf("OrdTypeMarket = %q, want %q", OrdTypeMarket, "1")
	}
	if OrdTypeLimit != "2" {
		t.Errorf("OrdTypeLimit = %q, want %q", OrdTypeLimit, "2")
	}
}
`
	if err := os.WriteFile(filepath.Join(outDir, "round_trip_test.go"), []byte(testCode), 0o644); err != nil {
		t.Fatalf("write test: %v", err)
	}

	// Run go test in the generated package.
	cmd := exec.Command("go", "test", "-v", "./...")
	cmd.Dir = outDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go test failed:\n%s\n%v", out, err)
	}
	t.Logf("generated package test output:\n%s", out)
}

func TestSanitizePascal(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"BUY", "Buy"},
		{"SELL", "Sell"},
		{"MARKET", "Market"},
		{"LIMIT", "Limit"},
		{"STOP_LIMIT", "StopLimit"},
		{"NOT HELD", "NotHeld"},
	}
	for _, tt := range tests {
		got := sanitizePascal(tt.in)
		if got != tt.want {
			t.Errorf("sanitizePascal(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestToSnakeCase(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"NewOrderSingle", "new_order_single"},
		{"Heartbeat", "heartbeat"},
	}
	for _, tt := range tests {
		got := toSnakeCase(tt.in)
		if got != tt.want {
			t.Errorf("toSnakeCase(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLookupType(t *testing.T) {
	tests := []struct {
		fixType  string
		goType   string
		category string
	}{
		{"STRING", "string", "string"},
		{"INT", "int", "int"},
		{"PRICE", "float64", "float"},
		{"BOOLEAN", "bool", "bool"},
		{"UTCTIMESTAMP", "time.Time", "time"},
		{"DATA", "[]byte", "bytes"},
		{"UNKNOWN", "string", "string"},
	}
	for _, tt := range tests {
		info := lookupType(tt.fixType)
		if info.GoType != tt.goType {
			t.Errorf("lookupType(%q).GoType = %q, want %q", tt.fixType, info.GoType, tt.goType)
		}
		if info.Category != tt.category {
			t.Errorf("lookupType(%q).Category = %q, want %q", tt.fixType, info.Category, tt.category)
		}
	}
}

func TestGeneratedFileContents(t *testing.T) {
	dict, err := dictionary.LoadFile("testdata/FIX44.xml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	outDir := t.TempDir()
	if err := generate(dict, "fix44", outDir); err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Check doc.go has DO NOT EDIT header.
	doc, err := os.ReadFile(filepath.Join(outDir, "doc.go"))
	if err != nil {
		t.Fatalf("read doc.go: %v", err)
	}
	if !strings.Contains(string(doc), "DO NOT EDIT") {
		t.Error("doc.go missing DO NOT EDIT header")
	}

	// Check fields.go has tag constants.
	fields, err := os.ReadFile(filepath.Join(outDir, "fields.go"))
	if err != nil {
		t.Fatalf("read fields.go: %v", err)
	}
	fieldsStr := string(fields)
	if !strings.Contains(fieldsStr, "TagSide") {
		t.Error("fields.go missing TagSide constant")
	}
	if !strings.Contains(fieldsStr, "SideBuy") {
		t.Error("fields.go missing SideBuy enum constant")
	}
	if strings.Contains(fieldsStr, "parseUTCTimestamp") {
		t.Error("fields.go should not contain local parseUTCTimestamp helper (now in fix2 package)")
	}

	// Verify nested group fields are not on the base message.
	nos, err := os.ReadFile(filepath.Join(outDir, "msg_new_order_single.go"))
	if err != nil {
		t.Fatalf("read msg_new_order_single.go: %v", err)
	}
	nosStr := string(nos)

	// NoDlvyInst counter getter should be on NoAllocsEntry, not on NewOrderSingle.
	if strings.Contains(nosStr, "func (m *NewOrderSingle) NoDlvyInst()") {
		t.Error("NoDlvyInst() getter should not exist on NewOrderSingle (nested group counter)")
	}
	if !strings.Contains(nosStr, "func (e *NoAllocsEntry) NoDlvyInst()") {
		t.Error("NoDlvyInst() getter missing on NoAllocsEntry")
	}

	// DlvyInst getter should be on NoDlvyInstEntry, not on NewOrderSingle.
	if strings.Contains(nosStr, "func (m *NewOrderSingle) DlvyInst()") {
		t.Error("DlvyInst() getter should not exist on NewOrderSingle (nested group child)")
	}
	if !strings.Contains(nosStr, "func (e *NoDlvyInstEntry) DlvyInst()") {
		t.Error("DlvyInst() getter missing on NoDlvyInstEntry")
	}

	// NoDlvyInstEntries iteration should be on NoAllocsEntry.
	if !strings.Contains(nosStr, "func (e *NoAllocsEntry) NoDlvyInstEntries()") {
		t.Error("NoDlvyInstEntries() missing on NoAllocsEntry")
	}

	// AddNoDlvyInstEntry should be on NoAllocsEntryBuilder.
	if !strings.Contains(nosStr, "func (e *NoAllocsEntryBuilder) AddNoDlvyInstEntry(") {
		t.Error("AddNoDlvyInstEntry() missing on NoAllocsEntryBuilder")
	}

	// No setter for NoDlvyInst or DlvyInst on the message builder.
	if strings.Contains(nosStr, "func (b *NewOrderSingleBuilder) SetNoDlvyInst(") {
		t.Error("SetNoDlvyInst() should not exist on NewOrderSingleBuilder")
	}
	if strings.Contains(nosStr, "func (b *NewOrderSingleBuilder) SetDlvyInst(") {
		t.Error("SetDlvyInst() should not exist on NewOrderSingleBuilder")
	}
}
