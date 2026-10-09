//go:build cluster && !linux

package cluster_test

import "syscall"

// sysProcAttr returns the attributes of the node processes.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

// roomy reports whether dir can hold the drives of a test deployment
// (only the tmpfs of Linux is considered).
func roomy(string) bool { return false }
