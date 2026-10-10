package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/dave/jennifer/jen"
	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
)

const fix2Pkg = "github.com/sylr/fix2"

// structuralTags are managed by the parser/builder and skipped in generated code.
var structuralTags = map[int]bool{
	8: true, 9: true, 10: true, 35: true,
}

// enumTypeNames maps a field name to the name of its enum type, set by
// generate (see resolveEnumTypeNames).
var enumTypeNames map[string]string

// enumType returns the name of the enum type of field fieldName.
func enumType(fieldName string) string {
	if n, ok := enumTypeNames[fieldName]; ok {
		return n
	}
	return fieldName
}

// resolveEnumTypeNames names each enum type after its field, adding an
// "Enum" suffix when a generated type already uses the name: FIX 5.0 has
// both a SecurityStatus message and a SecurityStatus (965) field.
func resolveEnumTypeNames(dict *dictionary.Dict) map[string]string {
	taken := map[string]bool{"Dictionary": true} // see writeDictionary
	for _, md := range dict.Messages {
		for _, n := range []string{md.Name, md.Name + "Builder", "Wrap" + md.Name, "Build" + md.Name} {
			taken[n] = true
		}
	}
	for name := range mergeAllGroups(dict) {
		for _, n := range []string{name + "Entry", name + "EntryBuilder", "New" + name + "Entry"} {
			taken[n] = true
		}
	}
	for _, fd := range dict.Fields {
		taken["Tag"+fd.Name] = true
	}
	// Assign in name order, reserving each name, so renames are stable and
	// cannot collide with each other (Foo and FooEnum both needing FooEnum).
	var enumFields []string
	for _, fd := range dict.Fields {
		if len(fd.Enums) > 0 {
			enumFields = append(enumFields, fd.Name)
		}
	}
	slices.Sort(enumFields)
	names := map[string]string{}
	for _, field := range enumFields {
		name := field
		for taken[name] {
			name += "Enum"
		}
		taken[name] = true
		names[field] = name
	}
	return names
}

// builderMethods are the fix2.Builder methods generated builders call on
// their embedded Builder: a generated setter with one of these names would
// shadow it.
var builderMethods = []string{"Set", "SetString", "SetInt", "SetFloat", "SetBool", "SetData", "SetGroups", "SetSections", "Append", "Build", "BuildWith"}

// checkNames rejects dictionaries the generator cannot name safely: every
// field, group and message name must be an exported Go identifier made of
// ASCII letters and digits (as all FIX names are), so that the unexported
// identifiers derived from them, joined with "_", never clash; and a group
// cannot start with a DATA field, which FIX requires to follow its LENGTH
// field while the delimiter must come first.
func checkNames(dict *dictionary.Dict) error {
	valid := func(name string) bool {
		if name == "" || name[0] < 'A' || name[0] > 'Z' {
			return false
		}
		for _, c := range name {
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
				return false
			}
		}
		return true
	}
	for _, fd := range dict.Fields {
		if !valid(fd.Name) {
			return fmt.Errorf("field %q: name must be an ASCII letter followed by letters and digits, starting upper case", fd.Name)
		}
	}
	for _, md := range dict.Messages {
		if !valid(md.Name) {
			return fmt.Errorf("message %q: name must be an ASCII letter followed by letters and digits, starting upper case", md.Name)
		}
	}
	var checkGroups func(gs []dictionary.GroupDef) error
	checkGroups = func(gs []dictionary.GroupDef) error {
		for _, gd := range gs {
			if fix2.DataLengthTag(gd.DelimTag) != 0 {
				return fmt.Errorf("group %s starts with DATA field %s, which must follow its LENGTH field", gd.Name, dict.Fields[gd.DelimTag].Name)
			}
			if err := checkGroups(gd.Groups); err != nil {
				return err
			}
		}
		return nil
	}
	if err := checkGroups(slices.Concat(dict.HeaderGroups, dict.TrailerGroups)); err != nil {
		return err
	}
	for _, mt := range slices.Sorted(maps.Keys(dict.Messages)) {
		if err := checkGroups(dict.Messages[mt].Groups); err != nil {
			return err
		}
	}
	return nil
}

