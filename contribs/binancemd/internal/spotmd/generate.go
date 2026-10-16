package spotmd

// spot-fix-md.xml is Binance's QuickFIX dictionary of the Spot FIX market
// data API, from
// https://github.com/binance/binance-spot-api-docs/blob/5fdb76ad6108894e2ae9f2031cce23681b855cac/fix/schemas/spot-fix-md.xml

//go:generate go tool fix2gen tagvalue -dict spot-fix-md.xml -pkg spotmd -out . -dispatch -bytes
