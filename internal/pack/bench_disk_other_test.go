//go:build !unix

package pack_test

import "math"

// freeDiskBytes cannot be read here, so the 1M benchmarks are not guarded.
func freeDiskBytes(string) (uint64, error) { return math.MaxUint64, nil }
