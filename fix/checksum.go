package fix

// checksumBlockGeneric is the scalar byte sum, used directly when no SIMD
// implementation is available and as the fallback for unsupported CPUs.
func checksumBlockGeneric(data []byte) uint32 {
	var sum uint32
	for _, c := range data {
		sum += uint32(c)
	}
	return sum
}
