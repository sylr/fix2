package tagvalue

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/dictionary/xmldict"
)

func TestGenerate(t *testing.T) {
	dict, err := xmldict.LoadFile("../../../fix/dictionary/testdata/FIX44.xml")
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
	writeTestModule(t, outDir)

	// Create a main_test.go that exercises the generated code.
	testCode := `package fix44

import (
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
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

	msg, err := fix.Parse(raw)
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

	msg, err := fix.Parse(raw)
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
	cmd := exec.Command("go", "test", "-mod=mod", "-v", "./...")
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
	dict, err := xmldict.LoadFile("../../../fix/dictionary/testdata/FIX44.xml")
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
	dict, err := xmldict.LoadFile("../../../fix/dictionary/testdata/FIX44.xml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	outDir := t.TempDir()
	header := "// " + generatedHeader + "\n\npackage fix44\n"
	files := map[string]string{
		"group_no_old.go":  header,                                        // stale generated file: removed
		"msg_old.go":       header,                                        // stale generated file: removed
		"msg_fixgen.go":    "// Code generated by fixgen; DO NOT EDIT.\n", // generated by fixgen: removed
		"generate.go":      "package fix44\n",                             // hand-written: kept
		"x.go":             "//",                                          // shorter than the header: kept
		"notes.txt":        header,                                        // not a .go file: kept
		"sub/msg_old.go":   header,                                        // in a subdirectory: kept
		"msg_heartbeat.go": "// stale content, rewritten\n",               // emitted again: rewritten
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

	for _, name := range []string{"group_no_old.go", "msg_old.go", "msg_fixgen.go"} {
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
	dict, err := xmldict.LoadFile("../../../fix/dictionary/testdata/FIX44.xml")
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
		"var newOrderSingle_NoAllocs_spec = &fix.GroupSpec{",
		"Counter: TagNoAllocs,",
		"Delim:   TagAllocAccount,",
		"Counter: TagNoDlvyInst,",
		"spec := newOrderSingle_NoAllocs_spec",
		"fix.EntriesOf(m.msg.Fields(), spec, newOrderSingle_specs, ",
		"var newOrderSingle_specs = []*fix.GroupSpec{",
	} {
		if !strings.Contains(nos, want) {
			t.Errorf("msg_new_order_single.go missing %q", want)
		}
	}
	allocs := read("group_no_allocs.go")
	if !strings.Contains(allocs, "spec := e.spec.Child(TagNoDlvyInst)") {
		t.Error("NoAllocsEntry does not look up NoDlvyInst in its own spec")
	}
	for _, want := range []string{"fix.EntriesOf(e.grp.Fields(), spec, e.spec.Nested, ", "e.grp.GetTopLevel(tag, e.spec.Nested...)"} {
		if !strings.Contains(allocs, want) {
			t.Errorf("group_no_allocs.go missing %q", want)
		}
	}
	if strings.Contains(allocs, "GroupSpec{") {
		t.Error("group files should not declare shared specs")
	}
}

// writeTestModule makes dir the module testgen, which builds against the
// fix2 of the repository. What testgen imports of fix2 may need fix2's
// dependencies, such as golang.org/x/sys on Linux, which testgen must then
// require and have the sums of: fix2's go.sum goes with it, and go test runs
// with -mod=mod, which adds the requirements at the versions fix2's go.mod
// already selects.
func writeTestModule(t *testing.T, dir string) {
	t.Helper()
	modRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	goMod := "module testgen\ngo 1.27\nrequire github.com/sylr/fix2 v0.0.0\nreplace github.com/sylr/fix2 => " + modRoot + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	goSum, err := os.ReadFile(filepath.Join(modRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), goSum, 0o644); err != nil {
		t.Fatal(err)
	}
}

// runGenerated generates package pkg from dictionary xml in a temporary
// module, adds testCode as a test file and runs go test there.
func runGenerated(t *testing.T, xml, pkg, testCode string) {
	t.Helper()
	runGeneratedWith(t, xml, pkg, testCode, options{})
}

func runGeneratedWith(t *testing.T, xml, pkg, testCode string, opts options) {
	t.Helper()
	dict, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	outDir := t.TempDir()
	if err := generateWith(dict, pkg, outDir, opts); err != nil {
		t.Fatalf("generate: %v", err)
	}
	writeTestModule(t, outDir)
	if err := os.WriteFile(filepath.Join(outDir, "gen_test.go"), []byte(testCode), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-mod=mod", "./...")
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

	"github.com/sylr/fix2/fix"
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
	msg, err := fix.Parse(raw)
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
	msg, err := fix.Parse(raw)
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

	"github.com/sylr/fix2/fix"
)

func TestPerMessageSpec(t *testing.T) {
	// 900=2|100=P1|200=1|201=C|100=P2|: in A, 100 starts the second parent
	// entry; the merged NoChild (which allows 100) must not swallow it.
	raw := fix.NewBuilder("FIX.4.4", "U1").
		SetInt(900, 2).
		AppendString(100, "P1").AppendInt(200, 1).AppendString(201, "C").
		AppendString(100, "P2").
		Build()
	msg, err := fix.Parse(raw)
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

// TestGenerateDeterministic generates the same dictionary twice: the output
// must not depend on map iteration order.
func TestGenerateDeterministic(t *testing.T) {
	dict, err := xmldict.LoadFile("../../../fix/dictionary/testdata/FIX50SP2.xml")
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
	dict, err := xmldict.LoadFile("../../../fix/dictionary/testdata/FIX50SP2.xml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	// EncodedLegIssuerLen (618) / EncodedLegIssuer (619), with another field
	// (600) between them or the length missing entirely.
	for _, tc := range []struct{ in, want []int }{
		{[]int{1, 618, 600, 619}, []int{1, 600, 618, 619}},
		{[]int{1, 619, 600}, []int{1, 618, 619, 600}},
		{[]int{1, 618, 619}, []int{1, 618, 619}},
		// LENGTH as delimiter pulls DATA right behind it. (A DATA
		// delimiter is rejected by checkNames.)
		{[]int{618, 600, 619}, []int{618, 619, 600}},
		// LENGTH without its DATA gets it right after.
		{[]int{1, 618, 600}, []int{1, 618, 619, 600}},
		{[]int{618, 600}, []int{618, 619, 600}},
	} {
		if got := pairDataLengths(dict, tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("pairDataLengths(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestGenerateHeaderGroupSharing covers a message that redefines the
// header's group under the same name and reuses a header-group child as a
// direct body field.
func TestGenerateHeaderGroupSharing(t *testing.T) {
	const xml = `<fix type="FIXT" major="1" minor="1">
  <header>
    <field name="SenderCompID" required="Y"/>
    <group name="NoHops" required="N"><field name="HopCompID" required="N"/></group>
  </header>
  <trailer/>
  <messages>
    <message name="Heartbeat" msgtype="0" msgcat="admin">
      <field name="HopCompID" required="N"/>
      <group name="NoHops" required="N"><field name="HopCompID" required="N"/></group>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="49" name="SenderCompID" type="STRING"/>
    <field number="627" name="NoHops" type="NUMINGROUP"/>
    <field number="628" name="HopCompID" type="STRING"/>
  </fields>
</fix>`
	runGenerated(t, xml, "hdr", `package hdr

import (
	"testing"

	"github.com/sylr/fix2/fix"
)

func TestSharing(t *testing.T) {
	raw := BuildHeartbeat("FIXT.1.1").
		SetSenderCompID("S").
		SetHopCompID("direct").
		AddNoHopsEntry(NewNoHopsEntry().SetHopCompID("hop")).
		Build()
	msg, err := fix.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	hb := WrapHeartbeat(&msg)
	hops := hb.NoHopsEntries()
	if len(hops) != 1 {
		t.Fatalf("%q: %d hops, want 1", raw, len(hops))
	}
	if v, _ := hops[0].HopCompID(); v != "hop" {
		t.Errorf("hop HopCompID = %q, want hop", v)
	}
	// The header group comes first on the wire: the direct getter must skip it.
	if v, _ := hb.HopCompID(); v != "direct" {
		t.Errorf("direct HopCompID = %q, want direct", v)
	}
}
`)
}

// TestGenerateGroupEdgeCases round-trips groups named like builder members,
// a group delimited by a LENGTH field and a group with a DATA field but no
// declared LENGTH field.
func TestGenerateGroupEdgeCases(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages>
    <message name="M" msgtype="U1" msgcat="app">
      <group name="Fields" required="N">
        <field name="A" required="N"/>
        <group name="AppendTo" required="N"><field name="B" required="N"/></group>
      </group>
      <group name="NoTexts" required="N">
        <field name="EncodedTextLen" required="N"/>
        <field name="EncodedText" required="N"/>
        <field name="A" required="N"/>
      </group>
      <group name="NoNotes" required="N">
        <field name="B" required="N"/>
        <field name="EncodedHeadline" required="N"/>
      </group>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="5001" name="Fields" type="NUMINGROUP"/>
    <field number="5002" name="AppendTo" type="NUMINGROUP"/>
    <field number="5003" name="NoTexts" type="NUMINGROUP"/>
    <field number="5004" name="NoNotes" type="NUMINGROUP"/>
    <field number="5010" name="A" type="STRING"/>
    <field number="5011" name="B" type="STRING"/>
    <field number="354" name="EncodedTextLen" type="LENGTH"/>
    <field number="355" name="EncodedText" type="DATA"/>
    <field number="358" name="EncodedHeadlineLen" type="LENGTH"/>
    <field number="359" name="EncodedHeadline" type="DATA"/>
  </fields>
</fix>`
	runGenerated(t, xml, "edge", `package edge

import (
	"testing"

	"github.com/sylr/fix2/fix"
)

func TestEdges(t *testing.T) {
	raw := BuildM("FIX.4.4").
		AddFieldsEntry(NewFieldsEntry().SetA("a").AddAppendToEntry(NewAppendToEntry().SetB("b"))).
		AddNoTextsEntry(NewNoTextsEntry().SetA("x").SetEncodedText([]byte("t\x01"))).
		AddNoNotesEntry(NewNoNotesEntry().SetB("y").SetEncodedHeadline([]byte("h\x01"))).
		Build()
	msg, err := fix.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	m := WrapM(&msg)
	fields := m.FieldsEntries()
	if len(fields) != 1 || len(fields[0].AppendToEntries()) != 1 {
		t.Errorf("%q: Fields/AppendTo not read back", raw)
	}
	texts := m.NoTextsEntries()
	if len(texts) != 1 {
		t.Fatalf("%q: %d texts, want 1", raw, len(texts))
	}
	if v, _ := texts[0].EncodedText(); string(v) != "t\x01" {
		t.Errorf("EncodedText = %q", v)
	}
	if a, _ := texts[0].A(); a != "x" {
		t.Errorf("A = %q", a)
	}
	notes := m.NoNotesEntries()
	if len(notes) != 1 {
		t.Fatalf("%q: %d notes, want 1", raw, len(notes))
	}
	if v, _ := notes[0].EncodedHeadline(); string(v) != "h\x01" {
		t.Errorf("EncodedHeadline = %q", v)
	}
}
`)
}

