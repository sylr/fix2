package md

// FIX50-prod-sand.xml is Coinbase Exchange's QuickFIX application
// dictionary of the FIX market data API, from market-data/ in
// https://docs.cdp.coinbase.com/exchange/files/cb_exch_fix_dictionaries-20260612.zip
//
// It declares FIX 5.0 (Version.ApplVerID 7), but Coinbase speaks FIX 5.0
// SP2: sessions announce DefaultApplVerID (1137) 9.

//go:generate go tool fix2gen tagvalue --dict FIX50-prod-sand.xml --pkg md --out . --dispatch --bytes
