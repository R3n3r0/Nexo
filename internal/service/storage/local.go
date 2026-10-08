package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// xlDirSuffix replaces the trailing slash of "directory object" keys
// ("photos/") on disk, so that the object directory does not collide with
// the prefix of the same name.
const xlDirSuffix = "__XLDIR__"

// xlStorage is StorageAPI over a local drive directory. Objects live in
// <root>/<volume>/<key>/xl.meta with their parts in <root>/<volume>/<key>/
// <dataDir>/part.N; every write goes through a temporary file in the system
// tmp directory followed by fsync and an atomic rename.
type xlStorage struct {
	deps Deps
	opts options
	ep   domain.Endpoint
	root string

	mu     sync.RWMutex
	closed bool
	diskID string
}

var _ StorageAPI = (*xlStorage)(nil)

// NewLocal opens (creating it when missing) the drive directory of a local
// endpoint, prepares the system directories (.nexo.sys, tmp, multipart,
// buckets), empties tmp, probes writability and loads the drive id from
// format.json when present. The drive is left unformatted when no
// format.json exists: Formatter.EnsureFormats writes it.
func NewLocal(deps Deps, ep domain.Endpoint, opts ...Option) (StorageAPI, error) {
	if ep.IsURL() || ep.Path == "" {
		return nil, fmt.Errorf("%w: storage: endpoint %q is not a local path", domain.ErrInvalidArgument, ep.String())
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	root, err := filepath.Abs(ep.Path)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve %s: %w", ep.Path, err)
	}
	s := &xlStorage{deps: deps, opts: o, ep: ep, root: root}
	if err := s.initDirs(); err != nil {
		return nil, err
	}
	if err := s.probe(); err != nil {
		return nil, err
	}
	s.diskID = s.loadDiskID()
	deps.Logger.Debug("storage: local drive ready", "path", root, "disk_id", s.diskID)
	return s, nil
}

func (s *xlStorage) initDirs() error {
	perm := fs.FileMode(s.opts.dirPerm)
	if err := os.MkdirAll(s.root, perm); err != nil {
		return fmt.Errorf("storage: create drive %s: %w", s.root, mapErr(err))
	}
	sys := filepath.Join(s.root, domain.SystemBucket)
	for _, d := range []string{sys, filepath.Join(sys, TmpDir), filepath.Join(sys, MultipartDir), filepath.Join(sys, BucketsDir)} {
		if err := os.MkdirAll(d, perm); err != nil {
			return fmt.Errorf("storage: create %s: %w", d, mapErr(err))
		}
	}
	if s.opts.cleanTmp {
		tmp := filepath.Join(sys, TmpDir)
		entries, err := os.ReadDir(tmp)
		if err != nil {
			return fmt.Errorf("storage: read %s: %w", tmp, mapErr(err))
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(tmp, e.Name())); err != nil {
				s.deps.Logger.Warn("storage: cannot remove stale temporary entry", "path", filepath.Join(tmp, e.Name()), "err", err)
			}
		}
	}
	return nil
}

// probe verifies that the drive accepts writes.
func (s *xlStorage) probe() error {
	name := filepath.Join(s.root, domain.SystemBucket, TmpDir, ".probe-"+randomSuffix())
	if err := os.WriteFile(name, []byte("nexo"), fs.FileMode(s.opts.filePerm)); err != nil {
		return fmt.Errorf("storage: drive %s is not writable: %w", s.root, mapErr(err))
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("storage: drive %s: remove probe: %w", s.root, mapErr(err))
	}
	return nil
}

func (s *xlStorage) loadDiskID() string {
	data, err := os.ReadFile(filepath.Join(s.root, domain.SystemBucket, FormatFile))
	if err != nil {
		return ""
	}
	f, err := topology.UnmarshalFormat(data)
	if err != nil {
		s.deps.Logger.Warn("storage: unreadable format.json", "path", s.root, "err", err)
		return ""
	}
	return f.Erasure.This
}

// Endpoint implements StorageAPI.
func (s *xlStorage) Endpoint() domain.Endpoint { return s.ep }

// IsOnline implements StorageAPI.
func (s *xlStorage) IsOnline() bool {
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return false
	}
	st, err := os.Stat(s.root)
	return err == nil && st.IsDir()
}

// Healing implements StorageAPI: local drives report no healing state yet.
func (s *xlStorage) Healing() *HealingTracker { return nil }

// Close implements StorageAPI.
func (s *xlStorage) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *xlStorage) checkOpen(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return fmt.Errorf("%w: drive %s is closed", domain.ErrDiskNotFound, s.root)
	}
	return nil
}

