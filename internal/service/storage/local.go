package storage

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// keyLockStripes is the number of mutexes serializing the read-modify-write
// cycles of xl.meta on the same object key (and the placement of a key
// with respect to its ancestor prefixes, see lockPlacement).
const keyLockStripes = 256

// xlStorage is StorageAPI over a local drive directory:
//
//	<root>/.nexo.sys/{format.json,healing.bin,tmp/,multipart/,buckets/,config/}
//	<root>/<volume>/<key>/xl.meta
//	<root>/<volume>/<key>/<dataDir>/part.N
//
// Every write goes through a file of the system tmp directory followed by
// fsync and an atomic rename, so a crash leaves either the old or the new
// state plus garbage in tmp (emptied at startup) or an unreferenced data
// directory (ignored by reads and listings). Updates of the same xl.meta
// are serialized by striped key locks; distributed exclusion across nodes
// is the job of the object layer's namespace locks.
type xlStorage struct {
	deps Deps
	opts options
	ep   domain.Endpoint
	root string

	mu       sync.RWMutex
	closed   bool
	diskID   string
	lastInfo domain.DiskInfo

	healing atomic.Pointer[HealingTracker]

	seed     maphash.Seed
	keyLocks [keyLockStripes]sync.RWMutex

	// rename is os.Rename; internal tests replace it to inject failures.
	rename func(oldpath, newpath string) error
}

var _ StorageAPI = (*xlStorage)(nil)

// NewLocal opens (creating it when missing) the drive directory of a local
// endpoint, prepares the system directories (.nexo.sys, tmp, multipart,
// buckets, config), empties tmp, probes writability and loads the drive id
// from format.json when present. The drive is left unformatted when no
// format.json exists: Formatter.EnsureFormats writes it. In a distributed
// deployment the drives of this node are URL endpoints marked IsLocal by the
// topology: they are opened at their path and keep their URL as Endpoint.
func NewLocal(deps Deps, ep domain.Endpoint, opts ...Option) (StorageAPI, error) {
	if (ep.IsURL() && !ep.IsLocal) || ep.Path == "" {
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
	s := &xlStorage{deps: deps, opts: o, ep: ep, root: root, seed: maphash.MakeSeed(), rename: os.Rename}
	if err := s.initDirs(); err != nil {
		return nil, err
	}
	if err := s.probe(); err != nil {
		return nil, err
	}
	s.diskID = s.loadDiskID()
	s.loadHealing()
	deps.Logger.Debug("storage: local drive ready", "path", root, "disk_id", s.diskID)
	return s, nil
}

func (s *xlStorage) initDirs() error {
	if err := os.MkdirAll(s.root, fs.FileMode(s.opts.dirPerm)); err != nil {
		return fmt.Errorf("storage: create drive %s: %w", s.root, mapErr(err))
	}
	sys := filepath.Join(s.root, domain.SystemBucket)
	for _, d := range []string{sys, filepath.Join(sys, TmpDir), filepath.Join(sys, MultipartDir), filepath.Join(sys, BucketsDir), filepath.Join(sys, ConfigDir)} {
		if err := s.mkdirAll(d, s.root); err != nil {
			return fmt.Errorf("storage: create %s: %w", d, mapErr(err))
		}
	}
	if s.opts.cleanTmp {
		return s.cleanTmp()
	}
	return nil
}

// cleanTmp removes the leftovers of interrupted writes from the system tmp
// directory: staged xl.meta files, partial data files and directories.
func (s *xlStorage) cleanTmp() error {
	tmp := filepath.Join(s.root, domain.SystemBucket, TmpDir)
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return fmt.Errorf("storage: read %s: %w", tmp, mapErr(err))
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(tmp, e.Name())); err != nil {
			s.deps.Logger.Warn("storage: cannot remove stale temporary entry", "path", filepath.Join(tmp, e.Name()), "err", err)
		}
	}
	if len(entries) > 0 {
		s.deps.Logger.Info("storage: removed stale temporary entries", "path", s.root, "count", len(entries))
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
	data, err := readFile(filepath.Join(s.root, domain.SystemBucket, FormatFile))
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

// IsOnline implements StorageAPI: the drive is open and its root exists.
func (s *xlStorage) IsOnline() bool {
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	return !closed && s.rootExists()
}

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

// stripe returns the key lock stripe of volume/key.
func (s *xlStorage) stripe(volume, key string) int {
	return int(maphash.String(s.seed, volume+"\x00"+key) % keyLockStripes)
}

// lockKey serializes the metadata updates of one existing object key
// (updates that cannot create the key nor change its placement).
func (s *xlStorage) lockKey(volume, key string) func() {
	m := &s.keyLocks[s.stripe(volume, key)]
	m.Lock()
	return m.Unlock
}

// stripeLock is one stripe acquired by lockPlacement.
type stripeLock struct {
	idx   int
	write bool
}

// lockPlacement locks a key for a metadata update that may create it or
// remove it, together with its ancestor prefixes ("a" and "a/b" for
// "a/b/c" or "a/b/c/"), so that checkObjectPlacement and the commit are
// atomic with respect to the writers of those prefixes: "k" and "k/x"
// written concurrently can no longer both pass the check. The key's own
// stripe is taken for writing, the ancestors' for reading (writers of
// sibling keys share them); the stripes are deduplicated (write wins) and
// acquired in index order, so two writers never wait on each other in
// opposite orders.
func (s *xlStorage) lockPlacement(volume, key string) func() {
	locks := []stripeLock{{idx: s.stripe(volume, key), write: true}}
	for p := strings.TrimSuffix(key, "/"); ; {
		i := strings.LastIndexByte(p, '/')
		if i <= 0 {
			break
		}
		p = p[:i]
		locks = append(locks, stripeLock{idx: s.stripe(volume, p)})
	}
	locks = orderStripes(locks)
	for _, l := range locks {
		if l.write {
			s.keyLocks[l.idx].Lock()
		} else {
			s.keyLocks[l.idx].RLock()
		}
	}
	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			if locks[i].write {
				s.keyLocks[locks[i].idx].Unlock()
			} else {
				s.keyLocks[locks[i].idx].RUnlock()
			}
		}
	}
}

