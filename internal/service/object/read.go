package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/r3n3r0/nexo/internal/domain"
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
		if isNotFound(err) && versionID == "" {
			// Distinguish a missing bucket from a missing key.
			if serr := l.statBucket(ctx, st, bucket); serr != nil {
				return storage.FileInfo{}, serr
			}
		}
		return storage.FileInfo{}, err
	}
	if fi.Deleted {
		if versionID == "" {
			return fi, domain.NewS3Error(domain.ErrObjectNotFound, "NoSuchKey", "The specified key does not exist.").WithResource(bucket + "/" + key)
		}
		return fi, fmt.Errorf("%w: version %s of %s/%s is a delete marker", domain.ErrMethodNotAllowed, versionID, bucket, key)
	}
	return fi, nil
}

// GetObjectInfo implements Service.
func (l *layer) GetObjectInfo(ctx context.Context, bucket, key string, opts GetObjectOptions) (domain.ObjectInfo, error) {
	if err := validObject(bucket, key); err != nil {
		return domain.ObjectInfo{}, err
	}
	if !opts.NoLock {
		lc, err := l.lock(ctx, bucket, key, false)
		if err != nil {
			return domain.ObjectInfo{}, err
		}
		defer lc.Unlock(ctx)
	}
	fi, err := l.readVersion(ctx, l.pick(key), bucket, key, opts.VersionID, false)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: head %s/%s: %w", l.name, bucket, key, err)
	}
	info := objectInfo(bucket, key, fi)
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

// GetObject implements Service. The read lock is held until the returned
// body is closed, so that the version being read is never purged under it.
func (l *layer) GetObject(ctx context.Context, bucket, key string, opts GetObjectOptions) (*Reader, error) {
	if err := validObject(bucket, key); err != nil {
		return nil, err
	}
	var unlock func()
	if !opts.NoLock {
		lc, err := l.lock(ctx, bucket, key, false)
		if err != nil {
			return nil, err
		}
		unlock = func() { lc.Unlock(ctx) }
	}
	r, _, err := l.openObject(ctx, bucket, key, opts, unlock)
	if err != nil {
		return nil, err
	}
	l.publish(ctx, event.ObjectAccessedGet, bucket, r.Info)
	return r, nil
}

// openObject resolves the version and the range of a read, evaluates the
// read conditions of opts and opens the data. unlock, when not nil, runs
// when the reader is closed, or before returning on failure. The returned
// FileInfo carries the raw metadata of the version.
func (l *layer) openObject(ctx context.Context, bucket, key string, opts GetObjectOptions, unlock func()) (*Reader, storage.FileInfo, error) {
	release := func() {
		if unlock != nil {
			unlock()
		}
	}
	st := l.pick(key)
	fi, err := l.readVersion(ctx, st, bucket, key, opts.VersionID, true)
	if err != nil {
		release()
		return nil, storage.FileInfo{}, fmt.Errorf("%s: get %s/%s: %w", l.name, bucket, key, err)
	}
	info := objectInfo(bucket, key, fi)
	if err := checkReadConditions(info, opts.Conditions); err != nil {
		release()
		return nil, storage.FileInfo{}, err
	}
	offset, length, err := resolveRange(fi, opts)
	if err != nil {
		release()
		return nil, storage.FileInfo{}, err
	}
	rc, err := l.openData(ctx, st, bucket, key, fi, offset, length)
	if err != nil {
		release()
		return nil, storage.FileInfo{}, fmt.Errorf("%s: read %s/%s: %w", l.name, bucket, key, err)
	}
	if unlock != nil {
		rc = &unlockOnClose{ReadCloser: rc, unlock: unlock}
	}
	fi.Data = nil
	return &Reader{ReadCloser: rc, Info: info, Offset: offset, Length: length}, fi, nil
}

// resolveRange turns the range or part number of a read into offset/length.
func resolveRange(fi storage.FileInfo, opts GetObjectOptions) (int64, int64, error) {
	if opts.PartNumber > 0 {
		if opts.Range != nil {
			return 0, 0, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", "Cannot specify both Range header and partNumber query parameter")
		}
		var start int64
		for i, p := range fi.Parts {
			if i+1 == opts.PartNumber {
				return start, p.Size, nil
			}
			start += p.Size
		}
		return 0, 0, domain.NewS3Error(domain.ErrInvalidRange, "InvalidPartNumber", "The requested partnumber is not satisfiable")
	}
	if opts.Range == nil {
		return 0, fi.Size, nil
	}
	return opts.Range.Resolve(fi.Size)
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
// one part at a time.
type partsReader struct {
	ctx       context.Context
	st        store
	bucket    string
	key       string
	fi        storage.FileInfo
	offset    int64
	remaining int64
	cur       io.ReadCloser
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
			return n, fmt.Errorf("object: read %s/%s: %w", r.bucket, r.key, err)
		}
		return n, nil
	}
}

func (r *partsReader) openNext() error {
	var start int64
	for _, part := range r.fi.Parts {
		if part.Size == 0 {
			continue
		}
		if r.offset >= start && r.offset < start+part.Size {
			partOff := r.offset - start
			partLen := part.Size - partOff
			if partLen > r.remaining {
				partLen = r.remaining
			}
			rc, err := r.st.ReadData(r.ctx, r.bucket, r.key, r.fi, part, partOff, partLen)
			if err != nil {
				return fmt.Errorf("object: open part %d of %s/%s: %w", part.Number, r.bucket, r.key, err)
			}
			r.cur = rc
			return nil
		}
		start += part.Size
	}
	return fmt.Errorf("%w: offset %d beyond the parts of %s/%s", domain.ErrFaultyDisk, r.offset, r.bucket, r.key)
}

func (r *partsReader) Close() error {
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
