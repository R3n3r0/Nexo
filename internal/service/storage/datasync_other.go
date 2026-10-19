//go:build !linux

package storage

import "os"

// datasync flushes f (fsync; FlushFileBuffers on Windows).
func datasync(f *os.File) error { return f.Sync() }

// totalMemory returns the RAM of the machine in bytes (0 when unknown).
func totalMemory() uint64 { return 0 }
