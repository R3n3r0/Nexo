//go:build linux

package storage

import (
	"os"

	"golang.org/x/sys/unix"
)

// directIOSupported reports whether this platform writes the data files of
// a drive with O_DIRECT (see directWriter).
const directIOSupported = true

// setDirectIO sets (on) or clears O_DIRECT on an open file (fcntl
// F_SETFL). A file system without direct I/O refuses to set it with
// EINVAL.
func setDirectIO(f *os.File, on bool) error {
	sc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	err = sc.Control(func(fd uintptr) {
		flags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
		if err != nil {
			serr = err
			return
		}
		if on {
			flags |= unix.O_DIRECT
		} else {
			flags &^= unix.O_DIRECT
		}
		_, serr = unix.FcntlInt(fd, unix.F_SETFL, flags)
	})
	if err != nil {
		return err
	}
	return serr
}

// directIOAlign returns the alignment (of the memory, of the offsets and
// of the lengths) of the direct I/O on the regular file name, as its file
// system reports it (statx STATX_DIOALIGN, Linux 6.1+): ok is false when
// the file system reports that it has no direct I/O. A kernel or a file
// system that does not report it gets defaultDirectIOAlign, the first
// write then verifying it (EINVAL falls back to the page cache).
func directIOAlign(name string) (align int, ok bool) {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, name, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_DIOALIGN, &st); err != nil || st.Mask&unix.STATX_DIOALIGN == 0 {
		return defaultDirectIOAlign, true
	}
	if st.Dio_offset_align == 0 || st.Dio_mem_align == 0 {
		return 0, false
	}
	return int(max(st.Dio_mem_align, st.Dio_offset_align)), true
}
