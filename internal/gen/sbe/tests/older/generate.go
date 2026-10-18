// Package older holds the codecs fix2gen sbe generates from
// testdata/older.xml, version 1 of a schema, strictly: they refuse the
// groups and data of version 2, which package newer encodes, with ErrNewer.
// Package newer's tests decode what it encodes with them, and with those of
// packages oldersame, olderunfilled and oldersameunfilled, generated from
// the same schema with options.
package older

//go:generate go run ../../../../../cmd/fix2gen sbe --schema ../../testdata/older.xml --pkg older --out .