// checkGenerated parses the generated files and reports any top-level
// identifier, or method of the same type, declared twice: dictionary names
// can combine into the same Go identifier in many ways (message FooBuilder
// and message Foo's builder, enum descriptions BUY and Buy, ...), and an
// explicit error beats generated code that does not compile.
func checkGenerated(outDir string, written map[string]bool) error {
	fset := token.NewFileSet()
	declared := map[string]string{} // identifier -> file declaring it
	declare := func(id, file string) error {
		if prev, ok := declared[id]; ok {
			return fmt.Errorf("generated identifier %s is declared in both %s and %s: rename the dictionary entries it comes from", id, prev, file)
		}
		declared[id] = file
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(written)) {
		file, err := parser.ParseFile(fset, filepath.Join(outDir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				id := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) > 0 {
					t := d.Recv.List[0].Type
					if star, ok := t.(*ast.StarExpr); ok {
						t = star.X
					}
					if ident, ok := t.(*ast.Ident); ok {
						id = ident.Name + "." + id
					}
				}
				if err := declare(id, name); err != nil {
					return err
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch sp := spec.(type) {
					case *ast.TypeSpec:
						if err := declare(sp.Name.Name, name); err != nil {
							return err
						}
					case *ast.ValueSpec:
						for _, n := range sp.Names {
							if err := declare(n.Name, name); err != nil {
								return err
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// generatedHeader is the first line of every generated file. removeStale
// relies on it to tell generated files from hand-written ones.
const generatedHeader = "Code generated by fixgen; DO NOT EDIT."

// generate writes a complete typed Go package from a dictionary, then removes
// files left over from a previous run that it no longer emits.
func generate(dict *dictionary.Dict, pkg, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	written := map[string]bool{"doc.go": true, "fields.go": true, "dictionary.go": true}
	if err := checkNames(dict); err != nil {
		return err
	}
	enumTypeNames = resolveEnumTypeNames(dict)

	if err := writeDoc(pkg, outDir); err != nil {
		return fmt.Errorf("doc.go: %w", err)
	}
	if err := writeFields(dict, pkg, outDir); err != nil {
		return fmt.Errorf("fields.go: %w", err)
	}
	if err := writeDictionary(dict, pkg, outDir); err != nil {
		return fmt.Errorf("dictionary.go: %w", err)
	}

	// Merge group definitions across all messages and generate shared group files.
	mergedGroups := mergeAllGroups(dict)
	groupNames := make([]string, 0, len(mergedGroups))
	for name := range mergedGroups {
		groupNames = append(groupNames, name)
	}
	sort.Strings(groupNames)

	for _, name := range groupNames {
		mg := mergedGroups[name]
		fileName := groupFileName(name)
		if written[fileName] {
			return fmt.Errorf("%s: written twice, two groups map to the same file", fileName)
		}
		if err := writeGroupFile(dict, mg, mergedGroups, pkg, outDir); err != nil {
			return fmt.Errorf("%s: %w", fileName, err)
		}
		written[fileName] = true
	}

	// Sort message types for deterministic output.
	msgTypes := make([]string, 0, len(dict.Messages))
	for mt := range dict.Messages {
		msgTypes = append(msgTypes, mt)
	}
	sort.Strings(msgTypes)

	for _, mt := range msgTypes {
		md := dict.Messages[mt]
		fileName := msgFileName(md.Name)
		if written[fileName] {
			return fmt.Errorf("%s: written twice, two messages are named %s", fileName, md.Name)
		}
		if err := writeMessage(dict, md, mergedGroups, pkg, outDir); err != nil {
			return fmt.Errorf("%s: %w", fileName, err)
		}
		written[fileName] = true
	}

	if err := checkGenerated(outDir, written); err != nil {
		return err
	}
	return removeStale(outDir, written)
}

func groupFileName(groupName string) string { return "group_" + toSnakeCase(groupName) + ".go" }

func msgFileName(msgName string) string { return "msg_" + toSnakeCase(msgName) + ".go" }

// removeStale deletes the .go files in outDir that carry generatedHeader but
// were not written by this run, e.g. the file of a group or message that was
// removed from the dictionary. Hand-written files are never touched.
func removeStale(outDir string, written map[string]bool) error {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return err
	}
	header := []byte("// " + generatedHeader)
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || filepath.Ext(name) != ".go" || written[name] {
			continue
		}
		path := filepath.Join(outDir, name)
		generated, err := hasPrefix(path, header)
		if err != nil {
			return err
		}
		if generated {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasPrefix reports whether the file at path starts with prefix.
func hasPrefix(path string, prefix []byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, len(prefix))
	if _, err := io.ReadFull(f, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil // shorter than the header
		}
		return false, err
	}
	return bytes.Equal(buf, prefix), nil
}

// ---------------------------------------------------------------------------
// doc.go
// ---------------------------------------------------------------------------

func writeDoc(pkg, outDir string) error {
	f := jen.NewFile(pkg)
	f.HeaderComment(generatedHeader)
	f.PackageComment(fmt.Sprintf("Package %s provides typed FIX message readers and builders, and the", pkg))
	f.PackageComment("dictionary they were generated from, for validating messages (see Dictionary).")
	return f.Save(filepath.Join(outDir, "doc.go"))
}

// ---------------------------------------------------------------------------
// fields.go  — tag constants, enum types, time helpers
// ---------------------------------------------------------------------------

func writeFields(dict *dictionary.Dict, pkg, outDir string) error {
	f := jen.NewFile(pkg)
	f.HeaderComment(generatedHeader)

	// Collect and sort fields by tag number.
	type fieldEntry struct {
		tag int
		def *dictionary.FieldDef
	}
	var fields []fieldEntry
	for tag, fd := range dict.Fields {
		fields = append(fields, fieldEntry{tag, fd})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].tag < fields[j].tag })

	// --- Tag constants ---
	var consts []jen.Code
	for _, fe := range fields {
		consts = append(consts, jen.Id("Tag"+fe.def.Name).Op("=").Lit(fe.tag))
	}
	f.Const().Defs(consts...)

	// --- Enum types ---
	for _, fe := range fields {
		if len(fe.def.Enums) == 0 {
			continue
		}
		typeName := enumType(fe.def.Name)
		f.Type().Id(typeName).String()

		// Sort enum values for deterministic output.
		type enumEntry struct {
			value string
			desc  string
		}
		var enums []enumEntry
		for v, d := range fe.def.Enums {
			enums = append(enums, enumEntry{v, d})
		}
		sort.Slice(enums, func(i, j int) bool { return enums[i].value < enums[j].value })

		var enumConsts []jen.Code
		for _, e := range enums {
			constName := fe.def.Name + sanitizePascal(e.desc)
			enumConsts = append(enumConsts, jen.Id(constName).Id(typeName).Op("=").Lit(e.value))
		}
		f.Const().Defs(enumConsts...)
	}

	// Standard header and trailer tags, registered by every message builder
	// with SetSections. Group tags are left out: header groups are written
	// by their own writer.
	var inHeader []int // header tags, which the trailer must not repeat
	writeSectionVar := func(name, doc string, refs []dictionary.FieldRef, groups []dictionary.GroupDef) {
		inGroups, counters := map[int]bool{}, map[int]bool{}
		collectGroupTags(dict, groups, inGroups, counters, false)
		var list []int
		add := func(tag int) {
			// A pair split between header and trailer stays in the header.
			if dict.Fields[tag] != nil && !slices.Contains(list, tag) && !slices.Contains(inHeader, tag) {
				list = append(list, tag)
			}
		}
		for _, ref := range refs {
			if structuralTags[ref.Tag] || inGroups[ref.Tag] || counters[ref.Tag] || dict.Fields[ref.Tag] == nil {
				continue
			}
			// Keep both fields of a LENGTH/DATA pair in the section, even
			// if the dictionary lists one of them elsewhere, so SetData's
			// adjacent pair is not split between sections.
			if l := fix2.DataLengthTag(ref.Tag); l != 0 {
				add(l)
			}
			add(ref.Tag)
			if d := fix2.DataTag(ref.Tag); d != 0 {
				add(d)
			}
		}
		var tags []jen.Code
		for _, tag := range list {
			tags = append(tags, tagID(dict.Fields[tag].Name))
		}
		if inHeader == nil {
			inHeader = list
		}
		f.Comment(doc)
		f.Var().Id(name).Op("=").Index().Int().Values(tags...)
	}
	writeSectionVar("standardHeader", "standardHeader lists the standard header tags, written first.", dict.Header, dict.HeaderGroups)
	writeSectionVar("standardTrailer", "standardTrailer lists the standard trailer tags, written last.", dict.Trailer, dict.TrailerGroups)

	if len(dict.HeaderGroups)+len(dict.TrailerGroups) > 0 || slices.ContainsFunc(slices.Collect(maps.Values(dict.Messages)), func(md *dictionary.MessageDef) bool { return len(md.Groups) > 0 }) {
		f.Comment("groupLayout is the order of a group entry's tags in one message's")
		f.Comment("definition, with the layouts of its nested groups by counter tag: the same")
		f.Comment("group can order its fields differently in other messages.")
		f.Type().Id("groupLayout").Struct(
			jen.Id("tags").Index().Int(),
			jen.Id("nested").Map(jen.Int()).Op("*").Id("groupLayout"),
		)
	}

	return f.Save(filepath.Join(outDir, "fields.go"))
}

// ---------------------------------------------------------------------------
// group merging — collect all groups across messages, union their fields
// ---------------------------------------------------------------------------

// mergeAllGroups collects every group definition (at any nesting depth) from
// all messages and produces a single merged GroupDef per unique group name.
// Fields are unioned: delimiter first, then remaining tags sorted.
func mergeAllGroups(dict *dictionary.Dict) map[string]dictionary.GroupDef {
	// Visit messages in MsgType order: the merged definitions, and so the
	// generated code, must not depend on map iteration order.
	byName := map[string][]dictionary.GroupDef{}
	collectGroupDefsRecursive(dict.HeaderGroups, byName)
	collectGroupDefsRecursive(dict.TrailerGroups, byName)
	for _, mt := range slices.Sorted(maps.Keys(dict.Messages)) {
		collectGroupDefsRecursive(dict.Messages[mt].Groups, byName)
	}
	merged := make(map[string]dictionary.GroupDef, len(byName))
	for _, defs := range byName {
		mg := mergeGroupDefs(defs)
		merged[mg.Name] = mg
	}
	return merged
}

func collectGroupDefsRecursive(groups []dictionary.GroupDef, byName map[string][]dictionary.GroupDef) {
	for _, gd := range groups {
		byName[gd.Name] = append(byName[gd.Name], gd)
		collectGroupDefsRecursive(gd.Groups, byName)
	}
}

func mergeGroupDefs(defs []dictionary.GroupDef) dictionary.GroupDef {
	mg := dictionary.GroupDef{
		Name:       defs[0].Name,
		CounterTag: defs[0].CounterTag,
		DelimTag:   defs[0].DelimTag,
	}

	// Union child fields: delimiter first, then remaining sorted by tag. When
	// entries start with a nested group (e.g. FIX 5.0 SP2 NoAsgnReqs starting
	// with Parties), the delimiter is that group's counter: it belongs to the
	// nested group, not to the entry's own fields.
	fieldSet := map[int]bool{}
	nestedCounter := map[int]bool{}
	for _, d := range defs {
		for _, fr := range d.Fields {
			fieldSet[fr.Tag] = true
		}
		for _, g := range d.Groups {
			nestedCounter[g.CounterTag] = true
		}
	}
	if !nestedCounter[mg.DelimTag] {
		mg.Fields = append(mg.Fields, dictionary.FieldRef{Tag: mg.DelimTag})
	}
	delete(fieldSet, mg.DelimTag)
	var rest []int
	for tag := range fieldSet {
		rest = append(rest, tag)
	}
	sort.Ints(rest)
	for _, tag := range rest {
		mg.Fields = append(mg.Fields, dictionary.FieldRef{Tag: tag})
	}

	// Union nested groups (recursively merge by name).
	nestedByName := map[string][]dictionary.GroupDef{}
	for _, d := range defs {
		for _, nested := range d.Groups {
			nestedByName[nested.Name] = append(nestedByName[nested.Name], nested)
		}
	}
	var nestedNames []string
	for name := range nestedByName {
		nestedNames = append(nestedNames, name)
	}
	sort.Strings(nestedNames)
	for _, name := range nestedNames {
		mg.Groups = append(mg.Groups, mergeGroupDefs(nestedByName[name]))
	}

	return mg
}

// ---------------------------------------------------------------------------
// group_*.go — shared entry reader, builder, child tags per group
// ---------------------------------------------------------------------------

func writeGroupFile(dict *dictionary.Dict, mg dictionary.GroupDef, allMerged map[string]dictionary.GroupDef, pkg, outDir string) error {
	f := jen.NewFile(pkg)
	f.HeaderComment(generatedHeader)

	entryName := mg.Name + "Entry"

	// --- Entry struct ---
	// spec is the entry's group spec in the message it was read from, so
	// nested groups are extracted with that message's definitions.
	f.Type().Id(entryName).Struct(
		jen.Id("grp").Qual(fix2Pkg, "Group"),
		jen.Id("spec").Op("*").Qual(fix2Pkg, "GroupSpec"),
	)

	// --- Getters for child fields ---
	// With nested groups, a field is read skipping their entries, which can
	// hold the same tag.
	if len(mg.Groups) > 0 {
		f.Comment("get returns the value of the entry's field with the given tag, outside its")
		f.Comment("nested groups.")
		f.Func().Params(jen.Id("e").Op("*").Id(entryName)).Id("get").Params(jen.Id("tag").Int()).Index().Byte().Block(
			jen.If(jen.Id("e").Dot("spec").Op("==").Nil()).Block(
				jen.Return(jen.Id("e").Dot("grp").Dot("Get").Call(jen.Id("tag"))),
			),
			jen.Return(jen.Id("e").Dot("grp").Dot("GetTopLevel").Call(jen.Id("tag"), jen.Id("e").Dot("spec").Dot("Nested").Op("..."))),
		)
	}
	scoped := len(mg.Groups) > 0
	for _, fr := range mg.Fields {
		fd := dict.Fields[fr.Tag]
		if fd == nil {
			continue
		}
		info := lookupType(fd.Type)
		hasEnums := len(fd.Enums) > 0
		addEntryGetter(f, entryName, fr.Tag, fd.Name, fd.Type, info, hasEnums, scoped)
	}

	// --- Nested groups: counter getter + iteration method on parent entry ---
	for _, nestedMg := range mg.Groups {
		globalNested := allMerged[nestedMg.Name]
		addEntryGetter(f, entryName, globalNested.CounterTag, globalNested.Name, "NUMINGROUP", lookupType("NUMINGROUP"), false, true)
		writeIterationMethod(f, entryName, "e", "grp", globalNested.Name, jen.Id("e").Dot("spec").Dot("Child").Call(tagID(globalNested.Name)), jen.Id("e").Dot("spec").Dot("Nested"))
	}

	// --- Entry builder struct ---
	entryBuilderName := mg.Name + "EntryBuilder"
	structFields := []jen.Code{
		jen.Id("fields").Index().Qual(fix2Pkg, "Field"),
	}
	for _, nestedMg := range mg.Groups {
		structFields = append(structFields,
			jen.Id(entriesField(nestedMg.Name)).Index().Op("*").Id(nestedMg.Name+"EntryBuilder"),
		)
	}
	f.Type().Id(entryBuilderName).Struct(structFields...)

	// --- Constructor ---
	f.Func().Id("New" + mg.Name + "Entry").Params().Op("*").Id(entryBuilderName).Block(
		jen.Return(jen.Op("&").Id(entryBuilderName).Values()),
	)

	// --- Setters for child fields ---
	for _, fr := range mg.Fields {
		fd := dict.Fields[fr.Tag]
		if fd == nil {
			continue
		}
		if isDataLength(dict, fr.Tag) {
			continue // computed by the DATA setter
		}
		if l := fix2.DataLengthTag(fr.Tag); l != 0 && dict.Fields[l] != nil {
			addEntryDataSetter(f, entryBuilderName, fd.Name, dict.Fields[l].Name)
			continue
		}
		info := lookupType(fd.Type)
		hasEnums := len(fd.Enums) > 0
		addEntryBuilderSetter(f, entryBuilderName, fr.Tag, fd.Name, fd.Type, info, hasEnums)
	}

	// --- Nested group Add methods on entry builder ---
	for _, nestedMg := range mg.Groups {
		globalNested := allMerged[nestedMg.Name]
		writeEntryGroupAddMethod(f, dict, entryBuilderName, globalNested)
	}

	writeEntryAppendTo(f, entryBuilderName, mg)

	return f.Save(filepath.Join(outDir, groupFileName(mg.Name)))
}

// ---------------------------------------------------------------------------
// per-message file — reader + builder (group types are now in group_*.go)
// ---------------------------------------------------------------------------

func writeMessage(dict *dictionary.Dict, md *dictionary.MessageDef, mergedGroups map[string]dictionary.GroupDef, pkg, outDir string) error {
	f := jen.NewFile(pkg)
	f.HeaderComment(generatedHeader)

	name := md.Name

	// The message's groups, including those of the standard header and
	// trailer that every message carries.
	groups := slices.Clone(dict.HeaderGroups)
	for _, gd := range slices.Concat(md.Groups, dict.TrailerGroups) {
		if !slices.ContainsFunc(groups, func(g dictionary.GroupDef) bool { return g.Name == gd.Name }) {
			groups = append(groups, gd)
		}
	}
	headerGroups := groups[:len(dict.HeaderGroups)]
	bodyGroups := groups[len(dict.HeaderGroups):]

	// Build sets of group child tags and counter tags from this message's groups.
	// Use the message's own definitions (not merged) so we only exclude fields
	// that actually belong to groups in THIS message.
	groupChildTags := map[int]bool{}
	counterTags := map[int]bool{}
	collectGroupTags(dict, groups, groupChildTags, counterTags, false)

	// A tag that occurs more often in the message's fields (group contents
	// included) than inside its groups is also a direct field of the
	// message, e.g. a header group child reused in the body: it keeps its
	// scalar getter and setter.
	inGroups, implicit := map[int]int{}, map[int]int{}
	countGroupTags(dict, groups, inGroups, implicit)
	occurrences := map[int]int{}
	for _, refs := range [][]dictionary.FieldRef{dict.Header, md.Fields, dict.Trailer} {
		for _, ref := range refs {
			occurrences[ref.Tag]++
		}
	}
	// Group fields are among the refs, their implicit DATA/LENGTH partners
	// are not.
	for tag, n := range implicit {
		occurrences[tag] += n
	}
	// Such a tag is read skipping the groups, or its getter could return a
	// group entry's value.
	topLevel := map[int]bool{}
	for tag := range groupChildTags {
		if occurrences[tag] > inGroups[tag] {
			delete(groupChildTags, tag)
			topLevel[tag] = true
		}
	}

	// Collect fields for this message: header, message and trailer fields, skip structural.
	type fieldInfo struct {
		tag      int
		name     string
		fixType  string
		goInfo   goTypeInfo
		hasEnums bool
	}
	var allFields []fieldInfo
	seen := map[int]bool{}

	addFields := func(refs []dictionary.FieldRef) {
		for _, ref := range refs {
			if structuralTags[ref.Tag] || seen[ref.Tag] {
				continue
			}
			fd := dict.Fields[ref.Tag]
			if fd == nil {
				continue
			}
			seen[ref.Tag] = true
			allFields = append(allFields, fieldInfo{
				tag:      ref.Tag,
				name:     fd.Name,
				fixType:  fd.Type,
				goInfo:   lookupType(fd.Type),
				hasEnums: len(fd.Enums) > 0,
			})
		}
	}
	addFields(dict.Header)
	addFields(md.Fields)
	addFields(dict.Trailer)

	// --- Reader struct ---
	f.Type().Id(name).Struct(
		jen.Id("msg").Op("*").Qual(fix2Pkg, "Message"),
	)

	// Wrap constructor
	f.Func().Id("Wrap" + name).Params(
		jen.Id("msg").Op("*").Qual(fix2Pkg, "Message"),
	).Op("*").Id(name).Block(
		jen.Return(jen.Op("&").Id(name).Values(jen.Dict{
			jen.Id("msg"): jen.Id("msg"),
		})),
	)

	// specsVar lists the message's group specs: reading a group, or a field
	// outside the groups, skips the others' entries.
	specsVar := unexport(name) + "_specs"
	if len(groups) > 0 {
		var specs []jen.Code
		for _, gd := range groups {
			specs = append(specs, jen.Id(unexport(name)+"_"+gd.Name+"_spec"))
		}
		f.Var().Id(specsVar).Op("=").Index().Op("*").Qual(fix2Pkg, "GroupSpec").Values(specs...)
	}

	// Getters — skip group child tags
	for _, fi := range allFields {
		if groupChildTags[fi.tag] {
			continue
		}
		var get *jen.Statement
		if topLevel[fi.tag] {
			get = jen.Id("m").Dot("msg").Dot("GetTopLevel").Call(tagID(fi.name), jen.Id(specsVar).Op("..."))
		}
		addGetter(f, name, fi.tag, fi.name, fi.fixType, fi.goInfo, fi.hasEnums, get)
	}

	// --- Group iteration methods on message reader ---
	// Specs come from this message's own group definitions: the same group
	// can hold different fields in other messages.
	for _, gd := range groups {
		v := unexport(name) + "_" + gd.Name + "_spec"
		f.Var().Id(v).Op("=").Add(specLiteral(dict, gd))
		f.Var().Id(unexport(name) + "_" + gd.Name + "_layout").Op("=").Add(layoutLiteral(dict, gd))
		writeIterationMethod(f, name, "m", "msg", gd.Name, jen.Id(v), jen.Id(specsVar))
	}

	// --- Builder struct with counter fields ---
	builderName := name + "Builder"
	builderFields := []jen.Code{
		jen.Op("*").Qual(fix2Pkg, "Builder"),
	}
	groupsType := unexport(name) + "_groups"
	if len(groups) > 0 {
		// Group entries live in state allocated by the constructor: a copy
		// of the builder shares them, as it shares the embedded Builder, and
		// the writer registered with SetGroups sees every Add.
		var groupFields []jen.Code
		for _, gd := range groups {
			groupFields = append(groupFields, jen.Id(entriesField(gd.Name)).Index().Op("*").Id(gd.Name+"EntryBuilder"))
		}
		f.Type().Id(groupsType).Struct(groupFields...)
		builderFields = append(builderFields, jen.Id("groups").Op("*").Id(groupsType))
	}
	f.Comment(builderName + " builds a " + name + " message. A copy shares its fields and")
	f.Comment("groups with the original.")
	f.Type().Id(builderName).Struct(builderFields...)

	// Build constructor
	ctorFields := jen.Dict{
		jen.Id("Builder"): jen.Qual(fix2Pkg, "NewBuilder").Call(jen.Id("beginString"), jen.Lit(md.MsgType)),
	}
	if len(groups) > 0 {
		ctorFields[jen.Id("groups")] = jen.Op("&").Id(groupsType).Values()
	}
	ctor := []jen.Code{jen.Id("b").Op(":=").Op("&").Id(builderName).Values(ctorFields)}
	// Every Build call, including the embedded Builder's at the end of a
	// fluent chain, writes the header first and the trailer last, and
	// serializes the groups.
	headerWriter := jen.Code(jen.Nil())
	if len(headerGroups) > 0 {
		headerWriter = jen.Id("b").Dot("groups").Dot("appendHeaderTo")
	}
	ctor = append(ctor, jen.Id("b").Dot("SetSections").Call(jen.Id("standardHeader"), jen.Id("standardTrailer"), headerWriter))
	if len(groups) > 0 {
		bodyWriter := jen.Code(jen.Nil())
		if len(bodyGroups) > 0 {
			bodyWriter = jen.Id("b").Dot("groups").Dot("appendTo")
		}
		args := []jen.Code{bodyWriter}
		for _, gd := range groups {
			args = append(args, tagID(gd.Name))
		}
		ctor = append(ctor, jen.Id("b").Dot("SetGroups").Call(args...))
	}
	ctor = append(ctor, jen.Return(jen.Id("b")))
	f.Func().Id("Build" + name).Params(
		jen.Id("beginString").String(),
	).Op("*").Id(builderName).Block(ctor...)

	// Setters — skip group child tags and counter tags (for builder)
	for _, fi := range allFields {
		if groupChildTags[fi.tag] || counterTags[fi.tag] || isDataLength(dict, fi.tag) {
			continue
		}
		// The setter must not shadow a fix2.Builder method the generated
		// code calls on the embedded builder.
		if slices.Contains(builderMethods, "Set"+fi.name) {
			return fmt.Errorf("field %s: its setter Set%s would shadow fix2.Builder.Set%s", fi.name, fi.name, fi.name)
		}
		if l := fix2.DataLengthTag(fi.tag); l != 0 && dict.Fields[l] != nil {
			addDataSetter(f, builderName, fi.name, dict.Fields[l].Name)
			continue
		}
		addSetter(f, builderName, fi.tag, fi.name, fi.fixType, fi.goInfo, fi.hasEnums)
	}

	// --- Group Add methods on message builder ---
	for _, gd := range groups {
		mg := mergedGroups[gd.Name]
		writeGroupAddMethod(f, dict, builderName, mg)
	}

	// --- group writers: registered by the constructor ---
	writeGroupWriter := func(method, doc string, gs []dictionary.GroupDef) {
		var body []jen.Code
		for _, gd := range gs {
			name := gd.Name
			body = append(body, groupBlock("dst", func() *jen.Statement { return jen.Id("b").Dot(entriesField(name)) }, gd, jen.Id(unexport(md.Name)+"_"+name+"_layout")))
		}
		body = append(body, jen.Return(jen.Id("dst")))
		f.Comment(doc)
		f.Func().Params(jen.Id("b").Op("*").Id(groupsType)).Id(method).Params(
			jen.Id("dst").Index().Qual(fix2Pkg, "Field"),
		).Index().Qual(fix2Pkg, "Field").Block(body...)
	}
	if len(headerGroups) > 0 {
		writeGroupWriter("appendHeaderTo", "appendHeaderTo appends the standard header's repeating groups to dst.", headerGroups)
	}
	if len(bodyGroups) > 0 {
		writeGroupWriter("appendTo", "appendTo appends the message's other repeating groups to dst.", bodyGroups)
	}

	return f.Save(filepath.Join(outDir, msgFileName(name)))
}

// collectGroupTags recursively collects child tags and counter tags from groups.
// Nested counter tags are added to childTags so they are skipped from the base
// message getters/setters (they only belong on the parent group entry).
// countGroupTags counts how many times each tag occurs inside groups: their
// counters, their fields with their implicit DATA/LENGTH partners and,
// recursively, their nested groups. implicit counts the partners alone.
func countGroupTags(dict *dictionary.Dict, groups []dictionary.GroupDef, counts, implicit map[int]int) {
	for _, gd := range groups {
		counts[gd.CounterTag]++
		declared, partners := groupFieldTags(dict, gd)
		for _, tag := range declared {
			counts[tag]++
		}
		for _, tag := range partners {
			counts[tag]++
			implicit[tag]++
		}
		countGroupTags(dict, gd.Groups, counts, implicit)
	}
}

func collectGroupTags(dict *dictionary.Dict, groups []dictionary.GroupDef, childTags, counterTags map[int]bool, nested bool) {
	for _, gd := range groups {
		counterTags[gd.CounterTag] = true
		if nested {
			childTags[gd.CounterTag] = true
		}
		declared, partners := groupFieldTags(dict, gd)
		for _, tag := range slices.Concat(declared, partners) {
			childTags[tag] = true
		}
		collectGroupTags(dict, gd.Groups, childTags, counterTags, true)
	}
}

// groupFieldTags returns the tags a group's entries declare and, apart, the
// DATA or LENGTH field each pairs with that the group does not declare:
// builders write the pair together (see pairDataLengths), so the partner
// is in the entry too.
func groupFieldTags(dict *dictionary.Dict, gd dictionary.GroupDef) (declared, partners []int) {
	for _, fr := range gd.Fields {
		declared = append(declared, fr.Tag)
	}
	for _, tag := range declared {
		for _, p := range []int{fix2.DataLengthTag(tag), fix2.DataTag(tag)} {
			if p != 0 && dict.Fields[p] != nil && !slices.Contains(declared, p) && !slices.Contains(partners, p) {
				partners = append(partners, p)
			}
		}
	}
	return declared, partners
}

// entriesField names the builder field holding a group's entries, e.g.
// noLegsEntries. The suffix keeps it apart from the builders' own members
// (fields, appendTo...) whatever the group is called.
func entriesField(groupName string) string { return unexport(groupName) + "Entries" }

// unexport lowercases the first letter of a name.
func unexport(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// ---------------------------------------------------------------------------
// child tags var + iteration method (split from former writeGroupIterator)
// ---------------------------------------------------------------------------

// specLiteral returns a &fix2.GroupSpec literal for gd and, recursively,
// its nested groups.
func specLiteral(dict *dictionary.Dict, gd dictionary.GroupDef) *jen.Statement {
	var tags []jen.Code
	added := map[int]bool{}
	addTag := func(tag int) {
		if !added[tag] {
			added[tag] = true
			tags = append(tags, tagID(dict.Fields[tag].Name))
		}
	}
	for _, fr := range gd.Fields {
		// DATA setters write the LENGTH and DATA fields together (see
		// pairDataLengths), even when the group declares only one of them:
		// readers must accept both.
		if l := fix2.DataLengthTag(fr.Tag); l != 0 && dict.Fields[l] != nil {
			addTag(l)
		}
		addTag(fr.Tag)
		if d := fix2.DataTag(fr.Tag); d != 0 && dict.Fields[d] != nil {
			addTag(d)
		}
	}
	values := jen.Dict{
		jen.Id("Counter"): tagID(gd.Name),
		jen.Id("Delim"):   tagID(dict.Fields[gd.DelimTag].Name),
		jen.Id("Tags"):    jen.Index().Int().Values(tags...),
	}
	if len(gd.Groups) > 0 {
		var nested []jen.Code
		for _, child := range gd.Groups {
			nested = append(nested, specLiteral(dict, child))
		}
		values[jen.Id("Nested")] = jen.Index().Op("*").Qual(fix2Pkg, "GroupSpec").Values(nested...)
	}
	return jen.Op("&").Qual(fix2Pkg, "GroupSpec").Values(values)
}

// writeIterationMethod generates <groupName>Entries on an owner (a message
// reader or a group entry). spec is the expression of the group's spec in
// that owner's context; entries keep it for their own nested groups. others
// is the expression of the owner's group specs, whose entries are skipped
// while looking for the group's counter.
func writeIterationMethod(f *jen.File, ownerName, ownerRecv, ownerField, groupName string, spec, others jen.Code) {
	entryName := groupName + "Entry"
	f.Func().Params(
		jen.Id(ownerRecv).Op("*").Id(ownerName),
	).Id(groupName+"Entries").Params().Index().Id(entryName).Block(
		jen.Id("spec").Op(":=").Add(spec),
		jen.If(jen.Id("spec").Op("==").Nil()).Block(jen.Return(jen.Nil())),
		jen.Id("groups").Op(":=").Id(ownerRecv).Dot(ownerField).Dot("GroupsOf").Call(jen.Id("spec"), jen.Add(others).Op("...")),
		jen.Id("entries").Op(":=").Make(jen.Index().Id(entryName), jen.Len(jen.Id("groups"))),
		jen.For(jen.Id("i").Op(":=").Range().Id("groups")).Block(
			jen.Id("entries").Index(jen.Id("i")).Op("=").Id(entryName).Values(jen.Dict{
				jen.Id("grp"):  jen.Id("groups").Index(jen.Id("i")),
				jen.Id("spec"): jen.Id("spec"),
			}),
		),
		jen.Return(jen.Id("entries")),
	)
}

// ---------------------------------------------------------------------------
// group entry getters (target e.grp.Get(tag) instead of m.msg.Get(tag))
// ---------------------------------------------------------------------------

// addEntryGetter generates a getter on a group entry; scoped getters read
// through e.get, skipping the entry's nested groups.
func addEntryGetter(f *jen.File, entryName string, tag int, fieldName, fixType string, info goTypeInfo, hasEnums, scoped bool) {
	recv := jen.Id("e").Op("*").Id(entryName)
	tid := tagID(fieldName)
	getCall := jen.Id("e").Dot("grp").Dot("Get").Call(tid)
	if scoped {
		getCall = jen.Id("e").Dot("get").Call(tid)
	}

	switch {
	case hasEnums:
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.Id(enumType(fieldName)), jen.Bool()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.Return(jen.Id(enumType(fieldName)).Call(jen.Id("v")), jen.Id("v").Op("!=").Nil()),
		)

	case info.Category == "string":
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.String(), jen.Bool()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.Return(jen.String().Call(jen.Id("v")), jen.Id("v").Op("!=").Nil()),
		)

	case info.Category == "int":
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.Int(), jen.Bool(), jen.Error()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.If(jen.Id("v").Op("==").Nil()).Block(
				jen.Return(jen.Lit(0), jen.False(), jen.Nil()),
			),
			jen.List(jen.Id("n"), jen.Id("err")).Op(":=").Qual(fix2Pkg, "ParseInt").Call(jen.Id("v")),
			jen.Return(jen.Id("n"), jen.True(), jen.Id("err")),
		)

	case info.Category == "float":
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.Float64(), jen.Bool(), jen.Error()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.If(jen.Id("v").Op("==").Nil()).Block(
				jen.Return(jen.Lit(0.0), jen.False(), jen.Nil()),
			),
			jen.List(jen.Id("f"), jen.Id("err")).Op(":=").Qual(fix2Pkg, "ParseFloat").Call(jen.Id("v")),
			jen.Return(jen.Id("f"), jen.True(), jen.Id("err")),
		)

	case info.Category == "bool":
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.Bool(), jen.Bool()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.Return(jen.Len(jen.Id("v")).Op("==").Lit(1).Op("&&").Id("v").Index(jen.Lit(0)).Op("==").LitRune('Y'), jen.Id("v").Op("!=").Nil()),
		)

	case info.Category == "time":
		parseFunc := timeParseFunc(fixType)
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.Qual("time", "Time"), jen.Bool(), jen.Error()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.If(jen.Id("v").Op("==").Nil()).Block(
				jen.Return(jen.Qual("time", "Time").Values(), jen.False(), jen.Nil()),
			),
			jen.List(jen.Id("t"), jen.Id("err")).Op(":=").Qual(fix2Pkg, parseFunc).Call(jen.Id("v")),
			jen.Return(jen.Id("t"), jen.True(), jen.Id("err")),
		)

	case info.Category == "bytes":
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.Index().Byte(), jen.Bool()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.Return(jen.Id("v"), jen.Id("v").Op("!=").Nil()),
		)
	}
}

