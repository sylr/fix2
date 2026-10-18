// Package newer holds the codecs fix2gen sbe generates from
// testdata/newer.xml, version 2 of the schema of packages older, oldersame,
// olderunfilled and oldersameunfilled, whose decoding of what these encode its tests check.
package newer

//go:generate go run ../../../../../cmd/fix2gen sbe --schema ../../testdata/newer.xml --pkg newer --out .
