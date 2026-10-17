// Package fixsbe holds the codecs fix2gen sbe generates from Binance's Spot
// "FIX SBE" message schema: the same pinned schema, generated with the same
// tolerations, as fix2's internal/gen/sbe/binance (see docs/sbe.md).
package fixsbe

// spot-fixsbe-1_1.xml is Binance's SBE schema of the Spot FIX SBE API
// (schema id 1, version 1), vendored from
// https://github.com/binance/binance-spot-api-docs/blob/14f151e8ea56b2c353cdc6857e23f3b4a7e3559a/sbe/schemas/spot-fixsbe-1_1.xml
// (SHA-256 6b44afe558d439ddc94323fd6ff026a949184ddd79a2cf033f610194c2537e70),
// a copy of internal/gen/sbe/binance's: generating never downloads it.

//go:generate go tool fix2gen sbe -schema spot-fixsbe-1_1.xml -pkg fixsbe -out . -tolerate-foreign-metadata -tolerate-undeclared-primitive-types -tolerate-group-redefinitions -tolerate-field-redefinitions

// Package binancefix exports these codecs through aliases, regenerated
// after them.

//go:generate go run ../aliasgen -src . -out ../../codecs.go
