// Package olderunfilled holds the codecs fix2gen sbe generates from
// testdata/older.xml, as package older, with -tolerate-unfilled-counts: they
// take counts of 0 for counts left unfilled, as Real Logic's encoders leave
// them.
package olderunfilled

//go:generate go run ../../../../../cmd/fix2gen sbe -schema ../../testdata/older.xml -pkg olderunfilled -out . -tolerate-unfilled-counts