// ---------------------------------------------------------------------------
// group entry builder setters
// ---------------------------------------------------------------------------

func addEntryBuilderSetter(f *jen.File, entryBuilderName string, tag int, fieldName, fixType string, info goTypeInfo, hasEnums bool) {
	recv := jen.Id("e").Op("*").Id(entryBuilderName)
	tid := tagID(fieldName)
	// Setting a field twice keeps one value, like Builder.Set.
	appendField := func(valExpr jen.Code) jen.Code {
		return jen.Id("e").Dot("fields").Op("=").Qual(fix2Pkg, "SetField").Call(
			jen.Id("e").Dot("fields"), tid, valExpr,
		)
	}

	switch {
	case hasEnums:
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").Id(enumType(fieldName)),
		).Op("*").Id(entryBuilderName).Block(
			appendField(jen.Index().Byte().Call(jen.String().Call(jen.Id("v")))),
			jen.Return(jen.Id("e")),
		)

	case info.Category == "string":
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").String(),
		).Op("*").Id(entryBuilderName).Block(
			appendField(jen.Index().Byte().Call(jen.Id("v"))),
			jen.Return(jen.Id("e")),
		)

	case info.Category == "int":
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").Int(),
		).Op("*").Id(entryBuilderName).Block(
			appendField(jen.Qual("strconv", "AppendInt").Call(jen.Nil(), jen.Int64().Call(jen.Id("v")), jen.Lit(10))),
			jen.Return(jen.Id("e")),
		)

	case info.Category == "float":
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").Float64(),
		).Op("*").Id(entryBuilderName).Block(
			appendField(jen.Qual(fix2Pkg, "AppendFloat").Call(jen.Nil(), jen.Id("v"))),
			jen.Return(jen.Id("e")),
		)

	case info.Category == "bool":
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").Bool(),
		).Op("*").Id(entryBuilderName).Block(
			jen.If(jen.Id("v")).Block(
				appendField(jen.Index().Byte().Call(jen.Lit("Y"))),
			).Else().Block(
				appendField(jen.Index().Byte().Call(jen.Lit("N"))),
			),
			jen.Return(jen.Id("e")),
		)

	case info.Category == "time":
		fmtConst := timeFormatConst(fixType)
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").Qual("time", "Time"),
		).Op("*").Id(entryBuilderName).Block(
			appendField(jen.Index().Byte().Call(timeValue(fixType).Dot("Format").Call(jen.Qual(fix2Pkg, fmtConst)))),
			jen.Return(jen.Id("e")),
		)
		// Set*2 variant accepting a TimeFormat.
		f.Func().Params(recv).Id("Set"+fieldName+"WithFormat").Params(
			jen.Id("v").Qual("time", "Time"),
			jen.Id("f").Qual(fix2Pkg, "TimeFormat"),
		).Op("*").Id(entryBuilderName).Block(
			appendField(jen.Index().Byte().Call(timeValue(fixType).Dot("Format").Call(jen.Id("f")))),
			jen.Return(jen.Id("e")),
		)

	case info.Category == "bytes":
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").Index().Byte(),
		).Op("*").Id(entryBuilderName).Block(
			// Copy: entries are serialized at Build time, after the caller
			// may have reused v.
			appendField(jen.Qual("bytes", "Clone").Call(jen.Id("v"))),
			jen.Return(jen.Id("e")),
		)
	}
}

