module github.com/sylr/fix2/jepsen/node

go 1.27

require (
	github.com/sylr/fix2 v0.1.0
	github.com/sylr/fix2/contribs/store/sqlite3 v0.0.0
	golang.org/x/sys v0.47.0
)

require github.com/mattn/go-sqlite3 v1.14.52 // indirect

// fix2 and its SQLite store are developed alongside: this module builds
// against the copies in the repository.
replace (
	github.com/sylr/fix2 => ../..
	github.com/sylr/fix2/contribs/store/sqlite3 => ../../contribs/store/sqlite3
)
