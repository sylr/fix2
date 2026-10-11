module github.com/sylr/fix2/contribs/binancemd

go 1.26

require github.com/sylr/fix2 v0.0.0

require (
	github.com/dave/jennifer v1.7.1 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

// fix2 is developed alongside: this module builds against the copy in the
// repository.
replace github.com/sylr/fix2 => ../..

tool github.com/sylr/fix2/cmd/fixgen