// ---------------------------------------------------------------------------
// group Add methods
// ---------------------------------------------------------------------------

// writeGroupAddMethod generates the Add method on a message builder.
func writeGroupAddMethod(f *jen.File, dict *dictionary.Dict, builderName string, gd dictionary.GroupDef) {
	f.Comment("Add" + gd.Name + "Entry adds an entry to the " + gd.Name + " group. Entries are serialized")
	f.Comment("by Build, after the other fields, so later changes to e are included.")
	f.Func().Params(
		jen.Id("b").Op("*").Id(builderName),
	).Id("Add"+gd.Name+"Entry").Params(
		jen.Id("e").Op("*").Id(gd.Name+"EntryBuilder"),
	).Op("*").Id(builderName).Block(
		jen.Id("b").Dot("groups").Dot(entriesField(gd.Name)).Op("=").Append(jen.Id("b").Dot("groups").Dot(entriesField(gd.Name)), jen.Id("e")),
		jen.Return(jen.Id("b")),
	)
}

// groupBlock returns the code appending a group's counter and entries to dst,
// laid out by layout (an expression yielding a *groupLayout), when owner (an
// expression yielding the entry slice) is not empty.
func groupBlock(dst string, owner func() *jen.Statement, gd dictionary.GroupDef, layout jen.Code) jen.Code {
	return jen.If(jen.Len(owner()).Op(">").Lit(0)).Block(groupWrite(dst, owner, gd, layout)...)
}

