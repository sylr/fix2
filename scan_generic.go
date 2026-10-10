//go:build !goexperiment.simd || !(amd64 || arm64)

package fix2

// checksumBlock returns the sum of all bytes in data as uint32.
func checksumBlock(data []byte) uint32 {
	return checksumBlockGeneric(data)
}
