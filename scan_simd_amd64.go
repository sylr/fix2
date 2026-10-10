//go:build goexperiment.simd && amd64

package fix2

import "simd/archsimd"

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