// orderStripes sorts stripe locks by index and merges duplicates, a write
// lock absorbing a read lock of the same stripe (a stripe is never locked
// twice by the same writer: RWMutex is not reentrant).
func orderStripes(locks []stripeLock) []stripeLock {
	slices.SortFunc(locks, func(a, b stripeLock) int {
		if c := cmp.Compare(a.idx, b.idx); c != 0 {
			return c
		}
		switch {
		case a.write == b.write:
			return 0
		case a.write:
			return -1
		default:
			return 1
		}
	})
	return slices.CompactFunc(locks, func(a, b stripeLock) bool { return a.idx == b.idx })
}

// DiskInfo implements StorageAPI. With NoOp it returns the usage figures
// of the previous call instead of querying the file system.
func (s *xlStorage) DiskInfo(ctx context.Context, opts DiskInfoOptions) (domain.DiskInfo, error) {
	if err := s.checkOpen(ctx); err != nil {
		return domain.DiskInfo{}, err
	}
	s.mu.Lock()
	if s.diskID == "" {
		s.diskID = s.loadDiskID()
	}
	id, cached := s.diskID, s.lastInfo
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
	if !s.rootExists() {
		info.State = domain.StateOffline
		info.Error = "drive directory not found"
		return info, fmt.Errorf("%w: drive %s is gone", domain.ErrDiskNotFound, s.root)
	}
	if opts.DiskID != "" && opts.DiskID != id {
		return info, fmt.Errorf("%w: drive %s has id %q, want %q", domain.ErrDiskNotFound, s.root, id, opts.DiskID)
	}
	if opts.NoOp {
		info.Total, info.Free, info.Used = cached.Total, cached.Free, cached.Used
		info.UsedInodes, info.FreeInodes, info.FSType, info.RootDisk = cached.UsedInodes, cached.FreeInodes, cached.FSType, cached.RootDisk
	} else {
		total, free, usedInodes, freeInodes, fsType, err := statfs(s.root)
		if err != nil {
			info.State = domain.StateOffline
			info.Error = err.Error()
			return info, fmt.Errorf("%w: %w", domain.ErrFaultyDisk, err)
		}
		info.Total, info.Free, info.UsedInodes, info.FreeInodes, info.FSType = total, free, usedInodes, freeInodes, fsType
		if total >= free {
			info.Used = total - free
		}
		info.RootDisk = isRootDisk(s.root)
		s.loadHealing()
		s.mu.Lock()
		s.lastInfo = info
		s.mu.Unlock()
	}
	if h := s.healing.Load(); h != nil && !h.Finished {
		info.Healing = true
		info.State = domain.StateHealing
		info.HealingProgress = h.Progress()
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
		return s.fsErr("", volume, err)
	}
	s.syncDir(s.root)
	return nil
}

