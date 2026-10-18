// Package codec holds the codecs of FIXP's session messages, which fix2gen
// sbe generates from SBEschemaForFIXP.xml, the schema of the FIX Trading
// Community's fixp-specification as published (docs/fixp.md), and the
// classification of the frames a FIXP session reads.
package codec

// The schema names fields Code with ids 7, 12, 15 and 16, and gives id 8 to
// Reason and Flow, which SBE forbids (§6.9): --tolerate-field-redefinitions
// loads it as published (docs/conformance.md, Contradictions).

//go:generate go run ../../../cmd/fix2gen sbe --schema SBEschemaForFIXP.xml --pkg codec --out . --tolerate-field-redefinitions
