package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/crypto"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// nullVersion is the S3 name of the version written while versioning is
// off or suspended; it is stored with the empty id.
const nullVersion = "null"

// readVersion resolves the version a read refers to: the latest one for
// an empty id, the null version for "null". Delete markers map to the S3
// semantics (latest marker → not found, explicit marker → method not
// allowed).
func (l *layer) readVersion(ctx context.Context, st store, bucket, key, versionID string, readData bool) (storage.FileInfo, error) {
	fi, err := st.ReadVersion(ctx, bucket, key, versionID, readData)
	if err != nil {
		if isNotFound(err) {
			// Distinguish a missing bucket from a missing key.
			if serr := l.statBucket(ctx, st, bucket); serr != nil {
				return storage.FileInfo{}, serr
			}
			if versionID != "" {
				// No version of a missing key exists: S3 answers
				// NoSuchVersion to a request naming one.
				return storage.FileInfo{}, fmt.Errorf("%w: version %s of %s/%s (no such key)", domain.ErrNoSuchVersion, versionID, bucket, key)
			}
		}
		return storage.FileInfo{}, err
	}
	if fi.Deleted {
		marker := &domain.DeleteMarkerError{VersionID: objectInfo(bucket, key, fi).VersionID, ModTime: fi.ModTime.UTC()}
		if versionID == "" {
			marker.Err = domain.NewS3Error(domain.ErrObjectNotFound, "NoSuchKey", "The specified key does not exist.").WithResource(bucket + "/" + key)
		} else {
			marker.Err = fmt.Errorf("%w: version %s of %s/%s is a delete marker", domain.ErrMethodNotAllowed, versionID, bucket, key)
		}
		return fi, marker
	}
	return fi, nil
}

// partChecksum returns the additional checksum of part n (1-based) of a
// multipart version, zero when the part has none.
func partChecksum(fi storage.FileInfo, n int) domain.Checksum {
	if n < 1 || n > len(fi.Parts) {
		return domain.Checksum{}
	}
	for algo, value := range fi.Parts[n-1].Checksums {
		if crypto.IsSealedValue(value) {
			// Sealed (encrypted version, openChecksums not applied): only
			// the algorithm is known.
			value = ""
		}
		return domain.Checksum{Algorithm: domain.ChecksumAlgorithm(algo), Value: value}
	}
	return domain.Checksum{}
}

// infoForRead describes the version a read serves: the read of one part
// of a multipart object (partNumber) reports the checksum of that part,
// with the checksum type of the object (COMPOSITE or FULL_OBJECT).
func infoForRead(bucket, key string, fi storage.FileInfo, opts GetObjectOptions) domain.ObjectInfo {
	info := objectInfo(bucket, key, fi)
	if opts.PartNumber > 0 && info.Parts > 0 {
		typ := info.Checksum.Type
		info.Checksum = partChecksum(fi, opts.PartNumber)
		if !info.Checksum.IsZero() {
			info.Checksum.Type = typ
		}
	}
	return info
}

// GetObjectInfo implements Service.
func (l *layer) GetObjectInfo(ctx context.Context, bucket, key string, opts GetObjectOptions) (domain.ObjectInfo, error) {
	if err := validObject(bucket, key); err != nil {
		return domain.ObjectInfo{}, err
	}
	if !opts.NoLock {
		lctx, unlock, err := l.lock(ctx, bucket, key, false)
		if err != nil {
			return domain.ObjectInfo{}, err
		}
		defer unlock()
		ctx = lctx
	}
	st, err := l.storeFor(ctx, bucket, key, opts.VersionID)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: head %s/%s: %w", l.name, bucket, key, err)
	}
	fi, err := l.readVersion(ctx, st, bucket, key, opts.VersionID, false)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: head %s/%s: %w", l.name, bucket, key, err)
	}
	okey, enc, err := l.describeKey(ctx, bucket, key, fi.Metadata, opts.SSECustomerKey, opts.ChecksumMode && hasSealedChecksums(fi))
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	if fi, err = openChecksums(okey, fi); err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: head %s/%s: %w", l.name, bucket, key, err)
	}
	info := infoForRead(bucket, key, fi, opts)
	info.Encryption = enc
	l.labelNull(ctx, bucket, &info)
	if opts.PartNumber > 0 {
		if _, _, err := resolveRange(fi, opts); err != nil {
			return info, err
		}
	}
	if err := checkReadConditions(info, opts.Conditions); err != nil {
		return info, err
	}
	return info, nil
}

