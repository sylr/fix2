// Package codec holds the codecs of FIXP's session messages, which fix2gen
// sbe generates from SBEschemaForFIXP.xml, the schema of the FIX Trading
// Community's fixp-specification as published (docs/fixp.md), and the
// classification of the frames a FIXP session reads.
package codec

//go:generate go run ../../../cmd/fix2gen sbe -schema SBEschemaForFIXP.xml -pkg codec -out .
