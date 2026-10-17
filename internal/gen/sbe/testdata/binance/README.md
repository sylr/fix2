# Binance's Spot FIX SBE schema

`spot-fixsbe-1_1.xml` is Binance's SBE schema of the Spot FIX SBE API
(schema id 1, version 1), from
https://github.com/binance/binance-spot-api-docs/blob/14f151e8ea56b2c353cdc6857e23f3b4a7e3559a/sbe/schemas/spot-fixsbe-1_1.xml
(SHA-256 `6b44afe558d439ddc94323fd6ff026a949184ddd79a2cf033f610194c2537e70`),
kept as fix2gen sbe's test data: it needs four of the generator's
tolerations (`binance_test.go`, docs/sbe.md). Its mbx:exponent attributes
take `-tolerate-foreign-metadata`; the fields its integers' types name as
bare primitives (uint32, int64), `-tolerate-undeclared-primitive-types`;
the groups it defines differently under one name (Orders, RelatedSym,
MDEntriesBids, MDEntriesAsks), `-tolerate-group-redefinitions`; the
fields it gives two encodings (ClOrdID, Symbol and others as varString8
and optionalVarString8) or two names (id 58, Text and ErrorText),
`-tolerate-field-redefinitions`.

The codecs generated from it, and their tests, are binancefix's
(`contribs/binancefix/internal/fixsbe`), which vendors the same file:
the two copies are byte for byte the same, both pinned to that SHA-256.
Neither is ever downloaded at generation.
