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
			SetIndividualAllocID("ALLOC-001")).
		AddNoAllocsEntry(NewNoAllocsEntry().
			SetAllocAccount("ACC2").
			SetAllocQty(300).
			SetAllocPrice(150.25).
			SetIndividualAllocID("ALLOC-002")).
		Build()

	msg, err := fix2.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	nos := WrapNewOrderSingle(&msg)
	if got := nos.ClOrdID(); got != "ORD-001" {
		t.Errorf("ClOrdID = %q, want %q", got, "ORD-001")
	}
	if got := nos.Side(); got != SideBuy {
		t.Errorf("Side = %q, want %q", got, SideBuy)
	}
	if got := nos.Symbol(); got != "AAPL" {
		t.Errorf("Symbol = %q, want %q", got, "AAPL")
	}
	if got := nos.Price(); got != 150.5 {
		t.Errorf("Price = %v, want %v", got, 150.5)
	}
	if got := nos.OrderQty(); got != 100 {
		t.Errorf("OrderQty = %v, want %v", got, 100.0)
	}
	if got := nos.SenderCompID(); got != "SENDER" {
		t.Errorf("SenderCompID = %q, want %q", got, "SENDER")
	}
	if got := nos.MsgSeqNum(); got != 1 {
		t.Errorf("MsgSeqNum = %d, want %d", got, 1)
	}
	if !nos.HasClOrdID() {
		t.Error("HasClOrdID should be true")
	}

	// Repeating group via typed API.
	if got := nos.NoAllocs(); got != 2 {
		t.Errorf("NoAllocs = %d, want %d", got, 2)
	}
	if !nos.HasNoAllocs() {
		t.Error("HasNoAllocs should be true")
	}

	entries := nos.NoAllocsEntries()
	if len(entries) != 2 {
		t.Fatalf("NoAllocsEntries len = %d, want 2", len(entries))
	}

	// First entry
	if got := entries[0].AllocAccount(); got != "ACC1" {
		t.Errorf("entry[0].AllocAccount = %q, want %q", got, "ACC1")
	}
	if got := entries[0].AllocQty(); got != 500 {
		t.Errorf("entry[0].AllocQty = %v, want %v", got, 500.0)
	}
	if got := entries[0].AllocPrice(); got != 149.75 {
		t.Errorf("entry[0].AllocPrice = %v, want %v", got, 149.75)
	}
	if got := entries[0].IndividualAllocID(); got != "ALLOC-001" {
		t.Errorf("entry[0].IndividualAllocID = %q, want %q", got, "ALLOC-001")
	}
	if !entries[0].HasAllocAccount() {
		t.Error("entry[0].HasAllocAccount should be true")
	}

	// Second entry
	if got := entries[1].AllocAccount(); got != "ACC2" {
		t.Errorf("entry[1].AllocAccount = %q, want %q", got, "ACC2")
	}
	if got := entries[1].AllocQty(); got != 300 {
		t.Errorf("entry[1].AllocQty = %v, want %v", got, 300.0)
	}
	if got := entries[1].AllocPrice(); got != 150.25 {
		t.Errorf("entry[1].AllocPrice = %v, want %v", got, 150.25)
	}
	if got := entries[1].IndividualAllocID(); got != "ALLOC-002" {
		t.Errorf("entry[1].IndividualAllocID = %q, want %q", got, "ALLOC-002")
	}

	tt, err := nos.TransactTime()
	if err != nil {
		t.Fatalf("TransactTime: %v", err)
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
	if got := hb.TestReqID(); got != "TEST123" {
		t.Errorf("TestReqID = %q, want %q", got, "TEST123")
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
	if !strings.Contains(fieldsStr, "parseUTCTimestamp") {
		t.Error("fields.go missing parseUTCTimestamp helper")
	}
}
