package server

import "syscall"

// realMSS bounds the segments of a test connection to those of an Ethernet
// network (1460 bytes) instead of the 64 KiB of the loopback, before it
// connects (net.Dialer.Control). A receiver announces its window only once
// an MSS of it is free: with the loopback MSS a client that reads slowly
// gets the bytes in bursts a second or more apart, which no real network
// does.
func realMSS(_, _ string, c syscall.RawConn) error {
	var err error
	if cerr := c.Control(func(fd uintptr) {
		err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_MAXSEG, 1460)
	}); cerr != nil {
		return cerr
	}
	return err
}
