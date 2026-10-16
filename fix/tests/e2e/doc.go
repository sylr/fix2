// Package e2e tests the library end to end: an acceptor and an initiator,
// configured with the limited FIX 4.4 and FIX 5.0 SP2 dictionaries of
// fix/dictionary/testdata, log on over TCP and in memory; the initiator
// sends NewOrderSingle messages built with the packages fix2gen generated
// from them (tests/dicts/fix44, tests/dicts/fix50sp2), and the acceptor
// acknowledges each and fills it with ExecutionReport messages. Both
// sessions validate every message they receive against their dictionaries.
package e2e
