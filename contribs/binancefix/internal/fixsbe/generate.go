// Package fixsbe holds the codecs fix2gen sbe generates from Binance's Spot
// "FIX SBE" message schema, and their tests: the schema needs four of
// fix2gen sbe's tolerations (see docs/sbe.md).
package fixsbe

// spot-fixsbe-1_1.xml is Binance's SBE schema of the Spot FIX SBE API
// (schema id 1, version 1), vendored from
// https://github.com/binance/binance-spot-api-docs/blob/14f151e8ea56b2c353cdc6857e23f3b4a7e3559a/sbe/schemas/spot-fixsbe-1_1.xml
// (SHA-256 6b44afe558d439ddc94323fd6ff026a949184ddd79a2cf033f610194c2537e70,
// TestSchemaPinned): generating never downloads it. fix2 keeps the same
// file as its generator's test data (internal/gen/sbe/testdata/binance).
// Its mbx:exponent attributes, the fields its integers' types name as bare
// primitives (uint32, int64), the groups it defines differently under one
// name (Orders, RelatedSym, MDEntriesBids, MDEntriesAsks), and the fields
// it gives two encodings (ClOrdID, Symbol and others as varString8 and
// optionalVarString8) or two names (id 58, Text and ErrorText), each take a
// toleration.

//go:generate go tool fix2gen sbe -schema spot-fixsbe-1_1.xml -pkg fixsbe -out . -tolerate-foreign-metadata -tolerate-undeclared-primitive-types -tolerate-group-redefinitions -tolerate-field-redefinitions

// Package binancefix exports these codecs through aliases, regenerated
// after them.

//go:generate go run ../aliasgen -src . -out ../../codecs.go