// GetObject implements Service. The read lock is held while the version
// is resolved and its data opened. When the read can open up front every
// file it needs (inline data, or at most WithPinnedReadParts parts, see
// pinnedReads), the lock is released before the body is returned: the
// read goes on from the open files even if a writer replaces the version
// and purges its data meanwhile, so a slow or stalled download never
// blocks the writers of the key. Otherwise the lock is held until the body
// is closed, so that the version being read is never purged under it; the
// body then fails if the lock is lost meanwhile.
func (l *layer) GetObject(ctx context.Context, bucket, key string, opts GetObjectOptions) (*Reader, error) {
	if err := validObject(bucket, key); err != nil {
		return nil, err
	}
	rctx := ctx
	var lk *readLock
	if !opts.NoLock {
		lctx, release, err := l.lock(ctx, bucket, key, false)
		if err != nil {
			return nil, err
		}
		rctx, lk = lctx, &readLock{release: release, free: ctx}
	}
	r, _, err := l.openObject(rctx, bucket, key, opts, lk)
	if err != nil {
		return nil, err
	}
	l.publish(ctx, event.ObjectAccessedGet, bucket, r.Info)
	return r, nil
}

// openObject resolves the version and the range of a read, evaluates the
// read conditions of opts and opens the data. lk, when not nil, is the
// read lock of the caller: it is released before returning when the data
// could be pinned (see GetObject), when the reader is closed otherwise,
// and before returning on failure. The returned FileInfo carries the raw
// metadata of the version.
func (l *layer) openObject(ctx context.Context, bucket, key string, opts GetObjectOptions, lk *readLock) (*Reader, storage.FileInfo, error) {
	release := func() {
		if lk != nil {
			lk.release()
		}
	}
	st, err := l.storeFor(ctx, bucket, key, opts.VersionID)
	if err != nil {
		release()
		return nil, storage.FileInfo{}, fmt.Errorf("%s: get %s/%s: %w", l.name, bucket, key, err)
	}
	fi, err := l.readVersion(ctx, st, bucket, key, opts.VersionID, true)
	if err != nil {
		release()
		return nil, storage.FileInfo{}, fmt.Errorf("%s: get %s/%s: %w", l.name, bucket, key, err)
	}
	okey, err := l.unsealFor(ctx, bucket, key, fi.Metadata, opts.SSECustomerKey)
	if err != nil {
		release()
		return nil, storage.FileInfo{}, err
	}
	if fi, err = openChecksums(okey, fi); err != nil {
		release()
		return nil, storage.FileInfo{}, fmt.Errorf("%s: get %s/%s: %w", l.name, bucket, key, err)
	}
	info := infoForRead(bucket, key, fi, opts)
	if okey != nil {
		info.Encryption = okey.Info()
	}
	l.labelNull(ctx, bucket, &info)
	if err := checkReadConditions(info, opts.Conditions); err != nil {
		release()
		return nil, storage.FileInfo{}, err
	}
	offset, length, err := resolveRange(fi, opts)
	if err != nil {
		release()
		return nil, storage.FileInfo{}, err
	}
	if lk != nil {
		if rc, ok := l.openPinned(lk.free, st, bucket, key, fi, okey, offset, length); ok {
			if err := checkLocks(ctx); err != nil {
				// The lock was lost while the data was opened: a writer
				// may have replaced the version in between.
				_ = rc.Close()
				release()
				return nil, storage.FileInfo{}, err
			}
			release()
			fi.Data = nil
			return &Reader{ReadCloser: rc, Info: info, Offset: offset, Length: length}, fi, nil
		}
	}
	var rc io.ReadCloser
	if okey != nil {
		rc = l.openEncrypted(ctx, st, bucket, key, fi, okey, offset, length)
	} else if rc, err = l.openData(ctx, st, bucket, key, fi, offset, length); err != nil {
		release()
		return nil, storage.FileInfo{}, fmt.Errorf("%s: read %s/%s: %w", l.name, bucket, key, err)
	}
	if lk != nil {
		rc = &unlockOnClose{ReadCloser: rc, unlock: lk.release}
	}
	fi.Data = nil
	return &Reader{ReadCloser: rc, Info: info, Offset: offset, Length: length}, fi, nil
}

// resolveRange turns the range or part number of a read into the
// offset/length of the plaintext (of the content of an unencrypted
// version).
func resolveRange(fi storage.FileInfo, opts GetObjectOptions) (int64, int64, error) {
	if opts.PartNumber > 0 {
		if opts.Range != nil {
			return 0, 0, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", "Cannot specify both Range header and partNumber query parameter")
		}
		var start int64
		for i, p := range fi.Parts {
			size := p.Size
			if crypto.IsEncrypted(fi.Metadata) {
				size = p.ActualSize
			}
			if i+1 == opts.PartNumber {
				return start, size, nil
			}
			start += size
		}
		return 0, 0, domain.NewS3Error(domain.ErrInvalidRange, "InvalidPartNumber", "The requested partnumber is not satisfiable")
	}
	size := actualSize(fi)
	if opts.Range == nil {
		return 0, size, nil
	}
	return opts.Range.Resolve(size)
}

