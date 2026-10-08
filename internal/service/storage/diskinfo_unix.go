//go:build linux || darwin || freebsd

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
	bsize := toUint64(st.Bsize)
	total = toUint64(st.Blocks) * bsize
	free = toUint64(st.Bavail) * bsize
	freeInodes = toUint64(st.Ffree)
	if files := toUint64(st.Files); files > freeInodes {
		usedInodes = files - freeInodes
	}
	return total, free, usedInodes, freeInodes, fsTypeName(st), nil
}

// toUint64 converts the statfs fields, whose types differ between Unix
// flavors (negative values, possible on BSD for reserved blocks, are 0).
func toUint64[T ~int32 | ~int64 | ~uint32 | ~uint64](v T) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}