// ListVols implements StorageAPI. Hidden directories (the system volume)
// and symbolic links are not volumes.
func (s *xlStorage) ListVols(ctx context.Context) ([]VolInfo, error) {
	if err := s.checkOpen(ctx); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, s.fsErr("", "", err)
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
	dir, err := s.ensureVolume(volume)
	if err != nil {
		return VolInfo{}, err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return VolInfo{}, s.fsErr(volume, "", err)
	}
	return VolInfo{Name: volume, Created: st.ModTime()}, nil
}

// DeleteVol implements StorageAPI. Without force a volume holding only
// empty directories (leftovers of deleted objects) counts as empty.
func (s *xlStorage) DeleteVol(ctx context.Context, volume string, force bool) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	if volume == domain.SystemBucket {
		return fmt.Errorf("%w: the system volume cannot be deleted", domain.ErrInvalidArgument)
	}
	dir, err := s.ensureVolume(volume)
	if err != nil {
		return err
	}
	if !force {
		empty, err := onlyEmptyDirs(dir)
		if err != nil {
			return s.fsErr(volume, "", err)
		}
		if !empty {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotEmpty, volume)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("storage: delete volume %s: %w", volume, mapErr(err))
	}
	s.syncDir(s.root)
	return nil
}

// onlyEmptyDirs reports whether the tree below dir holds no file.
func onlyEmptyDirs(dir string) (bool, error) {
	empty := true
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			empty = false
			return fs.SkipAll
		}
		return nil
	})
	return empty, err
}

// --- metadata ---------------------------------------------------------------

// ReadVersion implements StorageAPI. An empty versionID selects the latest
// version, "null" the null version. Without opts.ReadData only the
// metadata section of xl.meta is read.
func (s *xlStorage) ReadVersion(ctx context.Context, volume, objPath, versionID string, opts ReadOptions) (FileInfo, error) {
	if err := s.checkOpen(ctx); err != nil {
		return FileInfo{}, err
	}
	dir, err := s.objectDir(volume, objPath)
	if err != nil {
		return FileInfo{}, err
	}
	meta, err := s.readMeta(dir, opts.ReadData)
	if err != nil {
		return FileInfo{}, s.metaErr(volume, objPath, err)
	}
	idx := 0
	if versionID == "" {
		if _, ok := meta.Latest(); !ok {
			return FileInfo{}, fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, volume, objPath)
		}
	} else if idx, err = meta.FindVersion(versionID); err != nil {
		return FileInfo{}, fmt.Errorf("storage: %s/%s: %w", volume, objPath, err)
	}
	return meta.fileInfoAt(idx, volume, objPath, opts.ReadData), nil
}

// WriteMetadata implements StorageAPI: the version is added, replacing the
// version with the same id; the data directory of a replaced version, no
// longer referenced, is removed.
func (s *xlStorage) WriteMetadata(ctx context.Context, volume, objPath string, fi FileInfo) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	dir, err := s.objectDir(volume, objPath)
	if err != nil {
		return err
	}
	volDir, err := s.ensureVolume(volume)
	if err != nil {
		return err
	}
	unlock := s.lockPlacement(volume, objPath)
	defer unlock()
	if err := s.checkObjectPlacement(volume, objPath); err != nil {
		return err
	}
	meta, err := s.readMetaOrNew(volume, objPath, dir)
	if err != nil {
		return err
	}
	replaced, err := meta.AddVersion(versionFromFileInfo(fi))
	if err != nil {
		return fmt.Errorf("storage: %s/%s: %w", volume, objPath, err)
	}
	if err := s.writeMeta(dir, volDir, meta, true); err != nil {
		return err
	}
	if replaced != nil && !meta.sharesDataDir(replaced.DataDir) {
		s.removeDataDir(dir, replaced.DataDir)
	}
	return nil
}

