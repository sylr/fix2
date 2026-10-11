package main

import (
	"maps"
	"path/filepath"
	"slices"

	"github.com/dave/jennifer/jen"

	"github.com/sylr/fix2/dictionary"
)

// writeDispatch generates dispatch.go: a Handler interface with a method per
// message, an UnimplementedHandler to embed, and Dispatch, which calls the
// method of a message's type.
func writeDispatch(dict *dictionary.Dict, pkg, outDir string) error {
	f := jen.NewFile(pkg)
	f.HeaderComment(generatedHeader)
	f.ImportName(fix2Pkg, "fix2")

	msgTypes := slices.Sorted(maps.Keys(dict.Messages))
	var methods, unimplemented, cases []jen.Code
	for _, mt := range msgTypes {
		md := dict.Messages[mt]
		method := "On" + md.Name
		methods = append(methods, jen.Id(method).Params(jen.Op("*").Id(md.Name)).Error())
		unimplemented = append(unimplemented, jen.Func().Params(jen.Id("UnimplementedHandler")).Id(method).Params(jen.Op("*").Id(md.Name)).Error().Block(
			jen.Return(jen.Id("unsupported").Call(jen.Lit(mt))),
		))
		cases = append(cases, jen.Case(jen.Lit(mt)).Block(
			jen.Return(jen.Id("h").Dot(method).Call(jen.Id("Wrap"+md.Name).Call(jen.Id("m")))),
		))
	}
	f.Comment("Handler receives the messages of this package, typed, from Dispatch: embed")
	f.Comment("UnimplementedHandler and implement the methods of the messages handled.")
	f.Type().Id("Handler").Interface(methods...)
	f.Comment("UnimplementedHandler reports every message type unsupported (see")
	f.Comment("fix2.ErrUnsupportedMessageType).")
	f.Type().Id("UnimplementedHandler").Struct()
	for _, u := range unimplemented {
		f.Add(u)
	}
	f.Comment("Dispatch calls the method of h for m's MsgType. A type this package does not")
	f.Comment("define is unsupported (see fix2.ErrUnsupportedMessageType); sessions answer")
	f.Comment("it with a BusinessMessageReject.")
	f.Func().Id("Dispatch").Params(jen.Id("m").Op("*").Qual(fix2Pkg, "Message"), jen.Id("h").Id("Handler")).Error().Block(
		jen.Switch(jen.String().Parens(jen.Id("m").Dot("MsgType").Call())).Block(cases...),
		jen.Return(jen.Id("unsupported").Call(jen.String().Parens(jen.Id("m").Dot("MsgType").Call()))),
	)
	f.Func().Id("unsupported").Params(jen.Id("msgType").String()).Error().Block(
		jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("%w: %s"), jen.Qual(fix2Pkg, "ErrUnsupportedMessageType"), jen.Id("msgType"))),
	)
	return f.Save(filepath.Join(outDir, "dispatch.go"))
}
