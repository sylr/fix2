//go:build !goexperiment.simd || !amd64

package fix2

import "bytes"

// findByte returns the index of the first occurrence of b in data[offset:],
// relative to the start of data. Returns -1 if not found.
func findByte(data []byte, offset int, b byte) int {
	if offset >= len(data) {
		return -1
	}
	i := bytes.IndexByte(data[offset:], b)
	if i < 0 {
		return -1
	}
	return offset + i
}

// checksumBlock returns the sum of all bytes in data as uint32.
func checksumBlock(data []byte) uint32 {
	var sum uint32
	for _, c := range data {
		sum += uint32(c)
	}
	return sum
}