// UpdateMetadata implements StorageAPI. The version is matched by id (the
// empty id is the null version); content fields left empty in fi (inline
// data, data directory, parts, erasure layout) keep their stored value.
func (s *xlStorage) UpdateMetadata(ctx context.Context, volume, objPath string, fi FileInfo, opts UpdateMetadataOptions) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	dir, err := s.objectDir(volume, objPath)
	if err != nil {
		return err
	}
	volDir, err := s.volumeDir(volume)
	if err != nil {
		return err
	}
	unlock := s.lockKey(volume, objPath)
	defer unlock()
	meta, err := s.readMeta(dir, true)
	if err != nil {
		return s.metaErr(volume, objPath, err)
	}
	idx, err := meta.FindVersion(fi.VersionID)
	if err != nil {
		return fmt.Errorf("storage: %s/%s: %w", volume, objPath, err)
	}
	old := meta.Versions[idx]
	v := versionFromFileInfo(fi)
	if v.Data == nil && old.Flags&xlFlagInlineData != 0 {
		v.Data = old.Data
		v.Flags |= xlFlagInlineData
	}
	if v.DataDir == "" {
		v.DataDir = old.DataDir
	}
	if len(v.Parts) == 0 {
		v.Parts = old.Parts
	}
	if v.Erasure == nil {
		v.Erasure = old.Erasure
	}
	if err := v.validate(); err != nil {
		return fmt.Errorf("%w: storage: %s/%s: %w", domain.ErrInvalidArgument, volume, objPath, err)
	}
	meta.Versions[idx] = v
	meta.sortVersions()
	return s.writeMeta(dir, volDir, meta, !opts.NoPersistence)
}

// DeleteVersion implements StorageAPI.
func (s *xlStorage) DeleteVersion(ctx context.Context, volume, objPath string, fi FileInfo, opts DeleteOptions) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	if opts.OldDataDir != "" && !isDataDir(opts.OldDataDir) {
		return fmt.Errorf("%w: invalid data directory %q", domain.ErrInvalidArgument, opts.OldDataDir)
	}
	dir, err := s.objectDir(volume, objPath)
	if err != nil {
		return err
	}
	volDir, err := s.ensureVolume(volume)
	if err != nil {
		return err
	}
	// A delete marker may create the key; removing the last version prunes
	// the empty parent directories: both are placement changes.
	unlock := s.lockPlacement(volume, objPath)
	defer unlock()
	meta, err := s.readMeta(dir, true)
	if err != nil {
		if !fi.MarkDeleted || !errors.Is(err, fs.ErrNotExist) {
			return s.metaErr(volume, objPath, err)
		}
		// A delete marker on a missing object is still recorded, as S3 does.
		if err := s.checkObjectPlacement(volume, objPath); err != nil {
			return err
		}
		meta = &xlMeta{}
	}
	if fi.MarkDeleted {
		marker := versionFromFileInfo(fi)
		marker.Type = xlTypeDeleteMarker
		replaced, err := meta.AddVersion(marker)
		if err != nil {
			return fmt.Errorf("storage: %s/%s: %w", volume, objPath, err)
		}
		if err := s.writeMeta(dir, volDir, meta, true); err != nil {
			return err
		}
		// A marker replacing the null version (suspended versioning) frees its data.
		if replaced != nil && !meta.sharesDataDir(replaced.DataDir) {
			s.removeDataDir(dir, replaced.DataDir)
		}
		return nil
	}
	removed, err := meta.DeleteVersion(fi.VersionID)
	if err != nil {
		return fmt.Errorf("storage: %s/%s: %w", volume, objPath, err)
	}
	if len(meta.Versions) == 0 {
		if err := os.Remove(filepath.Join(dir, MetaFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("storage: remove %s/%s: %w", volume, objPath, mapErr(err))
		}
		s.removeOrphanDataDirs(dir)
		s.syncDir(dir)
		s.pruneEmptyDirs(dir, volDir)
		return nil
	}
	if err := s.writeMeta(dir, volDir, meta, true); err != nil {
		return err
	}
	for _, dataDir := range []string{removed.DataDir, opts.OldDataDir} {
		if dataDir != "" && !meta.sharesDataDir(dataDir) {
			s.removeDataDir(dir, dataDir)
		}
	}
	return nil
}

