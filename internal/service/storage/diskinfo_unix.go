//go:build !windows

package storage

import (
	"fmt"
	"syscall"
)

// statfs returns the usage of the file system holding path.
func statfs(path string) (total, free, usedInodes, freeInodes uint64, fsType string, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, 0, "", fmt.Errorf("statfs %s: %w", path, err)
	}
	bsize := uint64(st.Bsize)
	total = st.Blocks * bsize
	free = st.Bavail * bsize
	usedInodes = st.Files - st.Ffree
	freeInodes = st.Ffree
	return total, free, usedInodes, freeInodes, fsTypeName(st), nil
}