// DiskInfo implements StorageAPI.
func (s *xlStorage) DiskInfo(ctx context.Context, opts DiskInfoOptions) (domain.DiskInfo, error) {
	if err := s.checkOpen(ctx); err != nil {
		return domain.DiskInfo{}, err
	}
	s.mu.Lock()
	if s.diskID == "" {
		s.diskID = s.loadDiskID()
	}
	id := s.diskID
	s.mu.Unlock()
	info := domain.DiskInfo{
		Endpoint: s.ep.String(),
		Path:     s.root,
		ID:       id,
		PoolIdx:  s.ep.PoolIdx,
		SetIdx:   s.ep.SetIdx,
		DiskIdx:  s.ep.DiskIdx,
		State:    domain.StateOnline,
	}
	if opts.DiskID != "" && opts.DiskID != id {
		return info, fmt.Errorf("%w: drive %s has id %q, want %q", domain.ErrDiskNotFound, s.root, id, opts.DiskID)
	}
	if !opts.NoOp {
		total, free, usedInodes, freeInodes, fsType, err := statfs(s.root)
		if err != nil {
			info.State = domain.StateOffline
			info.Error = err.Error()
			return info, fmt.Errorf("%w: %v", domain.ErrFaultyDisk, err)
		}
		info.Total, info.Free, info.UsedInodes, info.FreeInodes, info.FSType = total, free, usedInodes, freeInodes, fsType
		if total >= free {
			info.Used = total - free
		}
	}
	return info, nil
}

// MakeVol implements StorageAPI.
func (s *xlStorage) MakeVol(ctx context.Context, volume string) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	dir, err := s.volumeDir(volume)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, fs.FileMode(s.opts.dirPerm)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", domain.ErrBucketAlreadyExists, volume)
		}
		return fmt.Errorf("storage: make volume %s: %w", volume, mapErr(err))
	}
	s.syncDir(s.root)
	return nil
}

// ListVols implements StorageAPI.
func (s *xlStorage) ListVols(ctx context.Context) ([]VolInfo, error) {
	if err := s.checkOpen(ctx); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("storage: list volumes: %w", mapErr(err))
	}
	out := make([]VolInfo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, VolInfo{Name: e.Name(), Created: info.ModTime()})
	}
	return out, nil
}

// StatVol implements StorageAPI.
func (s *xlStorage) StatVol(ctx context.Context, volume string) (VolInfo, error) {
	if err := s.checkOpen(ctx); err != nil {
		return VolInfo{}, err
	}
	dir, err := s.volumeDir(volume)
	if err != nil {
		return VolInfo{}, err
	}
	st, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return VolInfo{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, volume)
		}
		return VolInfo{}, fmt.Errorf("storage: stat volume %s: %w", volume, mapErr(err))
	}
	if !st.IsDir() {
		return VolInfo{}, fmt.Errorf("%w: %s is not a directory", domain.ErrBucketNotFound, volume)
	}
	return VolInfo{Name: volume, Created: st.ModTime()}, nil
}

// DeleteVol implements StorageAPI.
func (s *xlStorage) DeleteVol(ctx context.Context, volume string, force bool) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	if volume == domain.SystemBucket {
		return fmt.Errorf("%w: the system volume cannot be deleted", domain.ErrInvalidArgument)
	}
	dir, err := s.volumeDir(volume)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, volume)
		}
		return fmt.Errorf("storage: stat volume %s: %w", volume, mapErr(err))
	}
	if force {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("storage: delete volume %s: %w", volume, mapErr(err))
		}
		s.syncDir(s.root)
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("storage: read volume %s: %w", volume, mapErr(err))
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s", domain.ErrBucketNotEmpty, volume)
	}
	if err := os.Remove(dir); err != nil {
		return fmt.Errorf("storage: delete volume %s: %w", volume, mapErr(err))
	}
	s.syncDir(s.root)
	return nil
}

// ReadVersion implements StorageAPI.
func (s *xlStorage) ReadVersion(ctx context.Context, volume, objPath, versionID string, opts ReadOptions) (FileInfo, error) {
	if err := s.checkOpen(ctx); err != nil {
		return FileInfo{}, err
	}
	dir, err := s.objectDir(volume, objPath)
	if err != nil {
		return FileInfo{}, err
	}
	meta, err := s.readMeta(dir)
	if err != nil {
		return FileInfo{}, s.notFound(volume, objPath, err)
	}
	idx := meta.find(versionID)
	if idx < 0 {
		if versionID == "" {
			return FileInfo{}, fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, volume, objPath)
		}
		return FileInfo{}, fmt.Errorf("%w: %s/%s version %s", domain.ErrNoSuchVersion, volume, objPath, versionID)
	}
	return meta.fileInfoAt(idx, volume, objPath, opts.ReadData), nil
}