// TestGenerateNameClashes covers a message named like an enum field, as
// FIX 5.0's SecurityStatus message and SecurityStatus (965) field.
func TestGenerateNameClashes(t *testing.T) {
	const xml = `<fix type="FIX" major="5" minor="0">
  <header/><trailer/>
  <messages>
    <message name="SecurityStatus" msgtype="f" msgcat="app">
      <field name="SecurityStatus" required="N"/>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="965" name="SecurityStatus" type="STRING">
      <value enum="1" description="ACTIVE"/>
    </field>
  </fields>
</fix>`
	runGenerated(t, xml, "clash", `package clash

import (
	"testing"

	"github.com/sylr/fix2/fix"
)

func TestClash(t *testing.T) {
	raw := BuildSecurityStatus("FIXT.1.1").SetSecurityStatus(SecurityStatusActive).Build()
	msg, err := fix.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var st SecurityStatusEnum
	st, ok := WrapSecurityStatus(&msg).SecurityStatus()
	if !ok || st != SecurityStatusActive {
		t.Errorf("SecurityStatus = %q, %v", st, ok)
	}
}
`)
}

func TestGenerateMethodClashErrors(t *testing.T) {
	dict := func(fieldName string) *dictionary.Dict {
		t.Helper()
		d, err := xmldict.Load(strings.NewReader(fmt.Sprintf(`<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages><message name="M" msgtype="U1" msgcat="app">
    <field name="%s" required="N"/>
    <group name="NoX" required="N"><field name="A" required="N"/></group>
  </message></messages>
  <components/>
  <fields>
    <field number="5000" name="%s" type="STRING"/>
    <field number="5001" name="NoX" type="NUMINGROUP"/>
    <field number="5002" name="A" type="STRING"/>
  </fields>
</fix>`, fieldName, fieldName)))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	for _, name := range []string{"NoXEntries", "String"} {
		if err := generate(dict(name), "m", t.TempDir()); err == nil {
			t.Errorf("field %s: expected a clash error", name)
		}
	}
	if err := generate(dict("Plain"), "m", t.TempDir()); err != nil {
		t.Errorf("field Plain: %v", err)
	}
}

func TestGenerateRejectsUnnameable(t *testing.T) {
	gen := func(xml string) error {
		d, err := xmldict.Load(strings.NewReader(xml))
		if err != nil {
			t.Fatal(err)
		}
		return generate(d, "x", t.TempDir())
	}
	const tmpl = `<fix type="FIX" major="4" minor="4"><header/><trailer/>
  <messages><message name="%s" msgtype="U1" msgcat="app">%s</message></messages>
  <components/><fields>%s</fields></fix>`
	for name, xml := range map[string]string{
		"lower-case message":  fmt.Sprintf(tmpl, "m", "", ""),
		"underscore in field": fmt.Sprintf(tmpl, "M", `<field name="A_B" required="N"/>`, `<field number="5000" name="A_B" type="STRING"/>`),
		"DATA delimiter": fmt.Sprintf(tmpl, "M",
			`<group name="NoX" required="N"><field name="EncodedText" required="N"/><field name="EncodedTextLen" required="N"/></group>`,
			`<field number="5001" name="NoX" type="NUMINGROUP"/><field number="354" name="EncodedTextLen" type="LENGTH"/><field number="355" name="EncodedText" type="DATA"/>`),
	} {
		if err := gen(xml); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestGenerateHeaderDataPair covers a header declaring SecureData without
// its LENGTH field: the pair must stay together in the header section.
func TestGenerateHeaderDataPair(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4">
  <header><field name="SenderCompID" required="Y"/><field name="SecureData" required="N"/></header>
  <trailer/>
  <messages><message name="Heartbeat" msgtype="0" msgcat="admin"><field name="TestReqID" required="N"/></message></messages>
  <components/>
  <fields>
    <field number="49" name="SenderCompID" type="STRING"/>
    <field number="90" name="SecureDataLen" type="LENGTH"/>
    <field number="91" name="SecureData" type="DATA"/>
    <field number="112" name="TestReqID" type="STRING"/>
  </fields>
</fix>`
	runGenerated(t, xml, "hdrdata", `package hdrdata

import (
	"testing"

	"github.com/sylr/fix2/fix"
)

func TestPair(t *testing.T) {
	raw := BuildHeartbeat("FIX.4.4").
		SetTestReqID("T").
		SetSecureData([]byte("s\x01d")).
		SetSenderCompID("S").
		Build()
	msg, err := fix.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	if v, _ := WrapHeartbeat(&msg).SecureData(); string(v) != "s\x01d" {
		t.Errorf("SecureData = %q", v)
	}
}
`)
}

// TestGenerateClashCatchAll covers name combinations found in review: the
// generator must either produce compiling code or fail with an error.
func TestGenerateClashCatchAll(t *testing.T) {
	gen := func(msgs, fields string) error {
		d, err := xmldict.Load(strings.NewReader(`<fix type="FIX" major="4" minor="4"><header/><trailer/><messages>` + msgs + `</messages><components/><fields>` + fields + `</fields></fix>`))
		if err != nil {
			t.Fatal(err)
		}
		return generate(d, "x", t.TempDir())
	}
	field := func(num int, name, typ string, enums ...string) string {
		var v strings.Builder
		for i, e := range enums {
			fmt.Fprintf(&v, `<value enum="%d" description="%s"/>`, i+1, e)
		}
		return fmt.Sprintf(`<field number="%d" name="%s" type="%s">%s</field>`, num, name, typ, v.String())
	}
	for _, tc := range []struct {
		name, msgs, fields string
		wantErr            bool
	}{
		{"message named like another's builder",
			`<message name="Foo" msgtype="U1" msgcat="app"/><message name="FooBuilder" msgtype="U2" msgcat="app"/>`, "", true},
		{"enum descriptions normalizing alike",
			`<message name="M" msgtype="U1" msgcat="app"><field name="Side" required="N"/></message>`, field(54, "Side", "CHAR", "BUY", "Buy"), true},
		{"two messages with the same name",
			`<message name="M" msgtype="U1" msgcat="app"/><message name="M" msgtype="U2" msgcat="app"/>`, "", true},
		{"DATA delimiter in one message only",
			`<message name="A" msgtype="U1" msgcat="app"><group name="NoX" required="N"><field name="B" required="N"/></group></message>
			 <message name="C" msgtype="U2" msgcat="app"><group name="NoX" required="N"><field name="EncodedText" required="N"/></group></message>`,
			field(5001, "NoX", "NUMINGROUP") + field(5002, "B", "STRING") + field(354, "EncodedTextLen", "LENGTH") + field(355, "EncodedText", "DATA"), true},
		{"Foo and FooEnum enums next to message Foo",
			`<message name="Foo" msgtype="U1" msgcat="app"><field name="Foo" required="N"/><field name="FooEnum" required="N"/></message>`,
			field(5001, "Foo", "CHAR", "A") + field(5002, "FooEnum", "CHAR", "B"), false},
		{"NoXEntries field on a message without NoX",
			`<message name="A" msgtype="U1" msgcat="app"><group name="NoX" required="N"><field name="B" required="N"/></group></message>
			 <message name="C" msgtype="U2" msgcat="app"><field name="NoXEntries" required="N"/></message>`,
			field(5001, "NoX", "NUMINGROUP") + field(5002, "B", "STRING") + field(5003, "NoXEntries", "STRING"), false},
	} {
		err := gen(tc.msgs, tc.fields)
		if tc.wantErr && err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// TestGenerateDataPairAcrossSections covers SecureData declared in the
// header and its LENGTH in the trailer: the pair is written once, in the
// header.
func TestGenerateDataPairAcrossSections(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4">
  <header><field name="SecureData" required="N"/></header>
  <trailer><field name="SecureDataLen" required="N"/></trailer>
  <messages><message name="Heartbeat" msgtype="0" msgcat="admin"><field name="TestReqID" required="N"/></message></messages>
  <components/>
  <fields>
    <field number="90" name="SecureDataLen" type="LENGTH"/>
    <field number="91" name="SecureData" type="DATA"/>
    <field number="112" name="TestReqID" type="STRING"/>
  </fields>
</fix>`
	runGenerated(t, xml, "pairsec", `package pairsec

import (
	"bytes"
	"testing"

	"github.com/sylr/fix2/fix"
)

func TestOnce(t *testing.T) {
	raw := BuildHeartbeat("FIX.4.4").SetTestReqID("T").SetSecureData([]byte("s")).Build()
	if n := bytes.Count(raw, []byte("\x0191=")); n != 1 {
		t.Fatalf("%q: SecureData written %d times", raw, n)
	}
	if n := bytes.Count(raw, []byte("\x0190=")); n != 1 {
		t.Fatalf("%q: SecureDataLen written %d times", raw, n)
	}
	// Adjacent, in the header (before the body's TestReqID).
	pair, body := bytes.Index(raw, []byte("\x0190=1\x0191=s\x01")), bytes.Index(raw, []byte("\x01112="))
	if pair < 0 || pair > body {
		t.Fatalf("%q: SecureDataLen/SecureData not adjacent before the body", raw)
	}
	if _, err := fix.Parse(raw); err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
}
`)
}

// TestGenerateClashCases complements TestGenerateClashCatchAll with the
// remaining combinations from review; successful cases are compiled.
func TestGenerateClashCases(t *testing.T) {
	field := func(num int, name, typ string, enums ...string) string {
		var v strings.Builder
		for i, e := range enums {
			fmt.Fprintf(&v, `<value enum="%d" description="%s"/>`, i+1, e)
		}
		return fmt.Sprintf(`<field number="%d" name="%s" type="%s">%s</field>`, num, name, typ, v.String())
	}
	dict := func(msgs, fields string) string {
		return `<fix type="FIX" major="4" minor="4"><header/><trailer/><messages>` + msgs + `</messages><components/><fields>` + fields + `</fields></fix>`
	}
	for _, tc := range []struct{ name, msgs, fields string }{
		{"WrapFoo message", `<message name="Foo" msgtype="U1" msgcat="app"/><message name="WrapFoo" msgtype="U2" msgcat="app"/>`, ""},
		{"NoXEntry message", `<message name="M" msgtype="U1" msgcat="app"><group name="NoX" required="N"><field name="A" required="N"/></group></message><message name="NoXEntry" msgtype="U2" msgcat="app"/>`,
			field(5001, "NoX", "NUMINGROUP") + field(5002, "A", "STRING")},
		{"TagFoo message", `<message name="TagFoo" msgtype="U1" msgcat="app"><field name="Foo" required="N"/></message>`, field(5001, "Foo", "STRING")},
		{"cross-field enum constants", `<message name="M" msgtype="U1" msgcat="app"><field name="A" required="N"/><field name="AB" required="N"/></message>`,
			field(5001, "A", "CHAR", "B_C") + field(5002, "AB", "CHAR", "C")},
		{"Time and TimeWithFormat", `<message name="M" msgtype="U1" msgcat="app"><field name="Time" required="N"/><field name="TimeWithFormat" required="N"/></message>`,
			field(5001, "Time", "UTCTIMESTAMP") + field(5002, "TimeWithFormat", "STRING")},
		{"nested DATA delimiter", `<message name="M" msgtype="U1" msgcat="app"><group name="NoX" required="N"><field name="A" required="N"/><group name="NoY" required="N"><field name="EncodedText" required="N"/></group></group></message>`,
			field(5001, "NoX", "NUMINGROUP") + field(5002, "A", "STRING") + field(5003, "NoY", "NUMINGROUP") + field(354, "EncodedTextLen", "LENGTH") + field(355, "EncodedText", "DATA")},
	} {
		d, err := xmldict.Load(strings.NewReader(dict(tc.msgs, tc.fields)))
		if err != nil {
			t.Fatalf("%s: Load: %v", tc.name, err)
		}
		if err := generate(d, "x", t.TempDir()); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}

	// A String field used only inside a group, and one never used, generate
	// compiling code: only a message-level setter would shadow SetString.
	runGenerated(t, dict(
		`<message name="M" msgtype="U1" msgcat="app"><group name="NoX" required="N"><field name="String" required="N"/></group></message>`,
		field(5001, "NoX", "NUMINGROUP")+field(5002, "String", "STRING")+field(5003, "Unused", "STRING")),
		"strs", "package strs\n")
	// The successful catch-all fixtures compile too.
	runGenerated(t, dict(
		`<message name="Foo" msgtype="U1" msgcat="app"><field name="Foo" required="N"/><field name="FooEnum" required="N"/></message>`,
		field(5001, "Foo", "CHAR", "A")+field(5002, "FooEnum", "CHAR", "B")),
		"fooenum", "package fooenum\n")
	runGenerated(t, dict(
		`<message name="A" msgtype="U1" msgcat="app"><group name="NoX" required="N"><field name="B" required="N"/></group></message>
		 <message name="C" msgtype="U2" msgcat="app"><field name="NoXEntries" required="N"/></message>`,
		field(5001, "NoX", "NUMINGROUP")+field(5002, "B", "STRING")+field(5003, "NoXEntries", "STRING")),
		"noxentries", "package noxentries\n")
}

// TestGenerateScopedGroupReads covers tags and groups reused across nesting
// levels: Q is in NoChildren entries and in their parent NoParents entry
// after them, and NoShared is nested in NoOuter entries and also a group of
// the message after NoOuter. Readers must return each level's own.
func TestGenerateScopedGroupReads(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages>
    <message name="Msg" msgtype="U1" msgcat="app">
      <group name="NoParents" required="N">
        <field name="P" required="N"/>
        <group name="NoChildren" required="N"><field name="R" required="N"/><field name="Q" required="N"/></group>
        <field name="Q" required="N"/>
      </group>
      <group name="NoOuter" required="N">
        <field name="O" required="N"/>
        <component name="Shared" required="N"/>
      </group>
      <component name="Shared" required="N"/>
    </message>
  </messages>
  <components>
    <component name="Shared"><group name="NoShared" required="N"><field name="S" required="N"/></group></component>
  </components>
  <fields>
    <field number="100" name="P" type="STRING"/>
    <field number="101" name="Q" type="STRING"/>
    <field number="102" name="R" type="STRING"/>
    <field number="103" name="O" type="STRING"/>
    <field number="104" name="S" type="STRING"/>
    <field number="501" name="NoParents" type="NUMINGROUP"/>
    <field number="502" name="NoChildren" type="NUMINGROUP"/>
    <field number="900" name="NoOuter" type="NUMINGROUP"/>
    <field number="200" name="NoShared" type="NUMINGROUP"/>
  </fields>
</fix>`
	runGenerated(t, xml, "scoped", `package scoped

import (
	"bytes"
	"testing"

	"github.com/sylr/fix2/fix"
)

func parse(t *testing.T, raw []byte) *Msg {
	t.Helper()
	m, err := fix.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return WrapMsg(&m)
}

func TestParentValueAfterChildren(t *testing.T) {
	raw := BuildMsg("FIX.4.4").
		AddNoParentsEntry(NewNoParentsEntry().SetP("p").SetQ("parent").
			AddNoChildrenEntry(NewNoChildrenEntry().SetR("r").SetQ("child"))).
		Build()
	if !bytes.Contains(raw, []byte("\x01501=1\x01100=p\x01502=1\x01102=r\x01101=child\x01101=parent\x01")) {
		t.Fatalf("unexpected layout %q", raw)
	}
	parents := parse(t, raw).NoParentsEntries()
	if len(parents) != 1 {
		t.Fatalf("got %d parents", len(parents))
	}
	if q, ok := parents[0].Q(); !ok || q != "parent" {
		t.Errorf("parent Q = %q, %v; want parent", q, ok)
	}
	children := parents[0].NoChildrenEntries()
	if len(children) != 1 {
		t.Fatalf("got %d children", len(children))
	}
	if q, ok := children[0].Q(); !ok || q != "child" {
		t.Errorf("child Q = %q, %v; want child", q, ok)
	}
	if n, ok, err := parents[0].NoChildren(); !ok || err != nil || n != 1 {
		t.Errorf("NoChildren = %d, %v, %v", n, ok, err)
	}

	// Without the parent's own Q.
	raw = BuildMsg("FIX.4.4").
		AddNoParentsEntry(NewNoParentsEntry().SetP("p").
			AddNoChildrenEntry(NewNoChildrenEntry().SetR("r").SetQ("child"))).
		Build()
	if q, ok := parse(t, raw).NoParentsEntries()[0].Q(); ok {
		t.Errorf("absent parent Q = %q, true", q)
	}
}

func TestGroupNestedAndDirect(t *testing.T) {
	raw := BuildMsg("FIX.4.4").
		AddNoOuterEntry(NewNoOuterEntry().SetO("o").AddNoSharedEntry(NewNoSharedEntry().SetS("nested"))).
		AddNoSharedEntry(NewNoSharedEntry().SetS("top")).
		Build()
	if !bytes.Contains(raw, []byte("\x01900=1\x01103=o\x01200=1\x01104=nested\x01200=1\x01104=top\x01")) {
		t.Fatalf("unexpected layout %q", raw)
	}
	m := parse(t, raw)
	top := m.NoSharedEntries()
	if len(top) != 1 {
		t.Fatalf("got %d top-level NoShared entries", len(top))
	}
	if s, _ := top[0].S(); s != "top" {
		t.Errorf("top-level NoShared S = %q, want top", s)
	}
	outer := m.NoOuterEntries()
	if len(outer) != 1 {
		t.Fatalf("got %d NoOuter entries", len(outer))
	}
	nested := outer[0].NoSharedEntries()
	if len(nested) != 1 {
		t.Fatalf("got %d nested NoShared entries", len(nested))
	}
	if s, _ := nested[0].S(); s != "nested" {
		t.Errorf("nested NoShared S = %q, want nested", s)
	}

	// Only nested: the message has no NoShared group of its own.
	raw = BuildMsg("FIX.4.4").
		AddNoOuterEntry(NewNoOuterEntry().SetO("o").AddNoSharedEntry(NewNoSharedEntry().SetS("nested"))).
		Build()
	if got := parse(t, raw).NoSharedEntries(); len(got) != 0 {
		t.Errorf("got %d top-level NoShared entries, want 0", len(got))
	}
}
`)
}

// TestGeneratePerMessageGroupLayouts covers a group whose entry type is
// shared by messages ordering it differently: A has NoItems [P, Q, R], B
// [Q, P], and C starts its entries with the nested NoSub group. Each message
// writes entries in its own order, so its reader finds them.
func TestGeneratePerMessageGroupLayouts(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages>
    <message name="A" msgtype="U1" msgcat="app">
      <group name="NoItems" required="N"><field name="P" required="N"/><field name="Q" required="N"/><field name="R" required="N"/></group>
    </message>
    <message name="B" msgtype="U2" msgcat="app">
      <group name="NoItems" required="N"><field name="Q" required="N"/><field name="P" required="N"/></group>
    </message>
    <message name="C" msgtype="U3" msgcat="app">
      <group name="NoItems" required="N">
        <group name="NoSub" required="N"><field name="S" required="N"/></group>
        <field name="P" required="N"/>
      </group>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="100" name="P" type="STRING"/>
    <field number="101" name="Q" type="STRING"/>
    <field number="102" name="R" type="STRING"/>
    <field number="103" name="S" type="STRING"/>
    <field number="500" name="NoItems" type="NUMINGROUP"/>
    <field number="501" name="NoSub" type="NUMINGROUP"/>
  </fields>
</fix>`
	runGenerated(t, xml, "layouts", `package layouts

import (
	"bytes"
	"testing"

	"github.com/sylr/fix2/fix"
)

func parse(t *testing.T, raw []byte) *fix.Message {
	t.Helper()
	m, err := fix.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return &m
}

func TestOwnOrder(t *testing.T) {
	item := func() *NoItemsEntryBuilder { return NewNoItemsEntry().SetP("p").SetQ("q") }

	a := BuildA("FIX.4.4").AddNoItemsEntry(item().SetR("r")).AddNoItemsEntry(item()).Build()
	if !bytes.Contains(a, []byte("\x01500=2\x01100=p\x01101=q\x01102=r\x01100=p\x01101=q\x01")) {
		t.Errorf("A: unexpected layout %q", a)
	}
	if got := WrapA(parse(t, a)).NoItemsEntries(); len(got) != 2 {
		t.Errorf("A: got %d entries", len(got))
	}

	b := BuildB("FIX.4.4").AddNoItemsEntry(item()).AddNoItemsEntry(item()).Build()
	if !bytes.Contains(b, []byte("\x01500=2\x01101=q\x01100=p\x01101=q\x01100=p\x01")) {
		t.Errorf("B: unexpected layout %q", b)
	}
	entries := WrapB(parse(t, b)).NoItemsEntries()
	if len(entries) != 2 {
		t.Fatalf("B: got %d entries", len(entries))
	}
	for i, e := range entries {
		if p, _ := e.P(); p != "p" {
			t.Errorf("B entry %d: P = %q", i, p)
		}
	}
}

func TestNestedDelimiterInOneMessage(t *testing.T) {
	c := BuildC("FIX.4.4").
		AddNoItemsEntry(NewNoItemsEntry().SetP("p1").AddNoSubEntry(NewNoSubEntry().SetS("s"))).
		AddNoItemsEntry(NewNoItemsEntry().SetP("p2")).
		Build()
	// C's entries start with NoSub, written even with no entries.
	if !bytes.Contains(c, []byte("\x01500=2\x01501=1\x01103=s\x01100=p1\x01501=0\x01100=p2\x01")) {
		t.Errorf("C: unexpected layout %q", c)
	}
	entries := WrapC(parse(t, c)).NoItemsEntries()
	if len(entries) != 2 {
		t.Fatalf("C: got %d entries", len(entries))
	}
	if p, _ := entries[1].P(); p != "p2" {
		t.Errorf("C entry 1: P = %q", p)
	}
	// A defines no NoSub: entries set on it are not written.
	a := BuildA("FIX.4.4").AddNoItemsEntry(NewNoItemsEntry().SetP("p").AddNoSubEntry(NewNoSubEntry().SetS("s"))).Build()
	if !bytes.Contains(a, []byte("\x01500=1\x01100=p\x0110=")) {
		t.Errorf("A: unexpected layout %q", a)
	}
}

func TestFieldsOutsideTheMessageDefinition(t *testing.T) {
	// B defines no R: writing it anywhere would end B's first entry for
	// B's readers and hide the second, so it is not written.
	item := func() *NoItemsEntryBuilder { return NewNoItemsEntry().SetP("p").SetQ("q").SetR("r") }
	b := BuildB("FIX.4.4").AddNoItemsEntry(item()).AddNoItemsEntry(item()).Build()
	if !bytes.Contains(b, []byte("\x01500=2\x01101=q\x01100=p\x01101=q\x01100=p\x0110=")) {
		t.Errorf("B: unexpected layout %q", b)
	}
	if got := WrapB(parse(t, b)).NoItemsEntries(); len(got) != 2 {
		t.Errorf("B: got %d entries, want 2", len(got))
	}
}

func TestZeroValueEntry(t *testing.T) {
	// Entries not read from a message have no spec: getters and nested
	// iteration must not panic.
	var e NoItemsEntry
	if _, ok := e.P(); ok {
		t.Error("zero entry has P")
	}
	if got := e.NoSubEntries(); got != nil {
		t.Errorf("zero entry NoSubEntries = %v", got)
	}
}
`)
}

// TestGenerateLengthWithoutData covers a message whose group lists a LENGTH
// field without its DATA field, which another message's definition has:
// the shared DATA setter writes both, and they must stay adjacent.
func TestGenerateLengthWithoutData(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages>
    <message name="A" msgtype="U1" msgcat="app">
      <group name="NoItems" required="N"><field name="P" required="N"/><field name="RawDataLength" required="N"/><field name="RawData" required="N"/></group>
    </message>
    <message name="B" msgtype="U2" msgcat="app">
      <group name="NoItems" required="N"><field name="P" required="N"/><field name="RawDataLength" required="N"/><field name="Q" required="N"/></group>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="95" name="RawDataLength" type="LENGTH"/>
    <field number="96" name="RawData" type="DATA"/>
    <field number="100" name="P" type="STRING"/>
    <field number="101" name="Q" type="STRING"/>
    <field number="500" name="NoItems" type="NUMINGROUP"/>
  </fields>
</fix>`
	runGenerated(t, xml, "lengthonly", `package lengthonly

import (
	"bytes"
	"testing"

	"github.com/sylr/fix2/fix"
)

func TestAdjacent(t *testing.T) {
	raw := BuildB("FIX.4.4").AddNoItemsEntry(NewNoItemsEntry().SetP("p").SetRawData([]byte("a\x01")).SetQ("q")).Build()
	if !bytes.Contains(raw, []byte("\x01500=1\x01100=p\x0195=2\x0196=a\x01\x01101=q\x01")) {
		t.Errorf("unexpected layout %q", raw)
	}
	m, err := fix.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	entries := WrapB(&m).NoItemsEntries()
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if q, _ := entries[0].Q(); q != "q" {
		t.Errorf("Q = %q, want q: the DATA field must not end the entry", q)
	}

	raw = BuildB("FIX.4.4").
		AddNoItemsEntry(NewNoItemsEntry().SetP("p1").SetRawData([]byte("x"))).
		AddNoItemsEntry(NewNoItemsEntry().SetP("p2").SetRawData([]byte("yz")).SetQ("q2")).
		Build()
	m, err = fix.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	entries = WrapB(&m).NoItemsEntries()
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	for i, want := range []string{"x", "yz"} {
		if v, _ := entries[i].RawData(); string(v) != want {
			t.Errorf("entry %d RawData = %q, want %q", i, v, want)
		}
	}
	if q, _ := entries[1].Q(); q != "q2" {
		t.Errorf("entry 1 Q = %q, want q2", q)
	}
}
`)
}

// dictionaryTestXML exercises every part of a generated dictionary literal.
const dictionaryTestXML = `<fix type="FIXT" major="1" minor="1">
  <header>
    <field name="MsgType" required="Y"/>
    <field name="SenderCompID" required="Y"/>
    <group name="NoHops" required="N"><field name="HopCompID" required="N"/><field name="HopRefID" required="N"/></group>
    <component name="HdrOpt" required="N"/>
  </header>
  <trailer>
    <component name="Sig" required="N"/>
    <field name="CheckSum" required="Y"/>
  </trailer>
  <messages>
    <message name="Order" msgtype="D" msgcat="app">
      <field name="Side" required="Y"/>
      <group name="NoParties" required="Y">
        <field name="PartyID" required="N"/>
        <group name="NoSubs" required="N"><field name="SubID" required="N"/></group>
        <field name="Text" required="N"/>
        <component name="PtyOpt" required="N"/>
      </group>
      <component name="Opt" required="N"/>
    </message>
    <message name="Ping" msgtype="0"/>
  </messages>
  <components>
    <component name="HdrOpt"><field name="OnBehalfOfCompID" required="Y"/><field name="OnBehalfOfSubID" required="N"/></component>
    <component name="Sig"><field name="SignatureLength" required="Y"/><field name="Signature" required="N"/></component>
    <component name="Opt"><field name="OptA" required="Y"/><field name="OptB" required="N"/></component>
    <component name="PtyOpt"><field name="PtyOptA" required="Y"/><field name="PtyOptB" required="N"/></component>
  </components>
  <fields>
    <field number="10" name="CheckSum" type="STRING"/>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="49" name="SenderCompID" type="STRING"/>
    <field number="54" name="Side" type="CHAR"><value enum="1" description="BUY"/><value enum="2" description="SELL &quot;quoted&quot; \ back"/></field>
    <field number="58" name="Text" type="STRING"/>
    <field number="89" name="Signature" type="DATA"/>
    <field number="93" name="SignatureLength" type="LENGTH"/>
    <field number="115" name="OnBehalfOfCompID" type="STRING"/>
    <field number="116" name="OnBehalfOfSubID" type="STRING"/>
    <field number="448" name="PartyID" type="STRING"/>
    <field number="453" name="NoParties" type="NUMINGROUP"/>
    <field number="627" name="NoHops" type="NUMINGROUP"/>
    <field number="628" name="HopCompID" type="STRING"/>
    <field number="630" name="HopRefID" type="SEQNUM"/>
    <field number="5000" name="NoSubs" type="NUMINGROUP"/>
    <field number="5001" name="SubID" type="MULTIPLESTRINGVALUE"><value enum="AB" description="ab"/><value enum="C" description="c"/></field>
    <field number="5002" name="OptA" type="STRING"/>
    <field number="5003" name="OptB" type="STRING"/>
    <field number="5004" name="PtyOptA" type="STRING"/>
    <field number="5005" name="PtyOptB" type="STRING"/>
  </fields>
</fix>`

// TestGenerateDictionary checks that the generated Dictionary is the one
// xmldict.Load reads from the same XML, and validates with it.
func TestGenerateDictionary(t *testing.T) {
	runGenerated(t, dictionaryTestXML, "gendict", `package gendict

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary/xmldict"
)

const xml = `+"`"+strings.ReplaceAll(dictionaryTestXML, "`", "")+"`"+`

func TestDictionary(t *testing.T) {
	want, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	got := Dictionary()
	for name, pair := range map[string][2]any{
		"Type": {got.Type, want.Type}, "Major": {got.Major, want.Major}, "Minor": {got.Minor, want.Minor},
		"Fields": {got.Fields, want.Fields}, "Header": {got.Header, want.Header}, "Trailer": {got.Trailer, want.Trailer},
		"HeaderGroups": {got.HeaderGroups, want.HeaderGroups}, "TrailerGroups": {got.TrailerGroups, want.TrailerGroups},
		"HeaderConditions": {got.HeaderConditions, want.HeaderConditions}, "TrailerConditions": {got.TrailerConditions, want.TrailerConditions},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s:\n%#v\n%#v", name, pair[0], pair[1])
		}
	}
	if len(want.HeaderConditions) == 0 || len(want.TrailerConditions) == 0 || len(want.HeaderGroups) == 0 {
		t.Error("the test dictionary should have header conditions, trailer conditions and header groups")
	}
	if len(want.Messages["D"].Groups[0].Conditions) == 0 {
		t.Error("the test dictionary should have a group with conditions")
	}
	if len(got.Messages) != len(want.Messages) {
		t.Fatalf("%d messages, want %d", len(got.Messages), len(want.Messages))
	}
	for mt, w := range want.Messages {
		g := got.Messages[mt]
		if g == nil || g.Name != w.Name || g.MsgType != w.MsgType || g.MsgCat != w.MsgCat ||
			!reflect.DeepEqual(g.Fields, w.Fields) || !reflect.DeepEqual(g.Groups, w.Groups) || !reflect.DeepEqual(g.Conditions, w.Conditions) {
			t.Errorf("message %s:\n%#v\n%#v", mt, g, w)
		}
	}
	if got.FieldByName("Side") == nil || got.FieldByName("Side").Tag != TagSide {
		t.Error("FieldByName(Side)")
	}

	for body, valid := range map[string]bool{
		"35=D\x0149=S\x0154=1\x01453=1\x01448=P\x0110=000\x01":         true,
		"35=D\x0149=S\x0154=3\x01453=1\x01448=P\x0110=000\x01":         false, // invalid Side
		"35=D\x0149=S\x0154=1\x01453=1\x01448=P\x015002=a\x0110=000\x01": true,  // Opt complete
		"35=D\x0149=S\x0154=1\x01453=1\x01448=P\x015003=b\x0110=000\x01": false, // Opt without OptA
	} {
		body = strings.TrimSuffix(body, "10=000\x01")
		raw := []byte("8=FIXT.1.1\x019=" + itoa(len(body)) + "\x01" + body)
		sum := 0
		for _, c := range raw {
			sum += int(c)
		}
		raw = append(raw, []byte("10="+pad(sum%256)+"\x01")...)
		msg, err := fix.Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if err := got.Validate(&msg); (err == nil) != valid {
			t.Errorf("%q: Validate = %v, want valid=%v", body, err, valid)
		}
	}
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

func pad(n int) string { s := itoa(n); return strings.Repeat("0", 3-len(s)) + s }
`)
}

// TestGenerateDictionaryNames covers an enum field named Dictionary, whose
// type must not clash with the generated Dictionary function, and a
// dictionary without messages or groups.
func TestGenerateDictionaryNames(t *testing.T) {
	runGenerated(t, `<fix major="4" minor="4"><header/><trailer/>
  <messages><message name="M" msgtype="M"><field name="Dictionary"/></message></messages>
  <components/><fields><field number="100" name="Dictionary" type="CHAR"><value enum="A" description="ALPHA"/></field></fields></fix>`, "dictname", `package dictname

import "testing"

func TestNames(t *testing.T) {
	var v DictionaryEnum = DictionaryAlpha
	if v != "A" || Dictionary().Fields[TagDictionary].Enums["A"] != "ALPHA" {
		t.Error(v)
	}
}
`)
	runGenerated(t, `<fix major="4" minor="4"><header/><trailer/><messages/><components/><fields/></fix>`, "empty", `package empty

import "testing"

func TestEmpty(t *testing.T) {
	if d := Dictionary(); len(d.Messages) != 0 || len(d.Fields) != 0 {
		t.Error(d)
	}
}
`)
}

// TestGenerateImplicitGroupLength covers a group declaring a DATA field but
// not its LENGTH field, which the trailer also declares: the group's
// implicit LENGTH must not answer the trailer's getter.
func TestGenerateImplicitGroupLength(t *testing.T) {
	runGenerated(t, `<fix major="4" minor="4">
  <header/>
  <trailer><field name="SignatureLength" required="N"/><field name="Signature" required="N"/></trailer>
  <messages><message name="M" msgtype="M">
    <group name="NoG" required="N"><field name="D" required="N"/><field name="Signature" required="N"/></group>
  </message></messages>
  <components/>
  <fields>
    <field number="89" name="Signature" type="DATA"/>
    <field number="93" name="SignatureLength" type="LENGTH"/>
    <field number="100" name="D" type="STRING"/>
    <field number="500" name="NoG" type="NUMINGROUP"/>
  </fields>
</fix>`, "implicitlen", `package implicitlen

import (
	"testing"

	"github.com/sylr/fix2/fix"
)

func TestTrailerLength(t *testing.T) {
	raw := BuildM("FIX.4.4").
		AddNoGEntry(NewNoGEntry().SetD("d").SetSignature([]byte("group"))).
		SetSignature([]byte("sig")).
		Build()
	msg, err := fix.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	m := WrapM(&msg)
	if n, ok, err := m.SignatureLength(); !ok || err != nil || n != 3 {
		t.Errorf("SignatureLength = %d, %v, %v; want the trailer's 3 (%q)", n, ok, err, raw)
	}
	if v, _ := m.Signature(); string(v) != "sig" {
		t.Errorf("Signature = %q", v)
	}
	if e := m.NoGEntries(); len(e) != 1 {
		t.Fatalf("entries = %d", len(e))
	} else if v, _ := e[0].Signature(); string(v) != "group" {
		t.Errorf("entry Signature = %q", v)
	}
}
`)
}

// TestGenerateCustomDataPair covers a DATA field only the dictionary pairs
// with its LENGTH field: the generated setter writes both, and messages parse
// with the dictionary's profile.
func TestGenerateCustomDataPair(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4" servicepack="0">
  <header><field name="MsgType" required="Y"/></header><trailer/>
  <messages>
    <message name="Blob" msgtype="U1" msgcat="app">
      <field name="CustomBlobLen" required="N"/>
      <field name="CustomBlob" required="N"/>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="50001" name="CustomBlobLen" type="LENGTH"/>
    <field number="50002" name="CustomBlob" type="DATA"/>
  </fields>
</fix>`
	runGenerated(t, xml, "blob", `package blob

import (
	"testing"

	"github.com/sylr/fix2/fix"
)

func TestCustomPair(t *testing.T) {
	raw := BuildBlob("FIX.4.4").SetCustomBlob([]byte("a\x01b")).Build()
	var m fix.Message
	if err := fix.ParseInto(&m, raw, Dictionary().Profile()); err != nil {
		t.Fatalf("ParseInto(%q): %v", raw, err)
	}
	if got, _ := WrapBlob(&m).CustomBlob(); string(got) != "a\x01b" {
		t.Errorf("CustomBlob = %q", got)
	}
	if err := Dictionary().Validate(&m); err != nil {
		t.Errorf("Validate: %v", err)
	}
}
`)
}

// TestGenerateCustomDataPairSent sends a custom DATA field holding SOH
// between two sessions whose dictionary pairs it with its length: in a
// NewOrderSingle's group entry, set by the generated setter, and on the
// acceptor's Logon, set by hand from ToAdmin. The default profile, which
// does not know the pair, refuses it.
func TestGenerateCustomDataPairSent(t *testing.T) {
	raw, err := os.ReadFile("../../../fix/dictionary/testdata/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	xml := string(raw)
	for _, r := range []struct{ old, new string }{
		{"<fields>", `<fields>
    <field number='50001' name='CustomBlobLen' type='LENGTH'/>
    <field number='50002' name='CustomBlob' type='DATA'/>
    <field number='50003' name='NoBlobs' type='NUMINGROUP'/>
    <field number='50004' name='BlobName' type='STRING'/>`},
		{"<field name='Password' required='N'/>", `<field name='Password' required='N'/>
      <field name='CustomBlobLen' required='N'/>
      <field name='CustomBlob' required='N'/>`},
		{"<field name='Text' required='N'/>\n    </message>\n    <message name='BusinessMessageReject'", `<field name='Text' required='N'/>
      <group name='NoBlobs' required='N'>
        <field name='BlobName' required='N'/>
        <field name='CustomBlobLen' required='N'/>
        <field name='CustomBlob' required='N'/>
      </group>
    </message>
    <message name='BusinessMessageReject'`},
	} {
		if strings.Count(xml, r.old) != 1 {
			t.Fatalf("FIX44.xml: %q not found once", r.old)
		}
		xml = strings.Replace(xml, r.old, r.new, 1)
	}
	runGenerated(t, xml, "blobs", `package blobs

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// end is either session's handler: it adds CustomBlob to its Logon if
// blob, and tells of the CustomBlob of the Logon and orders it receives,
// and of rejects. The initiator's Logon has none: an acceptor parses the
// first Logon with the default profile, to find its session.
type end struct {
	session.BaseHandler
	blob         bool
	logon, order chan []byte
	logons       chan struct{}
	rejects      chan string
}

func newEnd() *end {
	return &end{logon: make(chan []byte, 1), order: make(chan []byte, 1), logons: make(chan struct{}, 1), rejects: make(chan string, 8)}
}

func (e *end) ToAdmin(_ context.Context, _ *session.Session, msgType string, b *fix.Builder) error {
	if msgType == "A" && e.blob {
		// By hand, not with SetData: the session dictionary pairs them.
		b.SetInt(TagCustomBlobLen, 3).Set(TagCustomBlob, []byte("l\x01n"))
	}
	return nil
}

func (e *end) OnLogon(context.Context, *session.Session) { e.logons <- struct{}{} }

func (e *end) FromAdmin(_ context.Context, _ *session.Session, m *fix.Message, _ session.Inbound) {
	switch string(m.MsgType()) {
	case "A":
		e.logon <- bytes.Clone(m.Get(TagCustomBlob))
	case "3":
		e.rejects <- string(m.Get(58))
	}
}

func (e *end) FromApp(_ context.Context, _ *session.Session, m *fix.Message, _ session.Inbound) error {
	switch string(m.MsgType()) {
	case "D":
		for _, en := range WrapNewOrderSingle(m).NoBlobsEntries() {
			v, _ := en.CustomBlob()
			e.order <- bytes.Clone(v)
		}
	case "j":
		e.rejects <- string(m.Get(58))
	}
	return nil
}

func receive[T any](t *testing.T, c <-chan T, rejects <-chan string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case r := <-rejects:
		t.Fatalf("rejected: %s", r)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	var zero T
	return zero
}

func TestCustomDataSent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dicts := session.Dictionaries{Session: Dictionary()}
	acc, ini := newEnd(), newEnd()
	acc.blob = true
	a := session.NewAcceptor(session.AcceptorOptions{})
	if _, err := a.Add(session.Config{ID: session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"}, Handler: acc, Dictionaries: dicts}); err != nil {
		t.Fatal(err)
	}
	l := mem.Listen("blobs")
	a.Listen(l)
	go a.Run(ctx)
	i := session.NewInitiator(session.InitiatorOptions{})
	s, err := i.Add(session.Config{ID: session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"}, Handler: ini, Dictionaries: dicts}, l)
	if err != nil {
		t.Fatal(err)
	}
	go i.Run(ctx)
	if v := receive(t, ini.logon, ini.rejects); string(v) != "l\x01n" {
		t.Errorf("Logon's CustomBlob = %q", v)
	}
	receive(t, ini.logons, ini.rejects)

	o := BuildNewOrderSingle(session.FIX44).SetClOrdID("O1").SetSymbol("X").SetSide(SideBuy).
		SetTransactTime(time.Now()).SetOrderQty(1).SetOrdType(OrdTypeLimit).SetPrice(1).
		AddNoBlobsEntry(NewNoBlobsEntry().SetBlobName("b").SetCustomBlob([]byte("o\x01d")))
	if _, err := s.Send(ctx, o); err != nil {
		t.Fatal(err)
	}
	if v := receive(t, acc.order, ini.rejects); string(v) != "o\x01d" {
		t.Errorf("order's CustomBlob = %q", v)
	}
	var serr *fix.SOHValueError
	if _, err := o.AppendBody(nil, nil, nil); !errors.As(err, &serr) || serr.Tag != TagCustomBlob {
		t.Errorf("AppendBody with the default profile = %v, want a SOHValueError for CustomBlob", err)
	}
}
`)
}

// TestGenerateStandardDataPair covers RawData(96), which the parser reads by
// the length RawDataLength(95) gives, whether the dictionary defines 95 or
// not: builders write both, of messages and group entries, and readers read
// RawData back, from built messages and received ones.
func TestGenerateStandardDataPair(t *testing.T) {
	const tmpl = `<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header><trailer/>
  <messages><message name="M" msgtype="M" msgcat="app">%s<field name="RawData" required="N"/>
    <group name="NoX" required="N"><field name="Text" required="Y"/>%s<field name="RawData" required="N"/></group>
  </message></messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="58" name="Text" type="STRING"/>%s
    <field number="96" name="RawData" type="DATA"/>
    <field number="7001" name="NoX" type="NUMINGROUP"/>
  </fields>
</fix>`
	const length = `<field name="RawDataLength" required="N"/>`
	const lengthDef = `<field number="95" name="RawDataLength" type="LENGTH"/>`
	const test = `package PKG

import (
	"testing"

	"github.com/sylr/fix2/fix"
)

func parse(t *testing.T, raw []byte) *M {
	t.Helper()
	var m fix.Message
	if err := fix.ParseInto(&m, raw, Dictionary().Profile()); err != nil {
		t.Fatalf("ParseInto(%q): %v", raw, err)
	}
	if err := Dictionary().Validate(&m); err != nil {
		t.Fatalf("Validate(%q): %v", raw, err)
	}
	return WrapM(&m)
}

func check(t *testing.T, raw []byte, top string, entries ...string) {
	t.Helper()
	m := parse(t, raw)
	if v, ok := m.RawData(); string(v) != top || ok != (top != "") {
		t.Errorf("RawData = %q, %v; want %q (%q)", v, ok, top, raw)
	}
	es := m.NoXEntries()
	if len(es) != len(entries) {
		t.Fatalf("%d entries, want %d (%q)", len(es), len(entries), raw)
	}
	for i, want := range entries {
		if v, _ := es[i].RawData(); string(v) != want {
			t.Errorf("entry %d RawData = %q, want %q (%q)", i, v, want, raw)
		}
	}
}

func TestBuilt(t *testing.T) {
	if TagRawDataLength != 95 {
		t.Errorf("TagRawDataLength = %d", TagRawDataLength)
	}
	for _, v := range []string{"ab", "a\x01b", "\x0110=000\x01"} {
		check(t, BuildM("FIX.4.4").SetRawData([]byte(v)).Build(), v)
		check(t, BuildM("FIX.4.4").AddNoXEntry(NewNoXEntry().SetText("t").SetRawData([]byte(v))).Build(), "", v)
		check(t, BuildM("FIX.4.4").
			SetRawData([]byte(v)).
			AddNoXEntry(NewNoXEntry().SetText("t").SetRawData([]byte(v))).
			AddNoXEntry(NewNoXEntry().SetText("u")).
			AddNoXEntry(NewNoXEntry().SetText("w").SetRawData([]byte(v+v))).
			Build(), v, v, "", v+v)
	}
}

func TestReceived(t *testing.T) {
	raw := fix.NewBuilder("FIX.4.4", "M").
		SetData(95, 96, []byte("a\x01b")).
		Append(7001, []byte("2")).
		Append(58, []byte("t")).Append(95, []byte("3")).Append(96, []byte("c\x01d")).
		Append(58, []byte("u")).Append(95, []byte("2")).Append(96, []byte("ef")).
		Build()
	check(t, raw, "a\x01b", "c\x01d", "ef")
}
`
	for _, tc := range []struct{ name, pkg, length, lengthDef string }{
		{"RawDataLength undefined", "undeclared", "", ""},
		{"RawDataLength defined", "declared", length, lengthDef},
		{"RawDataLength defined, listed nowhere", "unlisted", "", lengthDef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			xml := fmt.Sprintf(tmpl, tc.length, tc.length, tc.lengthDef)
			runGenerated(t, xml, tc.pkg, strings.ReplaceAll(test, "PKG", tc.pkg))
		})
	}
}

// standardNamesXML defines EncodedText(355) and XmlData(213) without their
// LENGTH fields, EncodedTextLen(354) and XmlDataLen(212).
const standardNamesXML = `<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header><trailer/>
  <messages><message name="M" msgtype="M" msgcat="app">
    <field name="EncodedText" required="N"/>
    <group name="NoX" required="N"><field name="Text" required="Y"/><field name="XmlData" required="N"/></group>
  </message></messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="58" name="Text" type="STRING"/>
    <field number="213" name="XmlData" type="XMLDATA"/>
    <field number="355" name="EncodedText" type="DATA"/>
    <field number="7001" name="NoX" type="NUMINGROUP"/>
  </fields>
</fix>`

// TestWithLengthFields checks the dictionary the generator works from: it
// defines the LENGTH fields the dictionary leaves out, with their standard
// names, looked up as the others, and parses as the dictionary does.
func TestWithLengthFields(t *testing.T) {
	dict, err := xmldict.Load(strings.NewReader(standardNamesXML))
	if err != nil {
		t.Fatal(err)
	}
	full, err := withLengthFields(dict)
	if err != nil {
		t.Fatal(err)
	}
	for tag, name := range map[int]string{354: "EncodedTextLen", 212: "XmlDataLen"} {
		if fd := full.FieldByName(name); fd == nil || fd.Tag != tag || full.Fields[tag] != fd {
			t.Errorf("FieldByName(%s) = %v, want the definition of tag %d", name, fd, tag)
		}
		if dict.Fields[tag] != nil || dict.FieldByName(name) != nil {
			t.Errorf("the dictionary given now defines %s", name)
		}
	}
	want := slices.SortedFunc(slices.Values(dict.Profile().DataPairs()), func(a, b fix.DataPair) int { return a.Data - b.Data })
	got := slices.SortedFunc(slices.Values(full.Profile().DataPairs()), func(a, b fix.DataPair) int { return a.Data - b.Data })
	if !slices.Equal(got, want) {
		t.Errorf("DATA fields %v, want those of the dictionary given, %v", got, want)
	}
}

// TestStandardLengthNames checks the names of the standard LENGTH fields
// against the FIX Latest repository.
func TestStandardLengthNames(t *testing.T) {
	latest, err := xmldict.LoadFile("../../../fix/dictionary/testdata/OrchestraFIXLatest.xml")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range fix.DefaultProfile().DataPairs() {
		name := standardLengthNames[p.Length]
		if name == "" {
			t.Errorf("LENGTH field %d of DATA field %d has no standard name", p.Length, p.Data)
		}
		if fd := latest.Fields[p.Length]; fd != nil && fd.Name != name {
			t.Errorf("LENGTH field %d is named %s, FIX Latest names it %s", p.Length, name, fd.Name)
		}
	}
}

// TestGenerateStandardLengthNames covers DATA fields whose LENGTH fields
// the dictionary leaves out: the generated code names those as FIX does.
func TestGenerateStandardLengthNames(t *testing.T) {
	runGenerated(t, standardNamesXML, "stdnames", `package stdnames

import (
	"testing"

	"github.com/sylr/fix2/fix"
)

func TestNames(t *testing.T) {
	if TagEncodedTextLen != 354 || TagXmlDataLen != 212 {
		t.Errorf("TagEncodedTextLen = %d, TagXmlDataLen = %d", TagEncodedTextLen, TagXmlDataLen)
	}
	raw := BuildM("FIX.4.4").
		SetEncodedText([]byte("a\x01b")).
		AddNoXEntry(NewNoXEntry().SetText("t").SetXmlData([]byte("<x>\x01</x>"))).
		Build()
	var msg fix.Message
	if err := fix.ParseInto(&msg, raw, Dictionary().Profile()); err != nil {
		t.Fatalf("ParseInto(%q): %v", raw, err)
	}
	if err := Dictionary().Validate(&msg); err != nil {
		t.Fatalf("Validate(%q): %v", raw, err)
	}
	m := WrapM(&msg)
	if v, _ := m.EncodedText(); string(v) != "a\x01b" {
		t.Errorf("EncodedText = %q (%q)", v, raw)
	}
	if es := m.NoXEntries(); len(es) != 1 {
		t.Fatalf("%d entries (%q)", len(es), raw)
	} else if v, _ := es[0].XmlData(); string(v) != "<x>\x01</x>" {
		t.Errorf("XmlData = %q (%q)", v, raw)
	}
}
`)
}

// TestGenerateRejectsUnpairedData covers a DATA field no LENGTH field
// announces: its setter could not write a value the parser reads back, so
// the generator refuses the dictionary.
func TestGenerateRejectsUnpairedData(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(`<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header><trailer/>
  <messages><message name="Blob" msgtype="U1" msgcat="app"><field name="Text" required="N"/><field name="CustomBlob" required="N"/></message></messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="58" name="Text" type="STRING"/>
    <field number="50002" name="CustomBlob" type="DATA"/>
  </fields>
</fix>`))
	if err != nil {
		t.Fatal(err)
	}
	err = generate(d, "blob", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "CustomBlob") || !rejected(err) {
		t.Errorf("generate = %v, want CustomBlob refused for its missing LENGTH field", err)
	}
}

const rawGettersXML = `<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages>
    <message name="Quote" msgtype="S" msgcat="app">
      <field name="BidPx" required="N"/>
      <field name="TransactTime" required="N"/>
      <field name="Side" required="N"/>
      <field name="Symbol" required="N"/>
      <group name="NoLevels" required="N">
        <field name="LevelPx" required="N"/>
        <field name="LevelSize" required="N"/>
      </group>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="132" name="BidPx" type="PRICE"/>
    <field number="60" name="TransactTime" type="UTCTIMESTAMP"/>
    <field number="54" name="Side" type="CHAR"><value enum="1" description="BUY"/></field>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="5000" name="NoLevels" type="NUMINGROUP"/>
    <field number="5001" name="LevelPx" type="PRICE"/>
    <field number="5002" name="LevelSize" type="QTY"/>
  </fields>
</fix>`

// TestGenerateRawGetters covers --bytes: raw values of parsed fields, on
// messages and group entries, exactly as received.
func TestGenerateRawGetters(t *testing.T) {
	runGeneratedWith(t, rawGettersXML, "raw", `package raw

import (
	"reflect"
	"testing"

	"github.com/sylr/fix2/fix"
)

func TestRaw(t *testing.T) {
	msg, err := fix.Parse(fix.NewBuilder("FIX.4.4", "S").
		SetString(132, "62000.01000000").SetString(60, "20241019-05:40:11.464000").
		SetString(5000, "1").SetString(5001, "0.00001000").SetString(5002, "1.50").Build())
	if err != nil {
		t.Fatal(err)
	}
	q := WrapQuote(&msg)
	if v, ok := q.BidPxBytes(); !ok || string(v) != "62000.01000000" {
		t.Errorf("BidPxBytes = %q, %v", v, ok)
	}
	if v, ok := q.TransactTimeBytes(); !ok || string(v) != "20241019-05:40:11.464000" {
		t.Errorf("TransactTimeBytes = %q, %v", v, ok)
	}
	e := q.NoLevelsEntries()
	if len(e) != 1 {
		t.Fatalf("%d entries", len(e))
	}
	if v, ok := e[0].LevelPxBytes(); !ok || string(v) != "0.00001000" {
		t.Errorf("LevelPxBytes = %q, %v", v, ok)
	}
	if _, ok := e[0].LevelSizeBytes(); !ok {
		t.Error("LevelSizeBytes missing")
	}
	// Strings and enums need none.
	for _, name := range []string{"SymbolBytes", "SideBytes"} {
		if _, ok := reflect.TypeOf(q).MethodByName(name); ok {
			t.Errorf("unexpected method %s", name)
		}
	}
}
`, options{bytes: true})
}

func TestGenerateRawGetterClash(t *testing.T) {
	xml := strings.Replace(rawGettersXML, `<field name="Symbol" required="N"/>`, `<field name="Symbol" required="N"/><field name="BidPxBytes" required="N"/>`, 1)
	xml = strings.Replace(xml, `<field number="55" name="Symbol" type="STRING"/>`, `<field number="55" name="Symbol" type="STRING"/><field number="5003" name="BidPxBytes" type="STRING"/>`, 1)
	dict, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	if err := generateWith(dict, "raw", t.TempDir(), options{bytes: true}); err == nil {
		t.Error("expected a clash between BidPxBytes and BidPx's raw getter")
	}
	if err := generateWith(dict, "raw", t.TempDir(), options{}); err != nil {
		t.Errorf("without --bytes: %v", err)
	}
}

// FuzzGenerate checks that the generator neither panics nor writes Go that
// does not parse, for any dictionary xmldict.Load accepts: it returns an
// error for the ones it cannot name safely.
func FuzzGenerate(f *testing.F) {
	for _, name := range []string{"FIX40", "FIX44", "FIXT11", "OrchestraFIXLatest"} {
		data, err := os.ReadFile("../../../fix/dictionary/testdata/" + name + ".xml")
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data, false, false)
	}
	f.Add([]byte(`<fix type="FIX" major="4" minor="4"><header><field name="MsgType" required="Y"/></header><trailer/><messages><message name="M" msgtype="M"><component name="C"/><group name="NoG" required="Y"><field name="A"/><group name="NoH"><field name="B"/><field name="Len"/><field name="Data"/></group></group></message></messages><components><component name="C"><field name="A"/><component name="D"/></component><component name="D"><field name="B" required="Y"/></component></components><fields><field number="35" name="MsgType" type="STRING"/><field number="1" name="A" type="STRING"/><field number="2" name="B" type="INT"><value enum="1" description="ONE"/><value enum="2" description="One"/></field><field number="3" name="NoG" type="NUMINGROUP"/><field number="4" name="NoH" type="NUMINGROUP"/><field number="5" name="Len" type="LENGTH"/><field number="6" name="Data" type="DATA"/></fields></fix>`), true, true)
	f.Fuzz(func(t *testing.T, data []byte, dispatch, raw bool) {
		dict, err := xmldict.Load(bytes.NewReader(data))
		if err != nil {
			return
		}
		out := t.TempDir()
		hang := time.AfterFunc(10*time.Second, func() { panic("generate hangs") })
		err = generateWith(dict, "gen", out, options{dispatch: dispatch, bytes: raw})
		hang.Stop()
		// The generator refuses dictionaries whose names it cannot turn into
		// distinct Go identifiers and files; any other error is code that
		// does not format or parse, which generateWith checks itself.
		if err != nil && !rejected(err) {
			t.Fatalf("generated code does not format or parse: %v", err)
		}
	})
}

