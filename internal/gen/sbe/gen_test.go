package sbe

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strings"
	"testing"
)

// typecheck generates s and type-checks the package it makes.
func typecheck(t *testing.T, s *Schema) map[string][]byte {
	t.Helper()
	g := &gen{s: s, pkg: "p", idents: map[string]string{}, groups: map[string]*Group{}, files: map[string][]byte{}}
	if err := g.generate(); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range slices.Sorted(maps.Keys(g.files)) {
		f, err := parser.ParseFile(fset, name, g.files[name], 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check("p", fset, files, nil); err != nil {
		t.Fatal(err)
	}
	return g.files
}

// The schemas the tests do not generate into packages of their own.
func TestGenerateTypechecks(t *testing.T) {
	s, err := LoadFile("../../../fixp/internal/codec/SBEschemaForFIXP.xml")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("FIXP", func(t *testing.T) { typecheck(t, s) })

	t.Run("prose naming a package", func(t *testing.T) {
		s, err := Load(strings.NewReader(schema(
			`<composite name="c" description="not math.Pi nor bytes.Buffer"><type name="a" primitiveType="uint8"/></composite>`,
			`<sbe:message name="m" id="1" description="fmt.Sprintf slices.Sort"><field name="f" id="2" type="c"/></sbe:message>`)))
		if err != nil {
			t.Fatal(err)
		}
		files := typecheck(t, s)
		if strings.Contains(string(files["types.go"]), `"math"`) || strings.Contains(string(files["msg_m.go"]), `"slices"`) {
			t.Errorf("imports what descriptions name:\n%s\n%s", files["types.go"], files["msg_m.go"])
		}
	})
}

func TestGenerateRejects(t *testing.T) {
	for _, tc := range []struct {
		name, types, messages, want string
	}{
		{"one file for two messages", `<type name="u8" primitiveType="uint8"/>`,
			`<sbe:message name="FooBar" id="1"/><sbe:message name="Foo_Bar" id="2"/>`, "both written to msg_foo_bar.go"},
		{"one Go name for two types", `<type name="u8" primitiveType="uint8"/><enum name="e" encodingType="u8"><validValue name="a">1</validValue></enum><enum name="E" encodingType="u8"><validValue name="b">2</validValue></enum>`,
			``, "are both E in Go"},
		{"a field named as a method", `<type name="u8" primitiveType="uint8"/>`,
			`<sbe:message name="m" id="1"><field name="appendTo" id="2" type="u8"/></sbe:message>`, "AppendTo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Load(strings.NewReader(schema(tc.types, tc.messages)))
			if err != nil {
				t.Fatal(err)
			}
			g := &gen{s: s, pkg: "p", idents: map[string]string{}, groups: map[string]*Group{}, files: map[string][]byte{}}
			if err := g.generate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("generate = %v, want an error with %q", err, tc.want)
			}
		})
	}
}
