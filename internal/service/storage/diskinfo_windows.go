//go:build windows

package storage

// statfs is not implemented on Windows: usage figures are reported as zero
// and the file system type as unknown. Nexo servers target Unix hosts.
func statfs(string) (total, free, usedInodes, freeInodes uint64, fsType string, err error) {
	return 0, 0, 0, 0, "unknown", nil
}
