package main

import (
	"maps"
	"path/filepath"
	"slices"

	"github.com/dave/jennifer/jen"

	"github.com/sylr/fix2/dictionary"
)

const dictPkg = "github.com/sylr/fix2/dictionary"

// refsPerLine is how many field references or tags a line of the dictionary
// literal holds.
const refsPerLine = 4

// writeDictionary generates dictionary.go: Dictionary returns the dictionary
// the package was generated from, as dictionary.Load reads it, so that
// validating messages needs no XML at run time.
func writeDictionary(dict *dictionary.Dict, pkg, outDir string) error {
	f := jen.NewFile(pkg)
	f.HeaderComment(generatedHeader)
	f.ImportName(dictPkg, "dictionary")

	tag := func(t int) jen.Code {
		if fd := dict.Fields[t]; fd != nil {
			return tagID(fd.Name)
		}
		return jen.Lit(t)
	}

	values := jen.Dict{
		jen.Id("Type"):     jen.Lit(dict.Type),
		jen.Id("Major"):    jen.Lit(dict.Major),
		jen.Id("Minor"):    jen.Lit(dict.Minor),
		jen.Id("Fields"):   fieldsLiteral(dict, tag),
		jen.Id("Messages"): messagesLiteral(dict, tag),
	}
	for name, refs := range map[string][]dictionary.FieldRef{"Header": dict.Header, "Trailer": dict.Trailer} {
		if len(refs) > 0 {
			values[jen.Id(name)] = refsLiteral(refs, tag)
		}
	}
	for name, groups := range map[string][]dictionary.GroupDef{"HeaderGroups": dict.HeaderGroups, "TrailerGroups": dict.TrailerGroups} {
		if len(groups) > 0 {
			values[jen.Id(name)] = groupsLiteral(groups, tag)
		}
	}
	for name, conds := range map[string][]dictionary.Condition{"HeaderConditions": dict.HeaderConditions, "TrailerConditions": dict.TrailerConditions} {
		if len(conds) > 0 {
			values[jen.Id(name)] = conditionsLiteral(conds, tag)
		}
	}

	f.Comment("Dictionary returns the dictionary this package was generated from, as")
	f.Comment("dictionary.Load reads it, for validating messages without the XML. It is built")
	f.Comment("on first use and shared: it must not be changed.")
	f.Var().Id("Dictionary").Op("=").Qual("sync", "OnceValue").Call(
		jen.Func().Params().Op("*").Qual(dictPkg, "Dict").Block(
			jen.Return(jen.Qual(dictPkg, "New").Call(jen.Op("&").Qual(dictPkg, "Dict").Values(values))),
		),
	)

	return f.Save(filepath.Join(outDir, "dictionary.go"))
}

// lines returns items broken into lines of refsPerLine, for a composite
// literal.
func lines(items []jen.Code) []jen.Code {
	out := make([]jen.Code, 0, len(items)+1)
	for i, it := range items {
		if i%refsPerLine == 0 {
			it = jen.Line().Add(it)
		}
		out = append(out, it)
	}
	return append(out, jen.Line())
}

// fieldsLiteral returns the Fields map, in tag order.
func fieldsLiteral(dict *dictionary.Dict, tag func(int) jen.Code) jen.Code {
	var items []jen.Code
	for _, t := range slices.Sorted(maps.Keys(dict.Fields)) {
		fd := dict.Fields[t]
		def := jen.Dict{
			jen.Id("Tag"):  tag(t),
			jen.Id("Name"): jen.Lit(fd.Name),
			jen.Id("Type"): jen.Lit(fd.Type),
		}
		if fd.Enums != nil {
			var enums []jen.Code
			for _, v := range slices.Sorted(maps.Keys(fd.Enums)) {
				enums = append(enums, jen.Line().Lit(v).Op(":").Lit(fd.Enums[v]))
			}
			def[jen.Id("Enums")] = jen.Map(jen.String()).String().Values(append(enums, jen.Line())...)
		}
		items = append(items, jen.Line().Add(tag(t)).Op(":").Values(def))
	}
	return jen.Map(jen.Int()).Op("*").Qual(dictPkg, "FieldDef").Values(append(items, jen.Line())...)
}

