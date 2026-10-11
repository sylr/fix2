//go:build !goexperiment.simd || !(amd64 || arm64) || !go1.27

package fix2

// bulkChecksum reports whether the parsers checksum a message's body with
// checksumBlock after parsing it rather than field by field as they go.
// Without SIMD, summing field by field is as fast or faster. The SIMD
// implementations use the simd/archsimd API of Go 1.27: with an older Go,
// GOEXPERIMENT=simd builds this one.
const bulkChecksum = false

// checksumBlock returns the sum of all bytes in data as uint32.
func checksumBlock(data []byte) uint32 {
	return checksumBlockGeneric(data)
}