// openData returns a reader over [offset, offset+length) of a version.
func (l *layer) openData(ctx context.Context, st store, bucket, key string, fi storage.FileInfo, offset, length int64) (io.ReadCloser, error) {
	if length <= 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if fi.DataDir == "" {
		if offset+length > int64(len(fi.Data)) {
			return nil, fmt.Errorf("%w: inline data of %d bytes, want [%d,%d)", domain.ErrFaultyDisk, len(fi.Data), offset, offset+length)
		}
		return io.NopCloser(bytes.NewReader(fi.Data[offset : offset+length])), nil
	}
	return &partsReader{ctx: ctx, st: st, bucket: bucket, key: key, fi: fi, offset: offset, remaining: length}, nil
}

// partsReader streams a byte range across the parts of a version, opening
// one part at a time (or taking the streams pin opened up front).
type partsReader struct {
	ctx       context.Context
	st        store
	bucket    string
	key       string
	fi        storage.FileInfo
	offset    int64
	remaining int64
	cur       io.ReadCloser
	pinned    []pinnedStream
}

func (r *partsReader) Read(p []byte) (int, error) {
	for {
		if r.remaining <= 0 {
			return 0, io.EOF
		}
		if r.cur == nil {
			if err := r.openNext(); err != nil {
				return 0, err
			}
		}
		if int64(len(p)) > r.remaining {
			p = p[:r.remaining]
		}
		n, err := r.cur.Read(p)
		r.offset += int64(n)
		r.remaining -= int64(n)
		if errors.Is(err, io.EOF) {
			_ = r.cur.Close()
			r.cur = nil
			if n == 0 {
				continue
			}
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("object: read %s/%s: %w", r.bucket, r.key, lockErr(r.ctx, err))
		}
		return n, nil
	}
}

// locatePart returns the part holding offset, the offset in it and the
// length to read from it (bounded by remaining); ok is false when offset
// is beyond the parts.
func locatePart(fi storage.FileInfo, offset, remaining int64) (part storage.PartInfo, partOff, partLen int64, ok bool) {
	var start int64
	for _, part := range fi.Parts {
		if part.Size == 0 {
			continue
		}
		if offset >= start && offset < start+part.Size {
			partOff = offset - start
			return part, partOff, min(part.Size-partOff, remaining), true
		}
		start += part.Size
	}
	return storage.PartInfo{}, 0, 0, false
}

func (r *partsReader) openNext() error {
	part, partOff, partLen, ok := locatePart(r.fi, r.offset, r.remaining)
	if !ok {
		return fmt.Errorf("%w: offset %d beyond the parts of %s/%s", domain.ErrFaultyDisk, r.offset, r.bucket, r.key)
	}
	if rc := takePinned(&r.pinned, r.offset); rc != nil {
		r.cur = rc
		return nil
	}
	rc, err := r.st.ReadData(r.ctx, r.bucket, r.key, r.fi, part, partOff, partLen)
	if err != nil {
		return fmt.Errorf("object: open part %d of %s/%s: %w", part.Number, r.bucket, r.key, lockErr(r.ctx, err))
	}
	r.cur = rc
	return nil
}

// partsCovered returns the number of parts [offset, offset+length) of fi
// spans, or -1 when the range goes beyond its parts (no I/O).
func partsCovered(fi storage.FileInfo, offset, length int64) int {
	n := 0
	for off, rem := offset, length; rem > 0; n++ {
		_, _, partLen, ok := locatePart(fi, off, rem)
		if !ok || partLen <= 0 {
			return -1
		}
		off, rem = off+partLen, rem-partLen
	}
	return n
}

// pin implements pinnable. A range over more than max parts is read under
// the lock without opening anything first.
func (r *partsReader) pin(max int) bool {
	if n := partsCovered(r.fi, r.offset, r.remaining); n < 0 || n > max {
		return false
	}
	var opened []pinnedStream
	for off, rem := r.offset, r.remaining; rem > 0; {
		part, partOff, partLen, ok := locatePart(r.fi, off, rem)
		if !ok {
			closePinned(opened)
			return false
		}
		rc, pinned, err := r.st.OpenData(r.ctx, r.bucket, r.key, r.fi, part, partOff, partLen)
		if err != nil {
			closePinned(opened)
			return false
		}
		opened = append(opened, pinnedStream{off: off, rc: rc})
		if !pinned {
			closePinned(opened)
			return false
		}
		off, rem = off+partLen, rem-partLen
	}
	r.pinned = opened
	return true
}

func (r *partsReader) Close() error {
	closePinned(r.pinned)
	r.pinned = nil
	if r.cur != nil {
		err := r.cur.Close()
		r.cur = nil
		return err
	}
	return nil
}

// unlockOnClose releases the read lock when the body is closed.
type unlockOnClose struct {
	io.ReadCloser
	unlock func()
	done   bool
}

func (u *unlockOnClose) Close() error {
	err := u.ReadCloser.Close()
	if !u.done {
		u.done = true
		u.unlock()
	}
	return err
}
