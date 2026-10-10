package fix2

import "bytes"

// findByte returns the index of the first occurrence of b in data[offset:],
// relative to the start of data. Returns -1 if not found.
//
// Delegates to bytes.IndexByte, which the Go runtime already implements in
// vectorized assembly (AVX2 on amd64, NEON on arm64) — faster than a 128-bit
// archsimd loop for all input sizes.
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
