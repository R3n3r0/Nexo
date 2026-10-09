//go:build cluster && linux

package cluster_test

import "syscall"

// sysProcAttr kills the node processes with the test binary (a test that
// times out does not leave servers behind).
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// roomy reports whether dir is a writable directory of a file system with
// at least 2 GiB and 20% free.
func roomy(dir string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil || syscall.Access(dir, 2) != nil {
		return false
	}
	free := st.Bavail * uint64(st.Bsize)
	total := st.Blocks * uint64(st.Bsize)
	return free >= 2<<30 && free*5 >= total
}
