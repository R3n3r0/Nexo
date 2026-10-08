//go:build !windows && !linux && !darwin && !freebsd

package storage

// statfs is not implemented on this platform: usage figures are reported
// as zero and the file system type as unknown.
func statfs(string) (total, free, usedInodes, freeInodes uint64, fsType string, err error) {
	return 0, 0, 0, 0, "unknown", nil
}
