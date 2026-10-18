package fixt

// FIXT11-prod-sand.xml is Coinbase Exchange's QuickFIX transport dictionary
// of the FIX market data API, from market-data/ in
// https://docs.cdp.coinbase.com/exchange/files/cb_exch_fix_dictionaries-20260612.zip

//go:generate go tool fix2gen tagvalue --dict FIXT11-prod-sand.xml --pkg fixt --out . --bytes