// groupWrite returns the statements appending a group's counter, even zero,
// and its entries to dst.
func groupWrite(dst string, owner func() *jen.Statement, gd dictionary.GroupDef, layout jen.Code) []jen.Code {
	return []jen.Code{
		jen.Id(dst).Op("=").Qual(fix2Pkg, "AppendCount").Call(jen.Id(dst), tagID(gd.Name), jen.Len(owner())),
		jen.For(jen.List(jen.Id("_"), jen.Id("entry")).Op(":=").Range().Add(owner())).Block(
			jen.Id(dst).Op("=").Id("entry").Dot("appendTo").Call(jen.Id(dst), layout),
		),
	}
}

// layoutLiteral returns a *groupLayout literal for gd as one message defines
// it: the entry's tags in its own order, LENGTH fields right before their
// DATA fields, and its nested groups' layouts.
func layoutLiteral(dict *dictionary.Dict, gd dictionary.GroupDef) *jen.Statement {
	var tags []jen.Code
	for _, tag := range pairDataLengths(dict, gd.Layout) {
		tags = append(tags, tagID(dict.Fields[tag].Name))
	}
	values := jen.Dict{jen.Id("tags"): jen.Index().Int().Values(tags...)}
	if len(gd.Groups) > 0 {
		nested := jen.Dict{}
		for _, child := range gd.Groups {
			nested[tagID(child.Name)] = layoutLiteral(dict, child)
		}
		values[jen.Id("nested")] = jen.Map(jen.Int()).Op("*").Id("groupLayout").Values(nested)
	}
	return jen.Op("&").Id("groupLayout").Values(values)
}

