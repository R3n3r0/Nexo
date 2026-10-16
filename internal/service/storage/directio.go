package storage

import (
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// defaultDirectIOAlign is the alignment of direct I/O assumed when the file
// system does not report one: the page size, a multiple of the logical
// block size of every common device.
const defaultDirectIOAlign = 4096

// directIO is the O_DIRECT state of a local drive. The data files of the
// drive (parts, shards) larger than a copy buffer are written with
// O_DIRECT: the whole buffers bypass the page cache (no copy into it, no
// write-back of dirty pages competing with the next writes, no eviction of
// the hot data), the unaligned tail is written through the page cache
// after O_DIRECT is cleared, and the fsync before the commit is unchanged
// (it makes the metadata and the tail durable and flushes the device
// cache). Small files and xl.meta always go through the page cache.
type directIO struct {
	// align is the alignment of the I/O (memory, offsets, lengths); 0
	// when direct I/O is off for the drive (disabled by option, not
	// supported by the platform or the file system).
	align int
	// failed is set when the file system refused direct I/O at run time
	// (EINVAL: tmpfs before Linux 6.6, some ZFS and overlay mounts): the
	// drive then writes through the page cache.
	failed atomic.Bool
	// set sets or clears O_DIRECT on a file (setDirectIO; tests inject
	// failures).
	set func(f *os.File, on bool) error
}

// enabled reports whether the next data file may be written with O_DIRECT.
func (d *directIO) enabled() bool { return d.align > 0 && !d.failed.Load() }

// alignedBuffer returns a buffer of size bytes whose first byte is aligned
// to align (a power of two).
func alignedBuffer(size, align int) []byte {
	if align <= 1 {
		return make([]byte, size)
	}
	b := make([]byte, size+align)
	off := 0
	if r := int(uintptr(unsafe.Pointer(unsafe.SliceData(b))) & uintptr(align-1)); r != 0 { // #nosec G103 -- address arithmetic only, to align the buffer of O_DIRECT
		off = align - r
	}
	return b[off : off+size : off+size]
}

// isAligned reports whether p can be written with O_DIRECT: its address
// and its length are multiples of align.
func isAligned(p []byte, align int) bool {
	return len(p) > 0 && len(p)%align == 0 &&
		uintptr(unsafe.Pointer(unsafe.SliceData(p)))&uintptr(align-1) == 0 // #nosec G103 -- address arithmetic only
}

// directWriter writes a temporary data file of the drive (the file of
// CreateFile), switching it to O_DIRECT at the first whole aligned buffer
// written at an aligned offset, so only a file of at least a copy buffer
// goes direct, whatever the size the caller announced (erasure shards are
// written with an unknown size). A write that cannot go direct (the short
// tail) clears O_DIRECT first; the file system refusing direct I/O
// (EINVAL) clears it too, the write is completed through the page cache
// and the drive stops trying. Every byte reaches the file in order either
// way: the content is the same as with plain writes.
type directWriter struct {
	f   *os.File
	dio *directIO
	// write writes to f (f.Write; tests inject failures).
	write func(p []byte) (int, error)
	// onFail reports the first refusal of the file system.
	onFail func(err error)
	off    int64
	direct bool
	// tried is set once O_DIRECT was set or refused: a file switches to
	// direct I/O at most once.
	tried bool
}

func newDirectWriter(f *os.File, dio *directIO, onFail func(error)) *directWriter {
	return &directWriter{f: f, dio: dio, write: f.Write, onFail: onFail}
}

// fail disables direct I/O on the drive after a refusal of the file system.
func (w *directWriter) fail(err error) {
	if w.dio.failed.CompareAndSwap(false, true) && w.onFail != nil {
		w.onFail(err)
	}
}

// Write implements io.Writer.
func (w *directWriter) Write(p []byte) (int, error) {
	align := w.dio.align
	switch {
	case !w.tried && w.dio.enabled() && len(p) >= copyBufferSize && w.off%int64(align) == 0 && isAligned(p, align):
		w.tried = true
		if err := w.dio.set(w.f, true); err != nil {
			// Not a write error: the file is still a plain one.
			w.fail(err)
		} else {
			w.direct = true
		}
	case w.direct && !isAligned(p, align):
		if err := w.dio.set(w.f, false); err != nil {
			return 0, err
		}
		w.direct = false
	}
	n, err := w.write(p)
	w.off += int64(n)
	if err != nil && w.direct && errors.Is(err, syscall.EINVAL) {
		// Accepted at fcntl, refused at the I/O: the rest through the
		// page cache.
		w.fail(err)
		if cerr := w.dio.set(w.f, false); cerr != nil {
			return n, err
		}
		w.direct = false
		m, err := w.write(p[n:])
		w.off += int64(m)
		return n + m, err
	}
	return n, err
}
