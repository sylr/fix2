// Package fix2 implements a high-performance, zero-copy FIX protocol message parser.
//
// When built with GOEXPERIMENT=simd on amd64 (AVX) or arm64 (NEON), checksum
// computation uses 128-bit SIMD vectors via simd/archsimd. Byte scanning always
// delegates to bytes.IndexByte, which is already vectorized by the runtime.
// On all other configurations, it falls back to optimized generic Go code.
//
// All parsed field values are slices into the original input buffer (zero-copy).
package fix2