// writeEntryAppendTo generates appendTo, which serializes an entry in the
// order of its layout in the message being built: each layout tag is either
// one of the entry's fields, written if set, or a nested group's counter,
// written with the group's entries in their own layout. The delimiter comes
// first; when it is a nested group's counter, it is written even with no
// entries.
func writeEntryAppendTo(f *jen.File, entryBuilderName string, mg dictionary.GroupDef) {
	var cases []jen.Code
	for _, gd := range mg.Groups {
		name := gd.Name
		owner := func() *jen.Statement { return jen.Id("e").Dot(entriesField(name)) }
		cases = append(cases, jen.Case(tagID(name)).Block(
			jen.If(jen.Len(owner()).Op(">").Lit(0).Op("||").Id("tag").Op("==").Id("l").Dot("tags").Index(jen.Lit(0))).Block(
				groupWrite("dst", owner, gd, jen.Id("l").Dot("nested").Index(tagID(name)))...,
			),
		))
	}
	cases = append(cases, jen.Default().Block(
		jen.Id("dst").Op("=").Qual(fix2Pkg, "AppendField").Call(jen.Id("dst"), jen.Id("e").Dot("fields"), jen.Id("tag")),
	))
	f.Comment("appendTo appends the entry's fields to dst in the order of l, the entry's")
	f.Comment("layout in the message being built. Fields and nested groups l does not list")
	f.Comment("are not written: that message's readers could not find them, nor the entries")
	f.Comment("after them.")
	f.Func().Params(jen.Id("e").Op("*").Id(entryBuilderName)).Id("appendTo").Params(
		jen.Id("dst").Index().Qual(fix2Pkg, "Field"),
		jen.Id("l").Op("*").Id("groupLayout"),
	).Index().Qual(fix2Pkg, "Field").Block(
		jen.For(jen.List(jen.Id("_"), jen.Id("tag")).Op(":=").Range().Id("l").Dot("tags")).Block(
			jen.Switch(jen.Id("tag")).Block(cases...),
		),
		jen.Return(jen.Id("dst")),
	)
}

