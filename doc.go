// Package fix2 implements a high-performance, zero-copy FIX protocol message parser.
//
// When built with GOEXPERIMENT=simd on amd64, the parser uses SIMD instructions
// (128-bit SSE/AVX vectors) for byte scanning and checksum computation.
// On all other configurations, it falls back to optimized generic Go code.
//
// All parsed field values are slices into the original input buffer (zero-copy).
package fix2
