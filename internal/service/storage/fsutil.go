package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/r3n3r0/nexo/internal/domain"
)

// reliableRetries bounds the retries of mkdirAll and renameAll when a
// concurrent removal of empty parents races with them.
const reliableRetries = 10

// mapErr translates operating system errors into domain sentinels, keeping
// the original error in the chain.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errSymlink), isDomainError(err):
		return err
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return fmt.Errorf("%w: %w", domain.ErrObjectNotFound, err)
	case errors.Is(err, syscall.ELOOP):
		return fmt.Errorf("%w: %w", errSymlink, err)
	case errors.Is(err, syscall.ENAMETOOLONG):
		return fmt.Errorf("%w: %w", domain.ErrInvalidObjectName, err)
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return fmt.Errorf("%w: %w", domain.ErrStorageFull, err)
	case errors.Is(err, syscall.EIO), errors.Is(err, syscall.EROFS), errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%w: %w", domain.ErrFaultyDisk, err)
	default:
		return err
	}
}

// isDomainError reports errors already expressed with a domain sentinel.
func isDomainError(err error) bool {
	for _, sentinel := range []error{
		domain.ErrObjectNotFound, domain.ErrBucketNotFound, domain.ErrDiskNotFound, domain.ErrFaultyDisk,
		domain.ErrStorageFull, domain.ErrInvalidObjectName, domain.ErrInvalidArgument, domain.ErrInvalidFormat,
		domain.ErrNoSuchVersion, domain.ErrInvalidRange,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// fsErr maps a file system error met while accessing volume/p: a missing
// entry is ErrDiskNotFound when the drive root vanished (unmounted or
// deleted drive), ErrBucketNotFound when the volume is missing and
// ErrObjectNotFound otherwise. Other errors go through mapErr.
func (s *xlStorage) fsErr(volume, p string, err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, syscall.EISDIR) {
		return fmt.Errorf("storage: %s/%s: %w", volume, p, mapErr(err))
	}
	if !s.rootExists() {
		return fmt.Errorf("%w: drive %s is gone: %w", domain.ErrDiskNotFound, s.root, err)
	}
	if volume != "" {
		if dir, derr := s.volumeDir(volume); derr == nil {
			if st, serr := os.Lstat(dir); serr != nil || !st.IsDir() {
				return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, volume)
			}
		}
	}
	return fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, volume, p)
}

// writeErr maps an error met while writing volume/p: a path component that
// is a file, or a destination that is a non-empty directory, makes the path
// unusable (ErrInvalidObjectName); other errors go through fsErr.
func (s *xlStorage) writeErr(volume, p string, err error) error {
	if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) ||
		errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("%w: %s/%s conflicts with an existing entry: %w", domain.ErrInvalidObjectName, volume, p, err)
	}
	if volume == "" {
		if !errors.Is(err, fs.ErrNotExist) {
			return mapErr(err)
		}
		if !s.rootExists() {
			return fmt.Errorf("%w: drive %s is gone: %w", domain.ErrDiskNotFound, s.root, err)
		}
		return mapErr(err)
	}
	return s.fsErr(volume, p, err)
}

// rootExists reports whether the drive directory is still there.
func (s *xlStorage) rootExists() bool {
	st, err := os.Stat(s.root)
	return err == nil && st.IsDir()
}

// ensureVolume checks that a volume directory exists.
func (s *xlStorage) ensureVolume(volume string) (string, error) {
	dir, err := s.volumeDir(volume)
	if err != nil {
		return "", err
	}
	st, err := os.Lstat(dir)
	switch {
	case err != nil:
		return "", s.fsErr(volume, "", err)
	case st.Mode()&fs.ModeSymlink != 0:
		return "", fmt.Errorf("%w: volume %s", errSymlink, volume)
	case !st.IsDir():
		return "", fmt.Errorf("%w: %s", domain.ErrBucketNotFound, volume)
	}
	return dir, nil
}

