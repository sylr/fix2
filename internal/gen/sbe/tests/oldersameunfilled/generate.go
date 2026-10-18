// Package oldersameunfilled holds the codecs fix2gen sbe generates from
// testdata/older.xml, as package oldersame, with --tolerate-unfilled-counts
// too: they take counts of 0 for counts left unfilled, in newer versions
// too, then taking them to hold nothing version 1 does not know.
package oldersameunfilled

//go:generate go run ../../../../../cmd/fix2gen sbe --schema ../../testdata/older.xml --pkg oldersameunfilled --out . --assume-same-encodings --tolerate-unfilled-counts
