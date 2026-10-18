// Package oldersame holds the codecs fix2gen sbe generates from
// testdata/older.xml, as package older, with -assume-same-encodings: they
// skip the groups and data of version 2 by version 1's encodings.
package oldersame

//go:generate go run ../../../../../cmd/fix2gen sbe -schema ../../testdata/older.xml -pkg oldersame -out . -assume-same-encodings