// WriteMetadata implements StorageAPI.
func (s *xlStorage) WriteMetadata(ctx context.Context, volume, objPath string, fi FileInfo) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	dir, err := s.objectDir(volume, objPath)
	if err != nil {
		return err
	}
	if err := s.ensureVolume(volume); err != nil {
		return err
	}
	meta, err := s.readMeta(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return s.notFound(volume, objPath, err)
		}
		meta = &xlMeta{}
	}
	meta.upsert(versionFromFileInfo(fi))
	return s.writeMeta(dir, meta, true)
}

// UpdateMetadata implements StorageAPI.
func (s *xlStorage) UpdateMetadata(ctx context.Context, volume, objPath string, fi FileInfo, opts UpdateMetadataOptions) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	dir, err := s.objectDir(volume, objPath)
	if err != nil {
		return err
	}
	meta, err := s.readMeta(dir)
	if err != nil {
		return s.notFound(volume, objPath, err)
	}
	idx := meta.find(fi.VersionID)
	if idx < 0 {
		return fmt.Errorf("%w: %s/%s version %q", domain.ErrNoSuchVersion, volume, objPath, fi.VersionID)
	}
	meta.Versions[idx] = versionFromFileInfo(fi)
	meta.sort()
	return s.writeMeta(dir, meta, !opts.NoPersistence)
}

// DeleteVersion implements StorageAPI.
func (s *xlStorage) DeleteVersion(ctx context.Context, volume, objPath string, fi FileInfo, opts DeleteOptions) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	dir, err := s.objectDir(volume, objPath)
	if err != nil {
		return err
	}
	meta, err := s.readMeta(dir)
	if err != nil {
		if fi.MarkDeleted && errors.Is(err, fs.ErrNotExist) {
			// A delete marker on a missing object is still recorded, as S3 does.
			if err := s.ensureVolume(volume); err != nil {
				return err
			}
			meta = &xlMeta{}
		} else {
			return s.notFound(volume, objPath, err)
		}
	}
	if fi.MarkDeleted {
		marker := versionFromFileInfo(fi)
		marker.Deleted = true
		marker.DataDir, marker.Data, marker.Parts, marker.Size = "", nil, nil, 0
		meta.upsert(marker)
		return s.writeMeta(dir, meta, true)
	}
	idx := meta.findExact(fi.VersionID)
	if idx < 0 {
		return fmt.Errorf("%w: %s/%s version %q", domain.ErrNoSuchVersion, volume, objPath, fi.VersionID)
	}
	removed := meta.remove(idx)
	for _, dataDir := range []string{removed.DataDir, opts.OldDataDir} {
		if dataDir != "" {
			if err := os.RemoveAll(filepath.Join(dir, dataDir)); err != nil {
				s.deps.Logger.Warn("storage: cannot remove data dir", "path", filepath.Join(dir, dataDir), "err", err)
			}
		}
	}
	if len(meta.Versions) == 0 {
		if err := os.Remove(filepath.Join(dir, MetaFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("storage: remove %s: %w", MetaFile, mapErr(err))
		}
		volDir, _ := s.volumeDir(volume)
		s.pruneEmptyDirs(dir, volDir)
		return nil
	}
	return s.writeMeta(dir, meta, true)
}

// CreateFile implements StorageAPI.
func (s *xlStorage) CreateFile(ctx context.Context, volume, filePath string, size int64, r io.Reader) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	dst, err := s.filePath(volume, filePath)
	if err != nil {
		return err
	}
	if err := s.ensureVolume(volume); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), fs.FileMode(s.opts.dirPerm)); err != nil {
		return fmt.Errorf("storage: create %s: %w", filepath.Dir(dst), mapErr(err))
	}
	tmp, err := s.tempFile()
	if err != nil {
		return err
	}
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }
	src := &ctxReader{ctx: ctx, r: r}
	var n int64
	if size >= 0 {
		n, err = io.Copy(tmp, io.LimitReader(src, size))
		if err == nil && n != size {
			err = fmt.Errorf("%w: expected %d bytes, got %d", io.ErrUnexpectedEOF, size, n)
		}
	} else {
		n, err = io.Copy(tmp, src)
	}
	if err != nil {
		cleanup()
		return fmt.Errorf("storage: write %s/%s: %w", volume, filePath, mapErr(err))
	}
	if err := s.commitFile(tmp, dst); err != nil {
		cleanup()
		return fmt.Errorf("storage: commit %s/%s: %w", volume, filePath, err)
	}
	_ = n
	return nil
}

