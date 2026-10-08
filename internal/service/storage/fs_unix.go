//go:build !windows

package storage

import "syscall"

// oNoFollow makes open fail on a symbolic link instead of following it.
const oNoFollow = syscall.O_NOFOLLOW

// isRootDisk reports whether path lives on the file system mounted at "/".
func isRootDisk(path string) bool {
	var drive, root syscall.Stat_t
	if syscall.Stat(path, &drive) != nil || syscall.Stat("/", &root) != nil {
		return false
	}
	return drive.Dev == root.Dev
}
