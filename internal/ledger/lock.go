package ledger

import (
	"errors"
	"path/filepath"
)

// ErrLocked is returned by TryLock when another process holds the ledger lock.
var ErrLocked = errors.New("ledger is locked by another process")

// TryLock takes the ledger's advisory lock in dir without waiting, and returns
// the function that releases it. Store.Append and Store.Head wait for the same
// lock, so while it is held no writer can touch the file, and nothing in this
// process can append either. On platforms without file locks it always succeeds.
func TryLock(dir string) (func(), error) {
	return tryLockFile(filepath.Join(dir, LockFile))
}
