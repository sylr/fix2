module github.com/sylr/fix2/contribs/binancefix

go 1.27

require (
	github.com/spf13/cobra v1.10.2
	github.com/sylr/fix2 v0.0.0
)

require (
	github.com/dave/jennifer v1.7.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
)

// fix2 is developed alongside: this module builds against the copy in the
// repository.
replace github.com/sylr/fix2 => ../..

tool github.com/sylr/fix2/cmd/fix2gen