// pairDataLengths returns layout with the LENGTH tag of every DATA tag the
// parser reads by length placed right before it, as the parser requires,
// whatever order the dictionary produced. A LENGTH tag listed without its
// DATA tag gets it right after: the shared entry setter writes both, and
// LENGTH alone would not parse.
func pairDataLengths(dict *dictionary.Dict, layout []int) []int {
	out := slices.Clone(layout)
	for i := 0; i < len(out); i++ {
		if d := fix2.DataTag(out[i]); d != 0 && dict.Fields[d] != nil && !slices.Contains(out, d) {
			out = slices.Insert(out, i+1, d)
		}
	}
	layout = slices.Clone(out)
	for _, tag := range layout {
		l := fix2.DataLengthTag(tag)
		if l == 0 || dict.Fields[l] == nil || len(out) == 0 {
			continue
		}
		switch {
		case out[0] == tag:
			// DATA is the delimiter: its LENGTH cannot precede it inside
			// the entry, and the delimiter must stay first.
			continue
		case out[0] == l:
			// LENGTH is the delimiter: bring DATA right after it.
			i := slices.Index(out, tag)
			out = slices.Insert(slices.Delete(out, i, i+1), 1, tag)
		default:
			if i := slices.Index(out, l); i >= 0 {
				out = slices.Delete(out, i, i+1)
			}
			out = slices.Insert(out, slices.Index(out, tag), l)
		}
	}
	return out
}

// writeEntryGroupAddMethod generates an Add method for nested groups on an entry builder.
func writeEntryGroupAddMethod(f *jen.File, dict *dictionary.Dict, parentBuilderName string, gd dictionary.GroupDef) {
	f.Comment("Add" + gd.Name + "Entry adds an entry to the nested " + gd.Name + " group. Entries are")
	f.Comment("serialized when the message is built, so later changes to e are included.")
	f.Func().Params(
		jen.Id("e").Op("*").Id(parentBuilderName),
	).Id("Add"+gd.Name+"Entry").Params(
		jen.Id("ne").Op("*").Id(gd.Name+"EntryBuilder"),
	).Op("*").Id(parentBuilderName).Block(
		jen.Id("e").Dot(entriesField(gd.Name)).Op("=").Append(jen.Id("e").Dot(entriesField(gd.Name)), jen.Id("ne")),
		jen.Return(jen.Id("e")),
	)
}

// ---------------------------------------------------------------------------
// getter generation
// ---------------------------------------------------------------------------

func addGetter(f *jen.File, structName string, tag int, fieldName, fixType string, info goTypeInfo, hasEnums bool, get *jen.Statement) {
	recv := jen.Id("m").Op("*").Id(structName)
	tid := tagID(fieldName)
	getCall := jen.Id("m").Dot("msg").Dot("Get").Call(tid)
	if get != nil {
		getCall = get
	}

	switch {
	case hasEnums:
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.Id(enumType(fieldName)), jen.Bool()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.Return(jen.Id(enumType(fieldName)).Call(jen.Id("v")), jen.Id("v").Op("!=").Nil()),
		)

	case info.Category == "string":
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.String(), jen.Bool()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.Return(jen.String().Call(jen.Id("v")), jen.Id("v").Op("!=").Nil()),
		)

	case info.Category == "int":
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.Int(), jen.Bool(), jen.Error()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.If(jen.Id("v").Op("==").Nil()).Block(
				jen.Return(jen.Lit(0), jen.False(), jen.Nil()),
			),
			jen.List(jen.Id("n"), jen.Id("err")).Op(":=").Qual(fix2Pkg, "ParseInt").Call(jen.Id("v")),
			jen.Return(jen.Id("n"), jen.True(), jen.Id("err")),
		)

	case info.Category == "float":
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.Float64(), jen.Bool(), jen.Error()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.If(jen.Id("v").Op("==").Nil()).Block(
				jen.Return(jen.Lit(0.0), jen.False(), jen.Nil()),
			),
			jen.List(jen.Id("f"), jen.Id("err")).Op(":=").Qual(fix2Pkg, "ParseFloat").Call(jen.Id("v")),
			jen.Return(jen.Id("f"), jen.True(), jen.Id("err")),
		)

	case info.Category == "bool":
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.Bool(), jen.Bool()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.Return(jen.Len(jen.Id("v")).Op("==").Lit(1).Op("&&").Id("v").Index(jen.Lit(0)).Op("==").LitRune('Y'), jen.Id("v").Op("!=").Nil()),
		)

	case info.Category == "time":
		parseFunc := timeParseFunc(fixType)
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.Qual("time", "Time"), jen.Bool(), jen.Error()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.If(jen.Id("v").Op("==").Nil()).Block(
				jen.Return(jen.Qual("time", "Time").Values(), jen.False(), jen.Nil()),
			),
			jen.List(jen.Id("t"), jen.Id("err")).Op(":=").Qual(fix2Pkg, parseFunc).Call(jen.Id("v")),
			jen.Return(jen.Id("t"), jen.True(), jen.Id("err")),
		)

	case info.Category == "bytes":
		f.Func().Params(recv).Id(fieldName).Params().Params(jen.Index().Byte(), jen.Bool()).Block(
			jen.Id("v").Op(":=").Add(getCall),
			jen.Return(jen.Id("v"), jen.Id("v").Op("!=").Nil()),
		)
	}
}