// removeDataDir purges a data directory that no version references. A
// directory that is not a data directory on disk (a child prefix whose
// name happens to be a UUID) is never removed.
func (s *xlStorage) removeDataDir(objDir, dataDir string) {
	if !isDataDir(dataDir) {
		return
	}
	path := filepath.Join(objDir, dataDir)
	ok, err := isDataDirOnDisk(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.deps.Logger.Warn("storage: cannot inspect data dir", "path", path, "err", err)
	}
	if !ok {
		return
	}
	if err := os.RemoveAll(path); err != nil {
		s.deps.Logger.Warn("storage: cannot remove data dir", "path", path, "err", err)
	}
}

// removeOrphanDataDirs removes the data directories left in an object
// directory without metadata (interrupted commits, purged versions). Child
// prefixes and objects are left alone, whatever their name.
func (s *xlStorage) removeOrphanDataDirs(objDir string) {
	entries, err := os.ReadDir(objDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && isDataDir(e.Name()) {
			s.removeDataDir(objDir, e.Name())
		}
	}
}

// --- data files -------------------------------------------------------------

// CreateFile implements StorageAPI.
func (s *xlStorage) CreateFile(ctx context.Context, volume, filePath string, size int64, r io.Reader) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	if size < -1 {
		return fmt.Errorf("%w: negative size %d", domain.ErrInvalidArgument, size)
	}
	dst, err := s.filePath(volume, filePath)
	if err != nil {
		return err
	}
	volDir, err := s.ensureVolume(volume)
	if err != nil {
		return err
	}
	tmp, err := s.tempFile()
	if err != nil {
		return err
	}
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }
	src := &ctxReader{ctx: ctx, r: r}
	if size >= 0 {
		var n int64
		n, err = io.Copy(tmp, io.LimitReader(src, size))
		if err == nil && n != size {
			err = fmt.Errorf("%w: expected %d bytes, got %d", io.ErrUnexpectedEOF, size, n)
		}
	} else {
		_, err = io.Copy(tmp, src)
	}
	if err != nil {
		cleanup()
		return fmt.Errorf("storage: write %s/%s: %w", volume, filePath, mapErr(err))
	}
	if err := s.commitFile(tmp, dst, volDir); err != nil {
		cleanup()
		return fmt.Errorf("storage: commit %s/%s: %w", volume, filePath, s.writeErr(volume, filePath, err))
	}
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
	f, st, err := openFile(name)
	if err != nil {
		return nil, s.fsErr(volume, filePath, err)
	}
	size := st.Size()
	switch {
	case offset < 0 || offset > size:
		_ = f.Close()
		return nil, fmt.Errorf("%w: offset %d outside %s/%s (%d bytes)", domain.ErrInvalidRange, offset, volume, filePath, size)
	case length < 0:
		length = size - offset
	case length > size-offset:
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s/%s has %d bytes, want [%d, %d)", ErrFileCorrupt, volume, filePath, size, offset, offset+length)
	}
	return &sectionReadCloser{Reader: io.NewSectionReader(f, offset, length), f: f}, nil
}

