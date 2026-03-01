//go:build goexperiment.simd && amd64

package fix2

import (
	"bytes"
	"simd/archsimd"
)

var hasAVX = archsimd.X86.AVX()

// findByte returns the index of the first occurrence of b in data[offset:],
// relative to the start of data. Returns -1 if not found.
//
// Delegates to bytes.IndexByte which already uses optimized AVX2 assembly
// in the Go runtime — faster than a 128-bit SIMD loop for all input sizes.
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
	if !hasAVX {
		return checksumBlockGeneric(data)
	}
	zero := archsimd.BroadcastUint8x16(0)
	acc := archsimd.BroadcastUint16x8(0)

	i := 0
	for i+16 <= len(data) {
		chunk := archsimd.LoadUint8x16Slice(data[i:])
		// SumAbsDiff against zero computes sum of bytes in each 8-byte half.
		sad := chunk.SumAbsDiff(zero)
		acc = acc.Add(sad)
		i += 16
	}
	// Tail.
	if i < len(data) {
		chunk := archsimd.LoadUint8x16SlicePart(data[i:])
		sad := chunk.SumAbsDiff(zero)
		acc = acc.Add(sad)
	}
	// The result of SumAbsDiff places sums in elements 0 and 4 of the Uint16x8.
	// Reinterpret as Uint64x2 and sum the two halves.
	r := acc.AsUint64x2()
	return uint32(r.GetElem(0) + r.GetElem(1))
}

// checksumBlockGeneric is the scalar fallback when AVX is unavailable at runtime.
func checksumBlockGeneric(data []byte) uint32 {
	var sum uint32
	for _, c := range data {
		sum += uint32(c)
	}
	return sum
}
