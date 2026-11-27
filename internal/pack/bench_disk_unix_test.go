//go:build unix

package pack_test

import "syscall"

// freeDiskBytes is the space available to a normal user on the disk holding dir.
func freeDiskBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