// ReadFileStream implements StorageAPI.
func (s *xlStorage) ReadFileStream(ctx context.Context, volume, filePath string, offset, length int64) (io.ReadCloser, error) {
	if err := s.checkOpen(ctx); err != nil {
		return nil, err
	}
	name, err := s.filePath(volume, filePath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, s.notFound(volume, filePath, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("storage: stat %s: %w", filePath, mapErr(err))
	}
	if offset < 0 || offset > st.Size() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: offset %d beyond size %d", domain.ErrInvalidRange, offset, st.Size())
	}
	if length < 0 || offset+length > st.Size() {
		length = st.Size() - offset
	}
	return &sectionReadCloser{Reader: io.NewSectionReader(f, offset, length), f: f}, nil
}

// RenameData implements StorageAPI.
func (s *xlStorage) RenameData(ctx context.Context, srcVolume, srcPath string, fi FileInfo, dstVolume, dstPath string, opts RenameOptions) (RenameDataResult, error) {
	if err := s.checkOpen(ctx); err != nil {
		return RenameDataResult{}, err
	}
	dstDir, err := s.objectDir(dstVolume, dstPath)
	if err != nil {
		return RenameDataResult{}, err
	}
	if err := s.ensureVolume(dstVolume); err != nil {
		return RenameDataResult{}, err
	}
	if err := os.MkdirAll(dstDir, fs.FileMode(s.opts.dirPerm)); err != nil {
		return RenameDataResult{}, fmt.Errorf("storage: create %s: %w", dstDir, mapErr(err))
	}
	if fi.DataDir != "" {
		srcDir, err := s.filePath(srcVolume, srcPath)
		if err != nil {
			return RenameDataResult{}, err
		}
		if _, err := os.Stat(srcDir); err != nil {
			return RenameDataResult{}, s.notFound(srcVolume, srcPath, err)
		}
		target := filepath.Join(dstDir, fi.DataDir)
		if err := os.RemoveAll(target); err != nil {
			return RenameDataResult{}, fmt.Errorf("storage: replace %s: %w", target, mapErr(err))
		}
		if err := os.Rename(srcDir, target); err != nil {
			return RenameDataResult{}, fmt.Errorf("storage: rename data into %s/%s: %w", dstVolume, dstPath, mapErr(err))
		}
	}
	meta, err := s.readMeta(dstDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return RenameDataResult{}, s.notFound(dstVolume, dstPath, err)
		}
		meta = &xlMeta{}
	}
	var result RenameDataResult
	if replaced := meta.upsert(versionFromFileInfo(fi)); replaced != nil && replaced.DataDir != "" && replaced.DataDir != fi.DataDir {
		result.OldDataDir = replaced.DataDir
	}
	if err := s.writeMeta(dstDir, meta, true); err != nil {
		return RenameDataResult{}, err
	}
	if result.OldDataDir != "" && opts.BaseOptions.Immediate {
		if err := os.RemoveAll(filepath.Join(dstDir, result.OldDataDir)); err != nil {
			s.deps.Logger.Warn("storage: cannot purge replaced data dir", "path", filepath.Join(dstDir, result.OldDataDir), "err", err)
		}
		result.OldDataDir = ""
	}
	copy(result.Signature[:], fi.VersionID+fi.DataDir+"....")
	return result, nil
}

// ReadAll implements StorageAPI.
func (s *xlStorage) ReadAll(ctx context.Context, volume, filePath string) ([]byte, error) {
	if err := s.checkOpen(ctx); err != nil {
		return nil, err
	}
	name, err := s.filePath(volume, filePath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, s.notFound(volume, filePath, err)
	}
	return data, nil
}

// WriteAll implements StorageAPI.
func (s *xlStorage) WriteAll(ctx context.Context, volume, filePath string, b []byte) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	name, err := s.filePath(volume, filePath)
	if err != nil {
		return err
	}
	if err := s.ensureVolume(volume); err != nil {
		return err
	}
	if err := s.writeAtomic(name, b, true); err != nil {
		return fmt.Errorf("storage: write %s/%s: %w", volume, filePath, err)
	}
	return nil
}

