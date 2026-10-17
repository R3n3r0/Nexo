//go:build !linux

package server

import "syscall"

// realMSS leaves the segments of a test connection as they are outside
// Linux (see mss_linux_test.go).
func realMSS(_, _ string, _ syscall.RawConn) error { return nil }