// messagesLiteral returns the Messages map, in MsgType order.
func messagesLiteral(dict *dictionary.Dict, tag func(int) jen.Code) jen.Code {
	var items []jen.Code
	for _, mt := range slices.Sorted(maps.Keys(dict.Messages)) {
		md := dict.Messages[mt]
		def := jen.Dict{
			jen.Id("Name"):    jen.Lit(md.Name),
			jen.Id("MsgType"): jen.Lit(md.MsgType),
		}
		if md.MsgCat != "" {
			def[jen.Id("MsgCat")] = jen.Lit(md.MsgCat)
		}
		if len(md.Fields) > 0 {
			def[jen.Id("Fields")] = refsLiteral(md.Fields, tag)
		}
		if len(md.Groups) > 0 {
			def[jen.Id("Groups")] = groupsLiteral(md.Groups, tag)
		}
		if len(md.Conditions) > 0 {
			def[jen.Id("Conditions")] = conditionsLiteral(md.Conditions, tag)
		}
		items = append(items, jen.Line().Lit(mt).Op(":").Values(def))
	}
	return jen.Map(jen.String()).Op("*").Qual(dictPkg, "MessageDef").Values(append(items, jen.Line())...)
}

// refsLiteral returns refs as keyed literals: building them with function
// calls made the compiler inline tens of thousands of them, adding 20s to
// building a FIX 5.0 SP2 package.
func refsLiteral(refs []dictionary.FieldRef, tag func(int) jen.Code) jen.Code {
	items := make([]jen.Code, len(refs))
	for i, r := range refs {
		ref := []jen.Code{jen.Id("Tag").Op(":").Add(tag(r.Tag))}
		if r.Required {
			ref = append(ref, jen.Id("Required").Op(":").True())
		}
		items[i] = jen.Values(ref...)
	}
	return jen.Index().Qual(dictPkg, "FieldRef").Values(lines(items)...)
}

func tagsLiteral(tags []int, tag func(int) jen.Code) jen.Code {
	items := make([]jen.Code, len(tags))
	for i, t := range tags {
		items[i] = tag(t)
	}
	return jen.Index().Int().Values(lines(items)...)
}

func groupsLiteral(groups []dictionary.GroupDef, tag func(int) jen.Code) jen.Code {
	var items []jen.Code
	for _, g := range groups {
		def := jen.Dict{
			jen.Id("CounterTag"): tag(g.CounterTag),
			jen.Id("Name"):       jen.Lit(g.Name),
			jen.Id("DelimTag"):   tag(g.DelimTag),
		}
		if g.Required {
			def[jen.Id("Required")] = jen.True()
		}
		if len(g.Fields) > 0 {
			def[jen.Id("Fields")] = refsLiteral(g.Fields, tag)
		}
		if len(g.Groups) > 0 {
			def[jen.Id("Groups")] = groupsLiteral(g.Groups, tag)
		}
		if len(g.Layout) > 0 {
			def[jen.Id("Layout")] = tagsLiteral(g.Layout, tag)
		}
		items = append(items, jen.Line().Values(def))
	}
	return jen.Index().Qual(dictPkg, "GroupDef").Values(append(items, jen.Line())...)
}

func conditionsLiteral(conds []dictionary.Condition, tag func(int) jen.Code) jen.Code {
	var items []jen.Code
	for _, c := range conds {
		def := jen.Dict{jen.Id("Component"): jen.Lit(c.Component)}
		if len(c.Activation) > 0 {
			def[jen.Id("Activation")] = tagsLiteral(c.Activation, tag)
		}
		if len(c.Required) > 0 {
			def[jen.Id("Required")] = tagsLiteral(c.Required, tag)
		}
		items = append(items, jen.Line().Values(def))
	}
	return jen.Index().Qual(dictPkg, "Condition").Values(append(items, jen.Line())...)
}
