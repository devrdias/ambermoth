//go:build !unix

package ledger

// lockFile is a no-op on non-unix platforms; run a single writer there.
func lockFile(path string) (func(), error) { return func() {}, nil }

func tryLockFile(path string) (func(), error) { return func() {}, nil }
