//go:build goexperiment.simd && arm64

package fix2

import "simd/archsimd"

// checksumBlock returns the sum of all bytes in data as uint32.
//
// NEON is mandatory on arm64, so no runtime feature check is needed.
// Each 16-byte chunk is widened to two Uint16x8 halves and accumulated in
// uint16 lanes. A lane gains at most 2*255 per chunk, so it is flushed into
// uint32 lanes every 128 chunks (128*510 = 65280 <= 65535).
func checksumBlock(data []byte) uint32 {
	const chunksPerFlush = 128

	var acc32 archsimd.Uint32x4
	i := 0
	for i+16 <= len(data) {
		var acc16 archsimd.Uint16x8
		end := min(len(data), i+16*chunksPerFlush)
		for i+16 <= end {
			chunk := archsimd.LoadUint8x16(data[i:])
			acc16 = acc16.Add(chunk.ExtendLo8ToUint16()).Add(chunk.HiToLo().ExtendLo8ToUint16())
			i += 16
		}
		acc32 = acc32.Add(acc16.ExtendLo4ToUint32()).Add(acc16.HiToLo().ExtendLo4ToUint32())
	}
	// Tail.
	if i < len(data) {
		chunk, _ := archsimd.LoadUint8x16Part(data[i:])
		acc16 := chunk.ExtendLo8ToUint16().Add(chunk.HiToLo().ExtendLo8ToUint16())
		acc32 = acc32.Add(acc16.ExtendLo4ToUint32()).Add(acc16.HiToLo().ExtendLo4ToUint32())
	}
	return acc32.ReduceSum()
}
