// Package binance holds the codecs fix2gen sbe generates from Binance's Spot
// "FIX SBE" message schema, and their tests: the schema needs four of
// fix2gen sbe's tolerations (see docs/sbe.md).
package binance

// spot-fixsbe-1_1.xml is Binance's SBE schema of the Spot FIX SBE API
// (schema id 1, version 1), from
// https://github.com/binance/binance-spot-api-docs/blob/14f151e8ea56b2c353cdc6857e23f3b4a7e3559a/sbe/schemas/spot-fixsbe-1_1.xml
// (SHA-256 6b44afe558d439ddc94323fd6ff026a949184ddd79a2cf033f610194c2537e70).
// Its mbx:exponent attributes, the fields its integers' types name as bare
// primitives (uint32, int64), and the groups it defines differently under
// one name (Orders, RelatedSym, MDEntriesBids, MDEntriesAsks), and the
// fields it gives two encodings (ClOrdID, Symbol and others as varString8
// and optionalVarString8) or two names (id 58, Text and ErrorText), each
// take a toleration.

//go:generate go run ../../../../cmd/fix2gen sbe -schema spot-fixsbe-1_1.xml -pkg binance -out . -tolerate-foreign-metadata -tolerate-undeclared-primitive-types -tolerate-group-redefinitions -tolerate-field-redefinitions
