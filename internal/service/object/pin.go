package object

import (
	"context"
	"io"
	"runtime"

	"github.com/r3n3r0/nexo/internal/service/crypto"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// pinnedReads reports whether an open file keeps its content after it is
// removed (POSIX unlink semantics): a read that opened every file it
// needs then reads the version it resolved even after a writer replaced
// it and purged its data directory, which makes the data directory of a
// version an MVCC snapshot. On Windows an open file cannot be removed
// (the purge would fail and leak the replaced data), so reads keep their
// lock until their body is closed.
const pinnedReads = runtime.GOOS != "windows"

// readLock is the read lock of a GetObject: release frees it; free is the
// request context without the lock, the context of the streams of a read
// that pinned its data, since the lock-bound one is canceled on release.
type readLock struct {
	release func()
	free    context.Context
}

// pinnedStream is a stream opened up front for the read at offset (of the
// plaintext for encrypted versions).
type pinnedStream struct {
	off int64
	rc  io.ReadCloser
}

// pinnable is a reader of a version that can open up front every stream
// it will read.
type pinnable interface {
	io.ReadCloser
	// pin opens the streams of the whole range, at most max of them, and
	// reports whether every one is open and pinned (store.OpenData). On
	// false nothing is kept open and the reader opens its streams lazily.
	pin(max int) bool
}

// closePinned closes the streams of ps.
func closePinned(ps []pinnedStream) {
	for _, p := range ps {
		_ = p.rc.Close()
	}
}

// takePinned returns the first stream of *ps when it was opened for a
// read at off; otherwise the pinned streams no longer match the reads (a
// stream ended early) and are dropped, the reader opening its streams
// lazily from there.
func takePinned(ps *[]pinnedStream, off int64) io.ReadCloser {
	if len(*ps) == 0 {
		return nil
	}
	if (*ps)[0].off != off {
		closePinned(*ps)
		*ps = nil
		return nil
	}
	rc := (*ps)[0].rc
	*ps = (*ps)[1:]
	return rc
}

// openPinned opens the reader of [offset, offset+length) of fi with every
// stream it needs already open, in the context free (not bound to the read
// lock), when the store pins them and the range spans at most
// pinnedReadParts parts. ok is false when the read cannot be pinned: the
// caller then reads under its lock.
func (l *layer) openPinned(free context.Context, st store, bucket, key string, fi storage.FileInfo, okey *crypto.ObjectKey, offset, length int64) (io.ReadCloser, bool) {
	if l.opts.pinnedReadParts <= 0 {
		return nil, false
	}
	var r pinnable
	switch {
	case okey != nil:
		r = l.openEncrypted(free, st, bucket, key, fi, okey, offset, length)
	case length > 0 && isCompressed(fi.Metadata):
		r = l.openCompressed(free, st, bucket, key, fi, offset, length)
	case length <= 0 || fi.DataDir == "":
		// Inline data is in memory: nothing to open.
		rc, err := l.openData(free, st, bucket, key, fi, offset, length)
		return rc, err == nil
	default:
		r = &partsReader{ctx: free, st: st, bucket: bucket, key: key, fi: fi, offset: offset, remaining: length}
	}
	if !r.pin(l.opts.pinnedReadParts) {
		return nil, false
	}
	return r, true
}
