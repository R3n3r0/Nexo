//go:build darwin || freebsd

package storage

import "syscall"

// fsTypeName returns the file system name when the platform reports it.
func fsTypeName(st syscall.Statfs_t) string {
	n := 0
	for n < len(st.Fstypename) && st.Fstypename[n] != 0 {
		n++
	}
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = byte(st.Fstypename[i])
	}
	return string(b)
}