// RenameData implements StorageAPI. The new xl.meta is staged and synced
// in tmp first, then the data directory is moved into the object directory
// and the staged xl.meta renamed over the old one: the commit point is
// that last rename. When it fails the data directory is moved back (and a
// data directory it displaced restored), so the caller can retry or clean
// its temporary data; a crash before it leaves an unreferenced data
// directory that reads and listings ignore.
func (s *xlStorage) RenameData(ctx context.Context, srcVolume, srcPath string, fi FileInfo, dstVolume, dstPath string, opts RenameOptions) (RenameDataResult, error) {
	if err := s.checkOpen(ctx); err != nil {
		return RenameDataResult{}, err
	}
	if fi.DataDir != "" && !isDataDir(fi.DataDir) {
		return RenameDataResult{}, fmt.Errorf("%w: data directory %q is not a UUID", domain.ErrInvalidArgument, fi.DataDir)
	}
	dstDir, err := s.objectDir(dstVolume, dstPath)
	if err != nil {
		return RenameDataResult{}, err
	}
	dstVolDir, err := s.ensureVolume(dstVolume)
	if err != nil {
		return RenameDataResult{}, err
	}
	var srcDir, srcVolDir string
	if fi.DataDir != "" {
		if srcDir, err = s.filePath(srcVolume, srcPath); err != nil {
			return RenameDataResult{}, err
		}
		if srcVolDir, err = s.volumeDir(srcVolume); err != nil {
			return RenameDataResult{}, err
		}
		st, err := os.Lstat(srcDir)
		if err != nil {
			return RenameDataResult{}, s.fsErr(srcVolume, srcPath, err)
		}
		if !st.IsDir() {
			return RenameDataResult{}, fmt.Errorf("%w: %s/%s is not a data directory", domain.ErrObjectNotFound, srcVolume, srcPath)
		}
	}

	unlock := s.lockPlacement(dstVolume, dstPath)
	defer unlock()
	if err := s.checkObjectPlacement(dstVolume, dstPath); err != nil {
		return RenameDataResult{}, err
	}
	meta, err := s.readMetaOrNew(dstVolume, dstPath, dstDir)
	if err != nil {
		return RenameDataResult{}, err
	}
	v := versionFromFileInfo(fi)
	replaced, err := meta.AddVersion(v)
	if err != nil {
		return RenameDataResult{}, fmt.Errorf("storage: %s/%s: %w", dstVolume, dstPath, err)
	}
	buf, err := meta.encode()
	if err != nil {
		return RenameDataResult{}, err
	}
	staged, err := s.writeTemp(buf, true)
	if err != nil {
		return RenameDataResult{}, fmt.Errorf("storage: stage %s/%s: %w", dstVolume, dstPath, err)
	}
	defer func() { _ = os.Remove(staged) }() // no-op once committed

	if err := s.mkdirAll(dstDir, dstVolDir); err != nil {
		return RenameDataResult{}, fmt.Errorf("storage: create %s/%s: %w", dstVolume, dstPath, s.writeErr(dstVolume, dstPath, err))
	}
	var target, aside string
	if fi.DataDir != "" {
		target = filepath.Join(dstDir, fi.DataDir)
		if _, err := os.Lstat(target); err == nil {
			// Same data directory committed again (heal): keep the old
			// copy aside until the new metadata is in place.
			aside = s.tmpPath()
			if err := s.rename(target, aside); err != nil {
				return RenameDataResult{}, fmt.Errorf("storage: move %s aside: %w", target, mapErr(err))
			}
		}
		if err := s.rename(srcDir, target); err != nil {
			s.restoreAside(aside, target)
			s.pruneEmptyDirs(dstDir, dstVolDir)
			return RenameDataResult{}, fmt.Errorf("storage: rename data into %s/%s: %w", dstVolume, dstPath, s.writeErr(srcVolume, srcPath, err))
		}
		s.syncDir(dstDir)
	}
	if err := s.rename(staged, filepath.Join(dstDir, MetaFile)); err != nil {
		if target != "" {
			if rerr := s.renameAll(target, srcDir, srcVolDir); rerr != nil {
				s.deps.Logger.Error("storage: cannot roll back data directory", "from", target, "to", srcDir, "err", rerr)
			}
			s.restoreAside(aside, target)
		}
		s.pruneEmptyDirs(dstDir, dstVolDir)
		return RenameDataResult{}, fmt.Errorf("storage: commit %s/%s: %w", dstVolume, dstPath, s.writeErr(dstVolume, dstPath, err))
	}
	s.syncDir(dstDir)
	if aside != "" {
		if err := os.RemoveAll(aside); err != nil {
			s.deps.Logger.Warn("storage: cannot remove displaced data dir", "path", aside, "err", err)
		}
	}
	if srcDir != "" {
		s.pruneEmptyDirs(filepath.Dir(srcDir), s.pruneBoundary(srcVolume, srcDir))
	}

	result := RenameDataResult{Signature: v.signature()}
	if replaced != nil && !meta.sharesDataDir(replaced.DataDir) {
		result.OldDataDir = replaced.DataDir
		if opts.BaseOptions.Immediate {
			s.removeDataDir(dstDir, replaced.DataDir)
			result.OldDataDir = ""
		}
	}
	return result, nil
}