// ---------------------------------------------------------------------------
// setter generation
// ---------------------------------------------------------------------------

func addSetter(f *jen.File, builderName string, tag int, fieldName, fixType string, info goTypeInfo, hasEnums bool) {
	recv := jen.Id("b").Op("*").Id(builderName)
	tid := tagID(fieldName)

	switch {
	case hasEnums:
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").Id(enumType(fieldName)),
		).Op("*").Id(builderName).Block(
			jen.Id("b").Dot("SetString").Call(tid, jen.String().Call(jen.Id("v"))),
			jen.Return(jen.Id("b")),
		)

	case info.Category == "string":
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").String(),
		).Op("*").Id(builderName).Block(
			jen.Id("b").Dot("SetString").Call(tid, jen.Id("v")),
			jen.Return(jen.Id("b")),
		)

	case info.Category == "int":
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").Int(),
		).Op("*").Id(builderName).Block(
			jen.Id("b").Dot("SetInt").Call(tid, jen.Id("v")),
			jen.Return(jen.Id("b")),
		)

	case info.Category == "float":
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").Float64(),
		).Op("*").Id(builderName).Block(
			jen.Id("b").Dot("SetFloat").Call(tid, jen.Id("v")),
			jen.Return(jen.Id("b")),
		)

	case info.Category == "bool":
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").Bool(),
		).Op("*").Id(builderName).Block(
			jen.Id("b").Dot("SetBool").Call(tid, jen.Id("v")),
			jen.Return(jen.Id("b")),
		)

	case info.Category == "time":
		fmtConst := timeFormatConst(fixType)
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").Qual("time", "Time"),
		).Op("*").Id(builderName).Block(
			jen.Id("b").Dot("SetString").Call(tid, timeValue(fixType).Dot("Format").Call(jen.Qual(fix2Pkg, fmtConst))),
			jen.Return(jen.Id("b")),
		)
		// Set*2 variant accepting a TimeFormat.
		f.Func().Params(recv).Id("Set"+fieldName+"WithFormat").Params(
			jen.Id("v").Qual("time", "Time"),
			jen.Id("f").Qual(fix2Pkg, "TimeFormat"),
		).Op("*").Id(builderName).Block(
			jen.Id("b").Dot("SetString").Call(tid, timeValue(fixType).Dot("Format").Call(jen.Id("f"))),
			jen.Return(jen.Id("b")),
		)

	case info.Category == "bytes":
		f.Func().Params(recv).Id("Set"+fieldName).Params(
			jen.Id("v").Index().Byte(),
		).Op("*").Id(builderName).Block(
			jen.Id("b").Dot("Set").Call(tid, jen.Id("v")),
			jen.Return(jen.Id("b")),
		)
	}
}

// isDataLength reports whether tag is the LENGTH field of a DATA field the
// parser reads by length: its value is computed by the DATA setter, so it
// gets no setter of its own.
func isDataLength(dict *dictionary.Dict, tag int) bool {
	data := fix2.DataTag(tag)
	return data != 0 && dict.Fields[data] != nil
}

// addDataSetter generates a message builder setter for a DATA field that
// also sets its LENGTH field, adjacent, as the parser requires.
func addDataSetter(f *jen.File, builderName, fieldName, lengthName string) {
	f.Comment("Set" + fieldName + " sets " + fieldName + " and its length, " + lengthName + ".")
	f.Func().Params(jen.Id("b").Op("*").Id(builderName)).Id("Set"+fieldName).Params(
		jen.Id("v").Index().Byte(),
	).Op("*").Id(builderName).Block(
		jen.Id("b").Dot("SetData").Call(tagID(lengthName), tagID(fieldName), jen.Id("v")),
		jen.Return(jen.Id("b")),
	)
}

// addEntryDataSetter is addDataSetter for group entry builders. The entry's
// layout keeps the two fields in dictionary order, LENGTH right before DATA.
func addEntryDataSetter(f *jen.File, entryBuilderName, fieldName, lengthName string) {
	f.Comment("Set" + fieldName + " sets " + fieldName + " and its length, " + lengthName + ".")
	f.Func().Params(jen.Id("e").Op("*").Id(entryBuilderName)).Id("Set"+fieldName).Params(
		jen.Id("v").Index().Byte(),
	).Op("*").Id(entryBuilderName).Block(
		jen.Id("e").Dot("fields").Op("=").Qual(fix2Pkg, "SetField").Call(
			jen.Id("e").Dot("fields"), tagID(lengthName),
			jen.Qual("strconv", "AppendInt").Call(jen.Nil(), jen.Int64().Call(jen.Len(jen.Id("v"))), jen.Lit(10)),
		),
		jen.Id("e").Dot("fields").Op("=").Qual(fix2Pkg, "SetField").Call(
			jen.Id("e").Dot("fields"), tagID(fieldName), jen.Qual("bytes", "Clone").Call(jen.Id("v")),
		),
		jen.Return(jen.Id("e")),
	)
}

// tagID returns a jen expression for the tag constant (e.g. TagSide).
func tagID(fieldName string) *jen.Statement {
	return jen.Id("Tag" + fieldName)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func timeParseFunc(fixType string) string {
	switch fixType {
	case "UTCDATE", "LOCALMKTDATE", "DATE":
		return "ParseUTCDate"
	case "UTCTIMEONLY":
		return "ParseUTCTimeOnly"
	default:
		return "ParseUTCTimestamp"
	}
}

// isLocalDate reports whether fixType is a local (market) date, whose civil
// year/month/day must be written as given rather than converted to UTC.
func isLocalDate(fixType string) bool {
	return fixType == "LOCALMKTDATE" || fixType == "DATE"
}

// timeValue returns the expression a time setter formats: v as given for local
// dates, v.UTC() for the UTC types.
func timeValue(fixType string) *jen.Statement {
	if isLocalDate(fixType) {
		return jen.Id("v")
	}
	return jen.Id("v").Dot("UTC").Call()
}

func timeFormatConst(fixType string) string {
	switch fixType {
	case "UTCDATE", "LOCALMKTDATE", "DATE":
		return "TimeFormatDate"
	case "UTCTIMEONLY":
		return "TimeFormatTimeOnly"
	case "TIME":
		// FIX 4.0/4.1 TIME is YYYYMMDD-HH:MM:SS, without milliseconds.
		return "TimeFormatTimestampSec"
	default:
		return "TimeFormatTimestamp"
	}
}

// sanitizePascal converts a description string (e.g. "BUY") to PascalCase
// suitable for a Go identifier.
func sanitizePascal(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			upper = true
			continue
		}
		if upper {
			b.WriteRune(unicode.ToUpper(r))
			upper = false
		} else {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// toSnakeCase converts PascalCase to snake_case.
func toSnakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) && i > 0 {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
