package main

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		"group_no_allocs.go",
		"group_no_dlvy_inst.go",
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
	if got, ok, err := nos.Price(); err != nil || !ok || got != 150.5 {
		t.Errorf("Price = (%v, %v, %v), want (%v, true, nil)", got, ok, err, 150.5)
	}
	if got, ok, err := nos.OrderQty(); err != nil || !ok || got != 100 {
		t.Errorf("OrderQty = (%v, %v, %v), want (%v, true, nil)", got, ok, err, 100.0)
	}
	if got, ok := nos.SenderCompID(); !ok || got != "SENDER" {
		t.Errorf("SenderCompID = (%q, %v), want (%q, true)", got, ok, "SENDER")
	}
	if got, ok, err := nos.MsgSeqNum(); err != nil || !ok || got != 1 {
		t.Errorf("MsgSeqNum = (%d, %v, %v), want (%d, true, nil)", got, ok, err, 1)
	}

	// Repeating group via typed API.
	if got, ok, err := nos.NoAllocs(); err != nil || !ok || got != 2 {
		t.Errorf("NoAllocs = (%d, %v, %v), want (%d, true, nil)", got, ok, err, 2)
	}

	entries := nos.NoAllocsEntries()
	if len(entries) != 2 {
		t.Fatalf("NoAllocsEntries len = %d, want 2", len(entries))
	}

	// First entry
	if got, ok := entries[0].AllocAccount(); !ok || got != "ACC1" {
		t.Errorf("entry[0].AllocAccount = (%q, %v), want (%q, true)", got, ok, "ACC1")
	}
	if got, ok, err := entries[0].AllocQty(); err != nil || !ok || got != 500 {
		t.Errorf("entry[0].AllocQty = (%v, %v, %v), want (%v, true, nil)", got, ok, err, 500.0)
	}
	if got, ok, err := entries[0].AllocPrice(); err != nil || !ok || got != 149.75 {
		t.Errorf("entry[0].AllocPrice = (%v, %v, %v), want (%v, true, nil)", got, ok, err, 149.75)
	}
	if got, ok := entries[0].IndividualAllocID(); !ok || got != "ALLOC-001" {
		t.Errorf("entry[0].IndividualAllocID = (%q, %v), want (%q, true)", got, ok, "ALLOC-001")
	}

	// Nested group: first alloc entry has 2 delivery instructions.
	if got, ok, err := entries[0].NoDlvyInst(); err != nil || !ok || got != 2 {
		t.Errorf("entry[0].NoDlvyInst = (%d, %v, %v), want (%d, true, nil)", got, ok, err, 2)
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
	if got, ok, err := entries[1].AllocQty(); err != nil || !ok || got != 300 {
		t.Errorf("entry[1].AllocQty = (%v, %v, %v), want (%v, true, nil)", got, ok, err, 300.0)
	}
	if got, ok, err := entries[1].AllocPrice(); err != nil || !ok || got != 150.25 {
		t.Errorf("entry[1].AllocPrice = (%v, %v, %v), want (%v, true, nil)", got, ok, err, 150.25)
	}
	if got, ok := entries[1].IndividualAllocID(); !ok || got != "ALLOC-002" {
		t.Errorf("entry[1].IndividualAllocID = (%q, %v), want (%q, true)", got, ok, "ALLOC-002")
	}

	// Nested group: second alloc entry has 1 delivery instruction.
	if got, ok, err := entries[1].NoDlvyInst(); err != nil || !ok || got != 1 {
		t.Errorf("entry[1].NoDlvyInst = (%d, %v, %v), want (%d, true, nil)", got, ok, err, 1)
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
		{"TIME", "time.Time", "time"},
		{"DATE", "time.Time", "time"},
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

	// Group entry types should NOT be in the message file (they're in group_*.go now).
	if strings.Contains(nosStr, "type NoAllocsEntry struct") {
		t.Error("NoAllocsEntry struct should not be in msg_new_order_single.go (moved to group file)")
	}
	if strings.Contains(nosStr, "type NoAllocsEntryBuilder struct") {
		t.Error("NoAllocsEntryBuilder struct should not be in msg_new_order_single.go (moved to group file)")
	}

	// NoDlvyInst counter getter should not exist on NewOrderSingle.
	if strings.Contains(nosStr, "func (m *NewOrderSingle) NoDlvyInst()") {
		t.Error("NoDlvyInst() getter should not exist on NewOrderSingle (nested group counter)")
	}
	if strings.Contains(nosStr, "func (m *NewOrderSingle) DlvyInst()") {
		t.Error("DlvyInst() getter should not exist on NewOrderSingle (nested group child)")
	}

	// No setter for NoDlvyInst or DlvyInst on the message builder.
	if strings.Contains(nosStr, "func (b *NewOrderSingleBuilder) SetNoDlvyInst(") {
		t.Error("SetNoDlvyInst() should not exist on NewOrderSingleBuilder")
	}
	if strings.Contains(nosStr, "func (b *NewOrderSingleBuilder) SetDlvyInst(") {
		t.Error("SetDlvyInst() should not exist on NewOrderSingleBuilder")
	}

	// Message file should have iteration method and Add method.
	if !strings.Contains(nosStr, "func (m *NewOrderSingle) NoAllocsEntries()") {
		t.Error("NoAllocsEntries() missing on NewOrderSingle")
	}
	if !strings.Contains(nosStr, "func (b *NewOrderSingleBuilder) AddNoAllocsEntry(") {
		t.Error("AddNoAllocsEntry() missing on NewOrderSingleBuilder")
	}

	// Verify group files contain the entry types.
	allocsGrp, err := os.ReadFile(filepath.Join(outDir, "group_no_allocs.go"))
	if err != nil {
		t.Fatalf("read group_no_allocs.go: %v", err)
	}
	allocsStr := string(allocsGrp)

	if !strings.Contains(allocsStr, "type NoAllocsEntry struct") {
		t.Error("NoAllocsEntry struct missing from group_no_allocs.go")
	}
	if !strings.Contains(allocsStr, "type NoAllocsEntryBuilder struct") {
		t.Error("NoAllocsEntryBuilder struct missing from group_no_allocs.go")
	}
	if !strings.Contains(allocsStr, "func (e *NoAllocsEntry) NoDlvyInst()") {
		t.Error("NoDlvyInst() getter missing on NoAllocsEntry in group file")
	}
	if !strings.Contains(allocsStr, "func (e *NoAllocsEntry) NoDlvyInstEntries()") {
		t.Error("NoDlvyInstEntries() missing on NoAllocsEntry in group file")
	}
	if !strings.Contains(allocsStr, "func (e *NoAllocsEntryBuilder) AddNoDlvyInstEntry(") {
		t.Error("AddNoDlvyInstEntry() missing on NoAllocsEntryBuilder in group file")
	}

	dlvyGrp, err := os.ReadFile(filepath.Join(outDir, "group_no_dlvy_inst.go"))
	if err != nil {
		t.Fatalf("read group_no_dlvy_inst.go: %v", err)
	}
	dlvyStr := string(dlvyGrp)

	if !strings.Contains(dlvyStr, "type NoDlvyInstEntry struct") {
		t.Error("NoDlvyInstEntry struct missing from group_no_dlvy_inst.go")
	}
	if !strings.Contains(dlvyStr, "func (e *NoDlvyInstEntry) DlvyInst()") {
		t.Error("DlvyInst() getter missing on NoDlvyInstEntry in group file")
	}
}

func TestTimeParseAndFormat(t *testing.T) {
	tests := []struct {
		fixType   string
		parseFunc string
		fmtConst  string
	}{
		{"UTCTIMESTAMP", "ParseUTCTimestamp", "TimeFormatTimestamp"},
		{"TIME", "ParseUTCTimestamp", "TimeFormatTimestampSec"},
		{"UTCDATE", "ParseUTCDate", "TimeFormatDate"},
		{"LOCALMKTDATE", "ParseUTCDate", "TimeFormatDate"},
		{"DATE", "ParseUTCDate", "TimeFormatDate"},
		{"UTCTIMEONLY", "ParseUTCTimeOnly", "TimeFormatTimeOnly"},
	}
	for _, tt := range tests {
		if got := timeParseFunc(tt.fixType); got != tt.parseFunc {
			t.Errorf("timeParseFunc(%q) = %q, want %q", tt.fixType, got, tt.parseFunc)
		}
		if got := timeFormatConst(tt.fixType); got != tt.fmtConst {
			t.Errorf("timeFormatConst(%q) = %q, want %q", tt.fixType, got, tt.fmtConst)
		}
	}
}

func TestGenerateRemovesStaleFiles(t *testing.T) {
	dict, err := dictionary.LoadFile("testdata/FIX44.xml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	outDir := t.TempDir()
	header := "// " + generatedHeader + "\n\npackage fix44\n"
	files := map[string]string{
		"group_no_old.go":  header,                          // stale generated file: removed
		"msg_old.go":       header,                          // stale generated file: removed
		"generate.go":      "package fix44\n",               // hand-written: kept
		"x.go":             "//",                            // shorter than the header: kept
		"notes.txt":        header,                          // not a .go file: kept
		"sub/msg_old.go":   header,                          // in a subdirectory: kept
		"msg_heartbeat.go": "// stale content, rewritten\n", // emitted again: rewritten
	}
	for name, content := range files {
		path := filepath.Join(outDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := generate(dict, "fix44", outDir); err != nil {
		t.Fatalf("generate: %v", err)
	}

	for _, name := range []string{"group_no_old.go", "msg_old.go"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); !os.IsNotExist(err) {
			t.Errorf("stale %s still exists (err=%v)", name, err)
		}
	}
	for _, name := range []string{"generate.go", "x.go", "notes.txt", "sub/msg_old.go", "doc.go", "group_no_allocs.go", "msg_new_order_single.go"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Errorf("%s should exist: %v", name, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(outDir, "msg_heartbeat.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "// "+generatedHeader) || !strings.Contains(string(got), "func BuildHeartbeat") {
		t.Errorf("msg_heartbeat.go was not regenerated:\n%s", got)
	}
}

func TestTimeValue(t *testing.T) {
	for fixType, want := range map[string]string{
		"LOCALMKTDATE": "v",
		"DATE":         "v",
		"UTCTIMESTAMP": "v.UTC()",
		"TIME":         "v.UTC()",
		"UTCDATE":      "v.UTC()",
		"UTCTIMEONLY":  "v.UTC()",
	} {
		if got := fmt.Sprintf("%#v", timeValue(fixType)); got != want {
			t.Errorf("timeValue(%q) = %s, want %s", fixType, got, want)
		}
	}
}

func TestGeneratedGroupSpecs(t *testing.T) {
	dict, err := dictionary.LoadFile("testdata/FIX44.xml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	outDir := t.TempDir()
	if err := generate(dict, "fix44", outDir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// The message declares the spec tree of its own NoAllocs group, nesting
	// NoDlvyInst inline; entries look nested specs up in their context.
	nos := read("msg_new_order_single.go")
	for _, want := range []string{
		"var newOrderSingleNoAllocsSpec = &fix2.GroupSpec{",
		"Counter: TagNoAllocs,",
		"Delim:   TagAllocAccount,",
		"Counter: TagNoDlvyInst,",
		"spec := newOrderSingleNoAllocsSpec",
		"m.msg.GroupsOf(spec)",
	} {
		if !strings.Contains(nos, want) {
			t.Errorf("msg_new_order_single.go missing %q", want)
		}
	}
	allocs := read("group_no_allocs.go")
	if !strings.Contains(allocs, "spec := e.spec.Child(TagNoDlvyInst)") {
		t.Error("NoAllocsEntry does not look up NoDlvyInst in its own spec")
	}
	if strings.Contains(allocs, "GroupSpec{") {
		t.Error("group files should not declare shared specs")
	}
}

// runGenerated generates package pkg from dictionary xml in a temporary
// module, adds testCode as a test file and runs go test there.
func runGenerated(t *testing.T, xml, pkg, testCode string) {
	t.Helper()
	dict, err := dictionary.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	outDir := t.TempDir()
	if err := generate(dict, pkg, outDir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	modRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	goMod := "module testgen\ngo 1.26\nrequire github.com/sylr/fix2 v0.0.0\nreplace github.com/sylr/fix2 => " + modRoot + "\n"
	if err := os.WriteFile(filepath.Join(outDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "gen_test.go"), []byte(testCode), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = outDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test failed:\n%s\n%v", out, err)
	}
}

// TestGenerateDelimiterIsNestedCounter covers a group whose entries start
// with a nested group, as FIX 5.0 SP2 NoAsgnReqs starts with Parties: the
// delimiter is the nested NoPartyIDs counter.
func TestGenerateDelimiterIsNestedCounter(t *testing.T) {
	const xml = `<fix type="FIX" major="5" minor="0" servicepack="2">
  <header/><trailer/>
  <messages>
    <message name="StreamAssignmentRequest" msgtype="CC" msgcat="app">
      <field name="StreamAsgnReqID" required="Y"/>
      <group name="NoAsgnReqs" required="Y">
        <component name="Parties" required="N"/>
        <field name="MDReqID" required="N"/>
      </group>
    </message>
  </messages>
  <components>
    <component name="Parties">
      <group name="NoPartyIDs" required="N">
        <field name="PartyID" required="N"/>
        <field name="PartyRole" required="N"/>
      </group>
    </component>
  </components>
  <fields>
    <field number="1497" name="StreamAsgnReqID" type="STRING"/>
    <field number="1499" name="NoAsgnReqs" type="NUMINGROUP"/>
    <field number="453" name="NoPartyIDs" type="NUMINGROUP"/>
    <field number="448" name="PartyID" type="STRING"/>
    <field number="452" name="PartyRole" type="INT"/>
    <field number="262" name="MDReqID" type="STRING"/>
  </fields>
</fix>`
	runGenerated(t, xml, "sar", `package sar

import (
	"testing"

	fix2 "github.com/sylr/fix2"
)

func TestRoundTrip(t *testing.T) {
	raw := BuildStreamAssignmentRequest("FIXT.1.1").
		SetStreamAsgnReqID("R").
		AddNoAsgnReqsEntry(NewNoAsgnReqsEntry().
			SetMDReqID("A").
			AddNoPartyIDsEntry(NewNoPartyIDsEntry().SetPartyID("P1").SetPartyRole(1))).
		AddNoAsgnReqsEntry(NewNoAsgnReqsEntry().
			AddNoPartyIDsEntry(NewNoPartyIDsEntry().SetPartyID("P2")).
			AddNoPartyIDsEntry(NewNoPartyIDsEntry().SetPartyID("P3")).
			SetMDReqID("B")).
		Build()
	msg, err := fix2.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	reqs := WrapStreamAssignmentRequest(&msg).NoAsgnReqsEntries()
	if len(reqs) != 2 {
		t.Fatalf("%q: %d entries, want 2", raw, len(reqs))
	}
	for i, want := range []struct {
		md      string
		parties []string
	}{{"A", []string{"P1"}}, {"B", []string{"P2", "P3"}}} {
		if md, _ := reqs[i].MDReqID(); md != want.md {
			t.Errorf("entry %d MDReqID = %q, want %q", i, md, want.md)
		}
		parties := reqs[i].NoPartyIDsEntries()
		if len(parties) != len(want.parties) {
			t.Fatalf("%q: entry %d has %d parties, want %d", raw, i, len(parties), len(want.parties))
		}
		for j, id := range want.parties {
			if got, _ := parties[j].PartyID(); got != id {
				t.Errorf("entry %d party %d = %q, want %q", i, j, got, id)
			}
		}
	}
}

func TestEntryWithoutParties(t *testing.T) {
	// The NoPartyIDs counter is the delimiter: it is written as 0 rather
	// than left out, or the entry could not be read back.
	raw := BuildStreamAssignmentRequest("FIXT.1.1").
		SetStreamAsgnReqID("R").
		AddNoAsgnReqsEntry(NewNoAsgnReqsEntry().SetMDReqID("C")).
		Build()
	msg, err := fix2.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	reqs := WrapStreamAssignmentRequest(&msg).NoAsgnReqsEntries()
	if len(reqs) != 1 {
		t.Fatalf("%q: %d entries, want 1", raw, len(reqs))
	}
	if md, _ := reqs[0].MDReqID(); md != "C" {
		t.Errorf("MDReqID = %q, want C", md)
	}
}
`)
}

// TestGenerateGroupDefinedDifferentlyPerMessage covers a group whose fields
// differ between messages: message B's Child group also holds tag 100, which
// in message A is the delimiter of Parent, the group around Child. A's
// entries must be split with A's own definitions.
func TestGenerateGroupDefinedDifferentlyPerMessage(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages>
    <message name="A" msgtype="U1" msgcat="app">
      <group name="NoParent" required="N">
        <field name="P" required="N"/>
        <group name="NoChild" required="N"><field name="C" required="N"/></group>
      </group>
    </message>
    <message name="B" msgtype="U2" msgcat="app">
      <group name="NoChild" required="N"><field name="C" required="N"/><field name="P" required="N"/></group>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="900" name="NoParent" type="NUMINGROUP"/>
    <field number="100" name="P" type="STRING"/>
    <field number="200" name="NoChild" type="NUMINGROUP"/>
    <field number="201" name="C" type="STRING"/>
  </fields>
</fix>`
	runGenerated(t, xml, "perctx", `package perctx

import (
	"testing"

	fix2 "github.com/sylr/fix2"
)

func TestPerMessageSpec(t *testing.T) {
	// 900=2|100=P1|200=1|201=C|100=P2|: in A, 100 starts the second parent
	// entry; the merged NoChild (which allows 100) must not swallow it.
	raw := fix2.NewBuilder("FIX.4.4", "U1").
		SetInt(900, 2).
		AppendString(100, "P1").AppendInt(200, 1).AppendString(201, "C").
		AppendString(100, "P2").
		Build()
	msg, err := fix2.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	parents := WrapA(&msg).NoParentEntries()
	if len(parents) != 2 {
		t.Fatalf("%q: %d parent entries, want 2", raw, len(parents))
	}
	if p, _ := parents[1].P(); p != "P2" {
		t.Errorf("second parent P = %q, want P2", p)
	}
	if n := len(parents[0].NoChildEntries()); n != 1 {
		t.Errorf("first parent has %d children, want 1", n)
	}
}
`)
}

func TestMergeLayouts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		layouts [][]int
		want    []int
	}{
		{"identical", [][]int{{1, 2, 3}, {1, 2, 3}}, []int{1, 2, 3}},
		// B (5) only in the second layout goes where that layout puts it.
		{"field only in a later layout", [][]int{{1, 2, 9}, {1, 5, 2, 9}}, []int{1, 5, 2, 9}},
		{"subset", [][]int{{1, 2}, {1, 2, 3}}, []int{1, 2, 3}},
		// Never ahead of the delimiter, even without a known predecessor.
		{"no known predecessor", [][]int{{1, 2}, {7, 2}}, []int{1, 7, 2}},
		// A later layout orders tags an earlier one placed independently.
		{"compatible constraints", [][]int{{1, 2, 9}, {1, 3, 9}, {1, 2, 3, 9}}, []int{1, 2, 3, 9}},
		// Conflicting orders: the first layout wins.
		{"conflict", [][]int{{1, 2, 3}, {1, 3, 2}}, []int{1, 2, 3}},
	} {
		if got := mergeLayouts(tc.layouts); !slices.Equal(got, tc.want) {
			t.Errorf("%s: mergeLayouts = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestGenerateDeterministic generates the same dictionary twice: the output
// must not depend on map iteration order.
func TestGenerateDeterministic(t *testing.T) {
	dict, err := dictionary.LoadFile("testdata/FIX50SP2.xml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	var outputs [2]map[string]string
	for i := range outputs {
		dir := t.TempDir()
		if err := generate(dict, "fix50sp2", dir); err != nil {
			t.Fatalf("generate: %v", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		outputs[i] = map[string]string{}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			outputs[i][e.Name()] = string(b)
		}
	}
	if !maps.Equal(outputs[0], outputs[1]) {
		t.Error("two generations of the same dictionary differ")
	}
}

func TestPairDataLengths(t *testing.T) {
	dict, err := dictionary.LoadFile("testdata/FIX50SP2.xml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	// EncodedLegIssuerLen (618) / EncodedLegIssuer (619), with another field
	// (600) between them or the length missing entirely.
	for _, tc := range []struct{ in, want []int }{
		{[]int{1, 618, 600, 619}, []int{1, 600, 618, 619}},
		{[]int{1, 619, 600}, []int{1, 618, 619, 600}},
		{[]int{1, 618, 619}, []int{1, 618, 619}},
	} {
		if got := pairDataLengths(dict, tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("pairDataLengths(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