// TestMessageTags checks that generated readers split groups as validation
// does: the tags they know a message holds are those Defines reports, with
// the FIXT 1.1 header and trailer's for an application dictionary, which has
// none of its own; those of the header's and trailer's groups are those
// DefinesAny reports.
func TestMessageTags(t *testing.T) {
	dicts := map[string]*dictionary.Dict{}
	for _, name := range []string{"FIX40", "FIX44", "FIX50SP2", "FIXT11", "OrchestraFIXLatest"} {
		dict, err := xmldict.LoadFile("../../../fix/dictionary/testdata/" + name + ".xml")
		if err != nil {
			t.Fatal(err)
		}
		dicts[name] = dict
	}
	// EncodedText(355) is declared, its LENGTH, EncodedTextLen(354), is
	// not: messages allow it all the same.
	dict, err := xmldict.Load(strings.NewReader(`<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header>
  <trailer/>
  <messages>
    <message name="M" msgtype="M" msgcat="app">
      <group name="NoG" required="N"><field name="A" required="N"/><field name="EncodedText" required="N"/></group>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="500" name="NoG" type="NUMINGROUP"/>
    <field number="100" name="A" type="STRING"/>
    <field number="355" name="EncodedText" type="DATA"/>
  </fields>
</fix>`))
	if err != nil {
		t.Fatal(err)
	}
	dicts["undeclared LENGTH"] = dict
	if !slices.Contains(messageTags(dict, dict.Messages["M"]), 354) {
		t.Errorf("undeclared LENGTH: 354 missing from M's tags")
	}

	for name, dict := range dicts {
		envelope := envelopeTags(dict)
		for _, md := range dict.Messages {
			tags := messageTags(dict, md)
			for tag := range 10000 {
				if dict.Defines(md.MsgType, tag) != slices.Contains(tags, tag) && !(len(dict.Header) == 0 && slices.Contains(fixtEnvelope, tag)) {
					t.Errorf("%s %s: tag %d: Defines and messageTags disagree", name, md.MsgType, tag)
				}
			}
		}
		for tag := range 10000 {
			if dict.DefinesAny(tag) != slices.Contains(envelope, tag) {
				t.Errorf("%s: tag %d: DefinesAny and envelopeTags disagree", name, tag)
			}
		}
	}
}

