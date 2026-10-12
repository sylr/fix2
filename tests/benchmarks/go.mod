module github.com/sylr/fix2/tests/benchmarks

go 1.27

require (
	github.com/quickfixgo/quickfix v0.9.12
	github.com/sylr/fix2 v0.0.0
)

require (
	github.com/pires/go-proxyproto v0.7.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/quagmt/udecimal v1.8.0 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	golang.org/x/net v0.24.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

// fix2 is developed alongside: the benchmarks build against the copy in the
// repository.
replace github.com/sylr/fix2 => ../..