// restoreAside moves a displaced data directory back in place.
func (s *xlStorage) restoreAside(aside, target string) {
	if aside == "" {
		return
	}
	if err := s.rename(aside, target); err != nil {
		s.deps.Logger.Error("storage: cannot restore displaced data dir", "from", aside, "to", target, "err", err)
	}
}

// RenameFile implements StorageAPI.
func (s *xlStorage) RenameFile(ctx context.Context, srcVolume, srcPath, dstVolume, dstPath string) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	src, err := s.filePath(srcVolume, srcPath)
	if err != nil {
		return err
	}
	dst, err := s.filePath(dstVolume, dstPath)
	if err != nil {
		return err
	}
	dstVolDir, err := s.ensureVolume(dstVolume)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(src); err != nil {
		return s.fsErr(srcVolume, srcPath, err)
	}
	if err := s.renameAll(src, dst, dstVolDir); err != nil {
		return fmt.Errorf("storage: rename %s/%s to %s/%s: %w", srcVolume, srcPath, dstVolume, dstPath, s.writeErr(dstVolume, dstPath, err))
	}
	s.syncDir(filepath.Dir(dst))
	s.pruneEmptyDirs(filepath.Dir(src), s.pruneBoundary(srcVolume, src))
	return nil
}

// checkObjectPlacement refuses object keys whose parent prefix is itself an
// object ("a" exists, writing "a/b") and keys that are already a prefix with
// children ("a/b" exists, writing "a"): both would make entries unreachable
// through listings. Existing object directories (data dirs only) are fine;
// a sub-directory named like a data directory that holds directories or an
// xl.meta is a child prefix ("a/<uuid>/b" exists, writing "a").
func (s *xlStorage) checkObjectPlacement(volume, key string) error {
	volDir, err := s.volumeDir(volume)
	if err != nil {
		return err
	}
	dir, err := s.objectDir(volume, key)
	if err != nil {
		return err
	}
	for parent := filepath.Dir(dir); parent != volDir && strings.HasPrefix(parent, volDir+string(filepath.Separator)); parent = filepath.Dir(parent) {
		if regularFileExists(filepath.Join(parent, MetaFile)) {
			rel, _ := filepath.Rel(volDir, parent)
			return domain.NewS3Error(domain.ErrInvalidObjectName, "XNexoParentIsObject",
				"Object-prefix is already an object: "+filepath.ToSlash(rel)).WithResource(volume + "/" + key)
		}
	}
	if strings.HasSuffix(key, "/") || regularFileExists(filepath.Join(dir, MetaFile)) {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // does not exist yet (or unreadable: later operations report it)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dataDir, err := isDataDirOnDisk(filepath.Join(dir, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed concurrently
		}
		if err != nil {
			return s.fsErr(volume, key, err)
		}
		if !dataDir {
			return domain.NewS3Error(domain.ErrInvalidObjectName, "XNexoObjectExistsAsDirectory",
				"Object name already exists as a prefix: "+key+"/").WithResource(volume + "/" + key)
		}
	}
	return nil
}

// --- small files ------------------------------------------------------------

