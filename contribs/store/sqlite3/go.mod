module github.com/sylr/fix2/contribs/store/sqlite3

go 1.27

require (
	github.com/mattn/go-sqlite3 v1.14.52
	github.com/sylr/fix2 v0.1.0
)

require golang.org/x/sys v0.47.0 // indirect

// fix2 is developed alongside: this module builds against the copy in the
// repository.
replace github.com/sylr/fix2 => ../../..
