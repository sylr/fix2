package binancefix

import "strconv"

// FormatDecimal returns mantissa × 10^exponent as a decimal string,
// exactly: Binance's prices and quantities are such pairs (mantissa64
// fields, scaled by their message's, or their entry's, PriceExponent or
// QtyExponent). A negative exponent gives as many digits after the point,
// trailing zeros kept, the precision of the field: 150000000 and -8 give
// "1.50000000", 0 and -2 "0.00"; a positive one appends zeros, 15 and 2
// giving "1500"; zero stays a number. No float is involved, and every
// int64 is formatted, math.MinInt64 included: whether a field is null is
// told by its Optional, or by the market data validation (see doc.go),
// not here.
func FormatDecimal(mantissa int64, exponent int8) string {
	return string(AppendDecimal(nil, mantissa, exponent))
}

// AppendDecimal appends FormatDecimal(mantissa, exponent) to dst.
func AppendDecimal(dst []byte, mantissa int64, exponent int8) []byte {
	var abs uint64
	if mantissa < 0 {
		dst = append(dst, '-')
		abs = uint64(-(mantissa + 1)) + 1 // math.MinInt64's too
	} else {
		abs = uint64(mantissa)
	}
	var buf [20]byte
	digits := strconv.AppendUint(buf[:0], abs, 10)
	if exponent >= 0 {
		dst = append(dst, digits...)
		if abs != 0 {
			for range exponent {
				dst = append(dst, '0')
			}
		}
		return dst
	}
	frac := -int(exponent)
	if len(digits) > frac {
		dst = append(dst, digits[:len(digits)-frac]...)
		dst = append(dst, '.')
		return append(dst, digits[len(digits)-frac:]...)
	}
	dst = append(dst, '0', '.')
	for range frac - len(digits) {
		dst = append(dst, '0')
	}
	return append(dst, digits...)
}