// Delete implements StorageAPI.
func (s *xlStorage) Delete(ctx context.Context, volume, filePath string, opts DeleteOptions) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	name, err := s.filePath(volume, filePath)
	if err != nil {
		return err
	}
	if opts.Recursive {
		if _, err := os.Lstat(name); err != nil {
			return s.notFound(volume, filePath, err)
		}
		if err := os.RemoveAll(name); err != nil {
			return fmt.Errorf("storage: delete %s/%s: %w", volume, filePath, mapErr(err))
		}
	} else if err := os.Remove(name); err != nil {
		return s.notFound(volume, filePath, err)
	}
	volDir, _ := s.volumeDir(volume)
	s.pruneEmptyDirs(filepath.Dir(name), volDir)
	return nil
}

// CheckParts implements StorageAPI.
func (s *xlStorage) CheckParts(ctx context.Context, volume, objPath string, fi FileInfo) (*CheckPartsResult, error) {
	if err := s.checkOpen(ctx); err != nil {
		return nil, err
	}
	dir, err := s.objectDir(volume, objPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if volDir, _ := s.volumeDir(volume); volDir != "" {
				if _, verr := os.Stat(volDir); errors.Is(verr, fs.ErrNotExist) {
					return &CheckPartsResult{Results: fill(len(fi.Parts), CheckPartVolumeNotFound)}, nil
				}
			}
			return &CheckPartsResult{Results: fill(len(fi.Parts), CheckPartFileNotFound)}, nil
		}
		return nil, fmt.Errorf("storage: stat %s: %w", dir, mapErr(err))
	}
	res := &CheckPartsResult{Results: make([]int, len(fi.Parts))}
	for i, p := range fi.Parts {
		st, err := os.Stat(filepath.Join(dir, fi.DataDir, partName(p.Number)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			res.Results[i] = CheckPartFileNotFound
		case err != nil:
			res.Results[i] = CheckPartUnknown
		case st.Size() != p.Size:
			res.Results[i] = CheckPartFileCorrupt
		default:
			res.Results[i] = CheckPartSuccess
		}
	}
	return res, nil
}

// StatInfoFile implements StorageAPI.
func (s *xlStorage) StatInfoFile(ctx context.Context, volume, filePath string, glob bool) ([]StatInfo, error) {
	if err := s.checkOpen(ctx); err != nil {
		return nil, err
	}
	name, err := s.filePath(volume, filePath)
	if err != nil {
		return nil, err
	}
	names := []string{name}
	if glob {
		matches, err := filepath.Glob(name)
		if err != nil {
			return nil, fmt.Errorf("%w: glob %q: %v", domain.ErrInvalidArgument, filePath, err)
		}
		names = matches
	}
	volDir, _ := s.volumeDir(volume)
	out := make([]StatInfo, 0, len(names))
	for _, n := range names {
		st, err := os.Stat(n)
		if err != nil {
			if glob && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, s.notFound(volume, filePath, err)
		}
		rel, _ := filepath.Rel(volDir, n)
		out = append(out, StatInfo{Name: filepath.ToSlash(rel), Size: st.Size(), Dir: st.IsDir(), Mode: uint32(st.Mode()), ModTime: st.ModTime()})
	}
	return out, nil
}

// --- path helpers ---------------------------------------------------------

