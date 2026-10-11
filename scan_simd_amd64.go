//go:build goexperiment.simd && amd64 && go1.27

package fix2

import "simd/archsimd"

// bulkChecksum reports whether the parsers checksum a message's body with
// checksumBlock after parsing it rather than field by field as they go:
// a vector sum over the whole message, already in cache, is 2-15% faster
// (14 to 64 fields) than scalar sums interleaved with parsing.
const bulkChecksum = true

var hasAVX = archsimd.X86.AVX()

// checksumBlock returns the sum of all bytes in data as uint32.
func checksumBlock(data []byte) uint32 {
	if !hasAVX {
		return checksumBlockGeneric(data)
	}
	var zero archsimd.Uint8x16
	var acc archsimd.Uint64x2

	i := 0
	for i+16 <= len(data) {
		chunk := archsimd.LoadUint8x16(data[i:])
		// SumOf8AbsDiff against zero computes the sum of bytes in each 8-byte half.
		acc = acc.Add(chunk.SumOf8AbsDiff(zero))
		i += 16
	}
	// Tail.
	if i < len(data) {
		chunk, _ := archsimd.LoadUint8x16Part(data[i:])
		acc = acc.Add(chunk.SumOf8AbsDiff(zero))
	}
	return uint32(acc.GetElem(0) + acc.GetElem(1))
}