// mkdirAll creates dir and its missing parents up to base, which must
// exist: the drive never recreates a vanished volume or drive root (that
// would silently write to the parent file system of an unmounted drive).
// A parent removed concurrently by pruneEmptyDirs is created again.
func (s *xlStorage) mkdirAll(dir, base string) error {
	var err error
	for range reliableRetries {
		err = os.Mkdir(dir, fs.FileMode(s.opts.dirPerm))
		switch {
		case err == nil:
			// The entry of the new directory must reach the disk before
			// anything committed below it is acknowledged.
			s.syncDir(filepath.Dir(dir))
			return nil
		case errors.Is(err, fs.ErrExist):
			st, serr := os.Lstat(dir)
			if serr == nil {
				if st.IsDir() {
					return nil
				}
				return fmt.Errorf("mkdir %s: %w", dir, syscall.ENOTDIR)
			}
			continue // removed in between: try again
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		parent := filepath.Dir(dir)
		if dir == base || parent == dir || (parent != base && !strings.HasPrefix(parent, base+string(filepath.Separator))) {
			return err // the base itself is missing
		}
		if perr := s.mkdirAll(parent, base); perr != nil {
			return perr
		}
	}
	return err
}

// renameAll renames src to dst, creating the parents of dst below base and
// retrying when a concurrent prune removes them in between.
func (s *xlStorage) renameAll(src, dst, base string) error {
	var err error
	for range reliableRetries {
		if err = s.mkdirAll(filepath.Dir(dst), base); err != nil {
			return err
		}
		err = s.rename(src, dst)
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if _, serr := os.Lstat(src); serr != nil {
			return err // the source is really missing
		}
	}
	return err
}

// tmpPath returns a fresh path in the system tmp directory.
func (s *xlStorage) tmpPath() string {
	return filepath.Join(s.root, domain.SystemBucket, TmpDir, randomSuffix())
}

// tempFile creates a file in the system tmp directory, recreating the
// directory when it was removed.
func (s *xlStorage) tempFile() (*os.File, error) {
	var err error
	for range 2 {
		var f *os.File
		f, err = os.OpenFile(s.tmpPath(), os.O_RDWR|os.O_CREATE|os.O_EXCL|oNoFollow, fs.FileMode(s.opts.filePerm))
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			break
		}
		if !s.rootExists() {
			return nil, fmt.Errorf("%w: drive %s is gone: %w", domain.ErrDiskNotFound, s.root, err)
		}
		sys := filepath.Join(s.root, domain.SystemBucket)
		if merr := s.mkdirAll(filepath.Join(sys, TmpDir), s.root); merr != nil {
			return nil, fmt.Errorf("storage: recreate tmp: %w", mapErr(merr))
		}
	}
	return nil, fmt.Errorf("storage: create temporary file: %w", mapErr(err))
}

// writeTemp writes data to a new synced temporary file and returns its path.
func (s *xlStorage) writeTemp(data []byte, sync bool) (string, error) {
	f, err := s.tempFile()
	if err != nil {
		return "", err
	}
	return s.fillTemp(f, data, sync)
}

// stagedPrefix starts the name of a metadata file staged in its object
// directory (writeTempIn).
const stagedPrefix = ".staged-"

// writeTempIn writes data to a new synced file of dir (stagedPrefix and a
// random suffix) and returns its path.
func (s *xlStorage) writeTempIn(dir string, data []byte, sync bool) (string, error) {
	name := filepath.Join(dir, stagedPrefix+randomSuffix())
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|oNoFollow, fs.FileMode(s.opts.filePerm)) // #nosec G304 -- a directory of the drive, resolved by the storage layer
	if err != nil {
		return "", mapErr(err)
	}
	return s.fillTemp(f, data, sync)
}

// fillTemp writes data to the new file f, syncs it when asked and closes
// it; the file is removed on failure.
func (s *xlStorage) fillTemp(f *os.File, data []byte, sync bool) (string, error) {
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", mapErr(err)
	}
	if sync && s.opts.fsync {
		if err := f.Sync(); err != nil {
			_ = f.Close()
			_ = os.Remove(name)
			return "", mapErr(err)
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", mapErr(err)
	}
	return name, nil
}

