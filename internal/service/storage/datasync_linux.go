//go:build linux

package storage

import (
	"os"
	"syscall"
)

// datasync flushes the data of f and the metadata needed to read it back
// (its size), without its timestamps.
func datasync(f *os.File) error { return syscall.Fdatasync(int(f.Fd())) } // #nosec G115 -- a file descriptor

// totalMemory returns the RAM of the machine in bytes (0 when unknown).
func totalMemory() uint64 {
	var info syscall.Sysinfo_t
	if syscall.Sysinfo(&info) != nil {
		return 0
	}
	return uint64(info.Totalram) * uint64(info.Unit) // #nosec G115 -- sizes reported by the kernel
}
