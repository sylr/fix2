package fix2

import (
	"fmt"
	"strconv"
)

// ParseInt parses a FIX integer value from a byte slice.
// FIX integers are sequences of digits with an optional leading '-' sign.
func ParseInt(v []byte) (int, error) {
	if len(v) == 0 {
		return 0, fmt.Errorf("ParseInt: empty input")
	}

	start := 0
	neg := false
	if v[0] == '-' {
		neg = true
		start = 1
		if len(v) == 1 {
			return 0, fmt.Errorf("ParseInt: invalid input %q", v)
		}
	}

	n := 0
	for i := start; i < len(v); i++ {
		c := v[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("ParseInt: invalid byte '%c' in %q", c, v)
		}
		n = n*10 + int(c-'0')
	}

	if neg {
		return -n, nil
	}
	return n, nil
}

// ParseFloat parses a FIX float value (FLOAT, PRICE, QTY, AMT, PERCENTAGE)
// from a byte slice.
func ParseFloat(v []byte) (float64, error) {
	if len(v) == 0 {
		return 0, fmt.Errorf("ParseFloat: empty input")
	}
	return strconv.ParseFloat(string(v), 64)
}