// writeAtomic writes data to dst through a temporary file: write, fsync,
// rename, fsync of the parent directory. Readers see the old or the new
// content, never a partial one. Missing parents are created below base.
func (s *xlStorage) writeAtomic(dst, base string, data []byte, sync bool) error {
	tmp, err := s.writeTemp(data, sync)
	if err != nil {
		return err
	}
	if err := s.renameAll(tmp, dst, base); err != nil {
		_ = os.Remove(tmp)
		return mapErr(err)
	}
	if sync {
		s.syncDir(filepath.Dir(dst))
	}
	return nil
}

// commitFile syncs and closes tmp, renames it to dst (creating its parents
// below base) and syncs the parent directory.
func (s *xlStorage) commitFile(tmp *os.File, dst, base string) error {
	if s.opts.fsync {
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			return mapErr(err)
		}
	}
	if err := tmp.Close(); err != nil {
		return mapErr(err)
	}
	if err := s.renameAll(tmp.Name(), dst, base); err != nil {
		return mapErr(err)
	}
	s.syncDir(filepath.Dir(dst))
	return nil
}

// syncDir fsyncs a directory so that renames become durable (best effort).
func (s *xlStorage) syncDir(dir string) {
	// NTFS journals its directory entries and a directory handle cannot
	// be flushed: the open would only cost time.
	if !s.opts.fsync || runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir) // #nosec G304 -- a directory of a drive, resolved and checked by the storage layer
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// pruneBoundary returns the directory below which empty parents of a path
// may be removed: the volume directory, or the well-known top-level
// directory (tmp, multipart, buckets, ...) inside the system volume.
func (s *xlStorage) pruneBoundary(volume, name string) string {
	volDir, err := s.volumeDir(volume)
	if err != nil {
		return name
	}
	if volume != domain.SystemBucket {
		return volDir
	}
	rel, err := filepath.Rel(volDir, name)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return volDir
	}
	top := strings.SplitN(rel, string(filepath.Separator), 2)[0]
	return filepath.Join(volDir, top)
}

// pruneEmptyDirs removes dir and its empty parents up to (excluding) stop.
func (s *xlStorage) pruneEmptyDirs(dir, stop string) {
	for dir != stop && strings.HasPrefix(dir, stop+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// openFile opens a regular file for reading without following links.
func openFile(name string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|oNoFollow, 0) // #nosec G304 -- a file of a drive, resolved and checked by the storage layer (O_NOFOLLOW)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("open %s: %w", name, syscall.EISDIR)
	}
	return f, st, nil
}

// readFile reads a whole regular file without following links.
func readFile(name string) ([]byte, error) {
	f, st, err := openFile(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 0, st.Size()+1)
	for {
		n, err := f.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)] // grown concurrently: make room
		}
	}
}

func randomSuffix() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func fill(n, v int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// ctxReader stops a copy as soon as the context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// sectionReadCloser closes the file when the section is closed. It seeks
// (io.Seeker) within the section.
type sectionReadCloser struct {
	*io.SectionReader
	f *os.File
}

func (r *sectionReadCloser) Close() error { return r.f.Close() }

// WriteTo implements io.WriterTo. A writer that copies from readers itself
// (io.ReaderFrom) gets the rest of the section as an *io.LimitedReader
// over the file positioned at it: the response writer of a download
// hands that to sendfile, the kernel copying the page cache to the socket
// without a read into user space (zero-copy, see httpx.FileSection). The
// file is this stream's own (ReadFileStream opens one per stream), so
// moving its offset is safe. Any other writer gets the bytes copied.
func (r *sectionReadCloser) WriteTo(w io.Writer) (int64, error) {
	rf, ok := w.(io.ReaderFrom)
	if !ok {
		return io.Copy(w, struct{ io.Reader }{r.SectionReader})
	}
	pos, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	_, base, size := r.Outer()
	if pos >= size {
		return 0, nil
	}
	if _, err := r.f.Seek(base+pos, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := rf.ReadFrom(&io.LimitedReader{R: r.f, N: size - pos})
	if _, serr := r.Seek(n, io.SeekCurrent); err == nil {
		err = serr
	}
	return n, err
}
