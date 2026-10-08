//go:build windows

package storage

// oNoFollow is not supported on Windows; symbolic links are detected by
// the component checks of filePath only.
const oNoFollow = 0

// statfs is not implemented on Windows: usage figures are reported as zero
// and the file system type as unknown. Nexo servers target Unix hosts.
func statfs(string) (total, free, usedInodes, freeInodes uint64, fsType string, err error) {
	return 0, 0, 0, 0, "unknown", nil
}

// isRootDisk is not implemented on Windows.
func isRootDisk(string) bool { return false }