// ReadAll implements StorageAPI.
func (s *xlStorage) ReadAll(ctx context.Context, volume, filePath string) ([]byte, error) {
	if err := s.checkOpen(ctx); err != nil {
		return nil, err
	}
	name, err := s.filePath(volume, filePath)
	if err != nil {
		return nil, err
	}
	data, err := readFile(name)
	if err != nil {
		return nil, s.fsErr(volume, filePath, err)
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
	volDir, err := s.ensureVolume(volume)
	if err != nil {
		return err
	}
	if err := s.writeAtomic(name, volDir, b, true); err != nil {
		return fmt.Errorf("storage: write %s/%s: %w", volume, filePath, s.writeErr(volume, filePath, err))
	}
	if isHealingFile(volume, filePath) {
		s.loadHealing()
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
			return s.fsErr(volume, filePath, err)
		}
		if err := os.RemoveAll(name); err != nil {
			return fmt.Errorf("storage: delete %s/%s: %w", volume, filePath, mapErr(err))
		}
	} else if err := os.Remove(name); err != nil {
		return s.fsErr(volume, filePath, err)
	}
	s.pruneEmptyDirs(filepath.Dir(name), s.pruneBoundary(volume, name))
	if isHealingFile(volume, filePath) {
		s.loadHealing()
	}
	return nil
}

// CheckParts implements StorageAPI. Versions with inline data have no part
// files: their parts are present when xl.meta is.
func (s *xlStorage) CheckParts(ctx context.Context, volume, objPath string, fi FileInfo) (*CheckPartsResult, error) {
	if err := s.checkOpen(ctx); err != nil {
		return nil, err
	}
	if fi.DataDir != "" && !isDataDir(fi.DataDir) {
		return nil, fmt.Errorf("%w: data directory %q is not a UUID", domain.ErrInvalidArgument, fi.DataDir)
	}
	dir, err := s.objectDir(volume, objPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, verr := s.ensureVolume(volume); verr != nil {
				if errors.Is(verr, domain.ErrBucketNotFound) {
					return &CheckPartsResult{Results: fill(len(fi.Parts), CheckPartVolumeNotFound)}, nil
				}
				return nil, verr
			}
			return &CheckPartsResult{Results: fill(len(fi.Parts), CheckPartFileNotFound)}, nil
		}
		return nil, s.fsErr(volume, objPath, err)
	}
	if fi.DataDir == "" {
		state := CheckPartSuccess
		if !regularFileExists(filepath.Join(dir, MetaFile)) {
			state = CheckPartFileNotFound
		}
		return &CheckPartsResult{Results: fill(len(fi.Parts), state)}, nil
	}
	res := &CheckPartsResult{Results: make([]int, len(fi.Parts))}
	for i, p := range fi.Parts {
		st, err := os.Lstat(filepath.Join(dir, fi.DataDir, partName(p.Number)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			res.Results[i] = CheckPartFileNotFound
		case err != nil:
			res.Results[i] = CheckPartUnknown
		case !st.Mode().IsRegular() || st.Size() != p.Size:
			res.Results[i] = CheckPartFileCorrupt
		default:
			res.Results[i] = CheckPartSuccess
		}
	}
	return res, nil
}

// StatInfoFile implements StorageAPI. Symbolic links are never reported.
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
			return nil, fmt.Errorf("%w: glob %q: %w", domain.ErrInvalidArgument, filePath, err)
		}
		names = matches
	}
	volDir, _ := s.volumeDir(volume)
	out := make([]StatInfo, 0, len(names))
	for _, n := range names {
		st, err := os.Lstat(n)
		if err == nil && st.Mode()&fs.ModeSymlink != 0 {
			if glob {
				continue
			}
			return nil, fmt.Errorf("%w: %s/%s", errSymlink, volume, filePath)
		}
		if err != nil {
			if glob && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, s.fsErr(volume, filePath, err)
		}
		rel, _ := filepath.Rel(volDir, n)
		out = append(out, StatInfo{Name: filepath.ToSlash(rel), Size: st.Size(), Dir: st.IsDir(), Mode: uint32(st.Mode()), ModTime: st.ModTime()})
	}
	return out, nil
}