// volumeDir validates a volume name and returns its directory.
func (s *xlStorage) volumeDir(volume string) (string, error) {
	if volume == "" || volume == "." || volume == ".." || strings.ContainsAny(volume, `/\`) {
		return "", fmt.Errorf("%w: invalid volume name %q", domain.ErrInvalidBucketName, volume)
	}
	return filepath.Join(s.root, volume), nil
}

// filePath validates a slash-separated path inside a volume and returns the
// corresponding file system path.
func (s *xlStorage) filePath(volume, p string) (string, error) {
	dir, err := s.volumeDir(volume)
	if err != nil {
		return "", err
	}
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: invalid path %q", domain.ErrInvalidObjectName, p)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: invalid path %q", domain.ErrInvalidObjectName, p)
		}
	}
	return filepath.Join(dir, filepath.FromSlash(p)), nil
}

// objectDir returns the directory holding the xl.meta of an object key.
// Keys ending with a slash are stored under a "__XLDIR__" suffix.
func (s *xlStorage) objectDir(volume, key string) (string, error) {
	if strings.HasSuffix(key, "/") {
		key = strings.TrimSuffix(key, "/") + xlDirSuffix
	}
	return s.filePath(volume, key)
}

func (s *xlStorage) ensureVolume(volume string) error {
	dir, err := s.volumeDir(volume)
	if err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, volume)
		}
		return fmt.Errorf("storage: stat volume %s: %w", volume, mapErr(err))
	}
	if !st.IsDir() {
		return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, volume)
	}
	return nil
}

// notFound maps a file system error on volume/path to the domain sentinel:
// a missing volume is ErrBucketNotFound, a missing file ErrObjectNotFound.
func (s *xlStorage) notFound(volume, p string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		if dir, derr := s.volumeDir(volume); derr == nil {
			if _, serr := os.Stat(dir); errors.Is(serr, fs.ErrNotExist) {
				return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, volume)
			}
		}
		return fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, volume, p)
	}
	return fmt.Errorf("storage: %s/%s: %w", volume, p, mapErr(err))
}

// --- metadata and atomic writes ------------------------------------------

func (s *xlStorage) readMeta(dir string) (*xlMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, MetaFile))
	if err != nil {
		return nil, err
	}
	return decodeXLMeta(data)
}

func (s *xlStorage) writeMeta(dir string, meta *xlMeta, sync bool) error {
	data, err := meta.encode()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, fs.FileMode(s.opts.dirPerm)); err != nil {
		return fmt.Errorf("storage: create %s: %w", dir, mapErr(err))
	}
	if err := s.writeAtomic(filepath.Join(dir, MetaFile), data, sync); err != nil {
		return fmt.Errorf("storage: write %s: %w", MetaFile, err)
	}
	return nil
}

// writeAtomic writes data to a temporary file in the system tmp directory,
// syncs it and renames it over dst.
func (s *xlStorage) writeAtomic(dst string, data []byte, sync bool) error {
	if err := os.MkdirAll(filepath.Dir(dst), fs.FileMode(s.opts.dirPerm)); err != nil {
		return mapErr(err)
	}
	tmp, err := s.tempFile()
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return mapErr(err)
	}
	if !sync {
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmp.Name())
			return mapErr(err)
		}
		if err := os.Rename(tmp.Name(), dst); err != nil {
			_ = os.Remove(tmp.Name())
			return mapErr(err)
		}
		return nil
	}
	if err := s.commitFile(tmp, dst); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// commitFile syncs and closes tmp, renames it to dst and syncs the parent.
func (s *xlStorage) commitFile(tmp *os.File, dst string) error {
	if s.opts.fsync {
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			return mapErr(err)
		}
	}
	if err := tmp.Close(); err != nil {
		return mapErr(err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return mapErr(err)
	}
	s.syncDir(filepath.Dir(dst))
	return nil
}

func (s *xlStorage) tempFile() (*os.File, error) {
	dir := filepath.Join(s.root, domain.SystemBucket, TmpDir)
	f, err := os.OpenFile(filepath.Join(dir, randomSuffix()), os.O_RDWR|os.O_CREATE|os.O_EXCL, fs.FileMode(s.opts.filePerm))
	if err != nil {
		return nil, fmt.Errorf("storage: create temporary file: %w", mapErr(err))
	}
	return f, nil
}

// syncDir fsyncs a directory so that renames become durable (best effort).
func (s *xlStorage) syncDir(dir string) {
	if !s.opts.fsync {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
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

func randomSuffix() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func partName(n int) string { return fmt.Sprintf("part.%d", n) }

func fill(n, v int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// mapErr translates operating system errors into domain sentinels.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %v", domain.ErrObjectNotFound, err)
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return fmt.Errorf("%w: %v", domain.ErrStorageFull, err)
	case errors.Is(err, syscall.EIO), errors.Is(err, syscall.EROFS), errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%w: %v", domain.ErrFaultyDisk, err)
	default:
		return err
	}
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

// sectionReadCloser closes the file when the section is closed.
type sectionReadCloser struct {
	io.Reader
	f *os.File
}

func (r *sectionReadCloser) Close() error { return r.f.Close() }

// keyFromDirName converts an on-disk object directory name back to the key
// segment it represents.
func keyFromDirName(name string) (segment string, dirObject bool) {
	if strings.HasSuffix(name, xlDirSuffix) {
		return strings.TrimSuffix(name, xlDirSuffix) + "/", true
	}
	return name, false
}

// joinKey joins a prefix (ending with a slash or empty) and a segment.
func joinKey(prefix, segment string) string {
	return path.Join(prefix, segment) + strings.Repeat("/", boolToInt(strings.HasSuffix(segment, "/")))
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