// rejected reports whether err is the generator refusing a dictionary, not
// failing to write valid Go for it.
func rejected(err error) bool {
	for _, why := range []string{
		"must be an ASCII letter followed by letters and digits", // checkNames
		"which must follow its LENGTH field",                     // checkNames
		"define its LENGTH field",                                // withLengthFields
		"written twice",                                          // generateWith
		"rename the dictionary entries it comes from",            // checkGenerated, a setter shadowing fix.Builder's
	} {
		if strings.Contains(err.Error(), why) {
			return true
		}
	}
	return false
}

// TestGenerateLengthOnlyGroup covers a group that lists only a LENGTH field
// whose DATA partner exists globally: the entry builder must still offer the
// DATA setter, and the entry it builds must pass strict validation and read
// back, as the dictionary and the readers accept the pair.
func TestGenerateLengthOnlyGroup(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4"><header><field name="MsgType" required="Y"/></header><trailer/><messages><message name="M" msgtype="M"><group name="NoG" required="N"><field name="RawDataLength" required="N"/>DATAREF</group></message></messages><fields><field number="35" name="MsgType" type="STRING"/><field number="500" name="NoG" type="NUMINGROUP"/><field number="95" name="RawDataLength" type="LENGTH"/><field number="96" name="RawData" type="DATA"/></fields></fix>`
	for name, ref := range map[string]string{
		"declared-data-control": `<field name="RawData" required="N"/>`,
		"implicit-data":         "",
	} {
		t.Run(name, func(t *testing.T) {
			runGenerated(t, strings.ReplaceAll(xml, "DATAREF", ref), "lengthonly", `package lengthonly

import (
	"bytes"
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
)

func TestWritableGroup(t *testing.T) {
	// The wire form the dictionary and the reader accept.
	rawControl := fix.NewBuilder("FIX.4.4", "M").AppendInt(500, 1).AppendInt(95, 3).AppendString(96, "a\x01b").Build()
	var control fix.Message
	if err := fix.ParseInto(&control, rawControl, Dictionary().Profile()); err != nil {
		t.Fatal(err)
	}
	if err := Dictionary().ValidateWith(&control, dictionary.CheckAll); err != nil {
		t.Fatalf("valid pair rejected: %v", err)
	}
	if got := len(WrapM(&control).NoGEntries()); got != 1 {
		t.Fatalf("control groups = %d", got)
	}
	if v, ok := WrapM(&control).NoGEntries()[0].RawData(); !ok || string(v) != "a\x01b" {
		t.Errorf("control RawData = %q, %v", v, ok)
	}

	raw := BuildM("FIX.4.4").AddNoGEntry(NewNoGEntry().SetRawData([]byte("a\x01b"))).Build()
	if !bytes.Contains(raw, []byte("\x01500=1\x0195=3\x0196=a\x01b\x01")) {
		t.Errorf("unexpected layout %q", raw)
	}
	var m fix.Message
	if err := fix.ParseInto(&m, raw, Dictionary().Profile()); err != nil {
		t.Fatalf("ParseInto: %v", err)
	}
	if err := Dictionary().ValidateWith(&m, dictionary.CheckAll); err != nil {
		t.Fatalf("built message rejected: %v", err)
	}
	if n := len(WrapM(&m).NoGEntries()); n != 1 {
		t.Fatalf("groups = %d, raw = %q", n, raw)
	}
	if v, ok := WrapM(&m).NoGEntries()[0].RawData(); !ok || string(v) != "a\x01b" {
		t.Errorf("built RawData = %q, %v", v, ok)
	}
}
`)
		})
	}
}
