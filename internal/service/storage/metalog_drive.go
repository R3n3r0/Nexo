package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
)

// MetadataLogTuner is implemented by the local drives: it changes the
// settings of the metadata log of a running drive and reports its state.
type MetadataLogTuner interface {
	// SetMetadataLogSegmentSize sets the size of the next segments
	// (bounded by MinMetaLogSegmentSize and MaxMetaLogSegmentSize).
	SetMetadataLogSegmentSize(n int64)
	// SetMetadataLogMemoryPercent sets the share of the RAM the index may
	// use (1-90; 0 keeps the default) before a warning.
	SetMetadataLogMemoryPercent(p int)
	// MetadataLogStats returns the state of the log (Enabled false when
	// the drive keeps xl.meta files).
	MetadataLogStats() MetadataLogStats
	// CompactMetadataLog compacts now every sealed segment that holds
	// garbage (the background compaction waits for half of a segment):
	// the segments removed and the bytes reclaimed; nothing without the
	// log.
	CompactMetadataLog(ctx context.Context) (segments int, reclaimed int64, err error)
	// Endpoint names the drive.
	Endpoint() domain.Endpoint
}

var _ MetadataLogTuner = (*xlStorage)(nil)

// maxNameLen is the longest file name of the usual file systems.
const maxNameLen = 255

// DefaultMetaLogMemoryPercent is the default share of the RAM the index of
// a drive may use.
const DefaultMetaLogMemoryPercent = 25

// SetMetadataLogSegmentSize implements MetadataLogTuner.
func (s *xlStorage) SetMetadataLogSegmentSize(n int64) {
	s.segSize.Store(n)
	if l := s.ml(); l != nil {
		l.setSegmentSize(n)
	}
}

// SetMetadataLogMemoryPercent implements MetadataLogTuner.
func (s *xlStorage) SetMetadataLogMemoryPercent(p int) {
	if p <= 0 {
		p = DefaultMetaLogMemoryPercent
	}
	p = min(p, 90)
	s.memPercent.Store(int64(p))
	l := s.ml()
	if l == nil {
		return
	}
	l.memMax.Store(totalMemory() / 100 * uint64(p)) // #nosec G115 -- 1..90
	l.warned.Store(false)
	l.checkMemory()
}

// MetadataLogStats implements MetadataLogTuner.
func (s *xlStorage) MetadataLogStats() MetadataLogStats {
	if l := s.ml(); l != nil {
		return l.stats()
	}
	return MetadataLogStats{}
}

// CompactMetadataLog implements MetadataLogTuner.
func (s *xlStorage) CompactMetadataLog(ctx context.Context) (int, int64, error) {
	if err := s.checkOpen(ctx); err != nil {
		return 0, 0, err
	}
	l := s.ml()
	if l == nil {
		return 0, 0, nil
	}
	return l.compact(true)
}

// ml returns the metadata log of the drive (nil without the log).
func (s *xlStorage) ml() *metaLog { return s.mlog.Load() }

// logged reports whether the metadata of volume is in the metadata log:
// the user volumes of a drive in log mode (the system volume keeps files).
func (s *xlStorage) logged(volume string) bool {
	return s.opts.metaLog && volume != domain.SystemBucket
}

// checkLog verifies, before the log answers for the objects of the drive,
// that the drive still holds it: a drive emptied or replaced while the
// server runs has lost its marker, and its log starts over empty (the
// healing rewrites its objects), as a drive of xl.meta files has lost them.
func (s *xlStorage) checkLog() error {
	l := s.ml()
	if regularFileExists(l.marker) {
		return nil
	}
	if !s.rootExists() {
		return fmt.Errorf("%w: drive %s is gone", domain.ErrDiskNotFound, s.root)
	}
	return s.resetLog(l)
}

// resetLog replaces a log whose drive was emptied with a new empty one.
func (s *xlStorage) resetLog(old *metaLog) error {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.ml() != old {
		return nil // reset by another caller
	}
	s.deps.Logger.Warn("storage: the metadata log of the drive is gone (drive emptied or replaced): it starts over empty", "path", s.root)
	old.close()
	dir := filepath.Join(s.root, domain.SystemBucket, MetaLogDir)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("%w: storage: reset %s: %w", domain.ErrFaultyDisk, dir, err)
	}
	if err := s.mkdirAll(filepath.Join(s.root, domain.SystemBucket, TmpDir), s.root); err != nil {
		return fmt.Errorf("%w: storage: reset %s: %w", domain.ErrFaultyDisk, dir, mapErr(err))
	}
	l, err := s.startLog(dir)
	if err != nil {
		return fmt.Errorf("%w: storage: reset %s: %w", domain.ErrFaultyDisk, dir, err)
	}
	s.mlog.Store(l)
	return nil
}

// startLog opens the log of dir, converts the xl.meta files of the drive
// when its marker is missing, and runs it.
func (s *xlStorage) startLog(dir string) (*metaLog, error) {
	seg := s.segSize.Load()
	if seg == 0 {
		seg = s.opts.segmentSize
	}
	l, err := openMetaLog(dir, seg, s.opts.fsync, s.deps.Logger)
	if err != nil {
		return nil, err
	}
	if !regularFileExists(l.marker) {
		if err := s.convertToLog(l, l.marker); err != nil {
			l.close()
			return nil, err
		}
	} else if err := s.finishPrune(dir); err != nil {
		l.close()
		return nil, err
	}
	l.start()
	if p := s.memPercent.Load(); p > 0 {
		l.memMax.Store(totalMemory() / 100 * uint64(p)) // #nosec G115 -- 1..90
	}
	return l, nil
}

// volumeLock returns the lock of a volume: the writes of a drive in log
// mode hold it shared, DeleteVol exclusively, so that no record is added
// to a volume being removed (its directory is not touched by the write of
// a small object).
func (s *xlStorage) volumeLock(volume string) *sync.RWMutex {
	m, _ := s.volLocks.LoadOrStore(volume, &sync.RWMutex{})
	return m.(*sync.RWMutex) //nolint:forcetypeassert // only *sync.RWMutex is stored
}

// lockVolumeShared takes the volume lock of a write in log mode; it
// returns a no-op for a drive (or volume) without the log.
func (s *xlStorage) lockVolumeShared(volume string) func() {
	if !s.logged(volume) {
		return func() {}
	}
	m := s.volumeLock(volume)
	m.RLock()
	return m.RUnlock
}

// logObjectDir validates an object key and returns its directory without
// looking at the file system: in log mode most objects have none, and the
// symbolic links are checked (checkNoSymlinks) only before the file system
// is touched.
func (s *xlStorage) logObjectDir(volume, key string) (string, error) {
	if err := validatePath(key); err != nil {
		return "", err
	}
	for _, seg := range strings.Split(strings.TrimSuffix(key, "/"), "/") {
		if strings.HasSuffix(seg, xlDirSuffix) {
			return "", fmt.Errorf("%w: key %q uses the reserved suffix %s", domain.ErrInvalidObjectName, key, xlDirSuffix)
		}
	}
	// The segments keep the limit of a file name, as on a drive without the
	// log: the object may need a directory (its data, a conversion back).
	for _, seg := range strings.Split(onDiskKey(key), "/") {
		if len(seg) > maxNameLen {
			return "", fmt.Errorf("%w: a segment of key %q is longer than %d bytes", domain.ErrInvalidObjectName, key, maxNameLen)
		}
	}
	dir, err := s.volumeDir(volume)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.FromSlash(onDiskKey(key))), nil
}

// metaObjectDir returns the directory of an object key: checked for
// symbolic links when the drive keeps xl.meta files, only validated in log
// mode (logObjectDir).
func (s *xlStorage) metaObjectDir(volume, key string) (string, error) {
	if s.logged(volume) {
		return s.logObjectDir(volume, key)
	}
	return s.objectDir(volume, key)
}

// --- open and conversions ----------------------------------------------------

// initMetaLog opens the metadata log of the drive. With the log enabled, a
// drive that still keeps xl.meta files converts them first; with the log
// disabled, a drive whose metadata is in the log converts it back to files.
// The marker file is written once the conversion to the log is complete
// and removed once the conversion back is: an interrupted conversion is
// redone at the next open, from the side that is still complete.
func (s *xlStorage) initMetaLog() error {
	dir := filepath.Join(s.root, domain.SystemBucket, MetaLogDir)
	marker := filepath.Join(dir, metaLogMarker)
	enabled := regularFileExists(marker)
	switch {
	case s.opts.metaLog:
		l, err := s.startLog(dir)
		if err != nil {
			return err
		}
		s.mlog.Store(l)
		s.SetMetadataLogMemoryPercent(s.opts.memPercent)
	case enabled:
		l, err := openMetaLog(dir, DefaultMetaLogSegmentSize, s.opts.fsync, s.deps.Logger)
		if err != nil {
			return err
		}
		err = s.convertToFiles(l, marker)
		l.close()
		if err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			s.deps.Logger.Warn("storage: cannot remove the metadata log after its conversion", "path", dir, "err", err)
		}
	default:
		// A conversion to the log interrupted before its marker: the
		// xl.meta files are still complete.
		if _, err := os.Lstat(dir); err == nil {
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("storage: remove an unfinished metadata log %s: %w", dir, mapErr(err))
			}
		}
	}
	return nil
}

// convertToLog records the xl.meta file of every object of the user
// volumes in l (the writer not running yet), writes the marker, then
// removes the files and the directories they leave empty.
func (s *xlStorage) convertToLog(l *metaLog, marker string) error {
	vols, err := s.userVolumes()
	if err != nil {
		return err
	}
	count := 0
	for _, volume := range vols {
		var batch []*logReq
		err := s.walkMetaFiles(volume, func(key, path string) error {
			data, err := readFile(path)
			if err != nil {
				return fmt.Errorf("storage: read %s: %w", path, mapErr(err))
			}
			batch = append(batch, &logReq{typ: recPut, volume: volume, key: key, payload: data})
			if len(batch) >= maxBatchRecords {
				if err := l.commit(batch); err != nil {
					return err
				}
				count += len(batch)
				batch = batch[:0]
			}
			return nil
		})
		if err == nil && len(batch) > 0 {
			err = l.commit(batch)
			count += len(batch)
		}
		if err != nil {
			return fmt.Errorf("storage: convert %s to the metadata log: %w", volume, err)
		}
	}
	// The files become stale with the marker: PRUNE, written first,
	// makes the next start remove them if this one cannot.
	prune := filepath.Join(filepath.Dir(marker), metaLogPrune)
	if err := writeMarker(prune, s.opts.fsync); err != nil {
		return err
	}
	if err := writeMarker(marker, s.opts.fsync); err != nil {
		return err
	}
	if err := s.finishPrune(filepath.Dir(marker)); err != nil {
		return err
	}
	s.deps.Logger.Info("storage: drive converted to the metadata log", "path", s.root, "objects", count)
	return nil
}

// finishPrune removes the xl.meta files of the user volumes, and the
// directories they leave empty, when the PRUNE file of the log directory
// says a conversion left some: in log mode they are stale copies, which a
// later conversion back to files must not find (an object deleted since
// would reappear).
func (s *xlStorage) finishPrune(dir string) error {
	prune := filepath.Join(dir, metaLogPrune)
	if !regularFileExists(prune) {
		return nil
	}
	vols, err := s.userVolumes()
	if err != nil {
		return err
	}
	for _, volume := range vols {
		volDir := filepath.Join(s.root, volume)
		err := s.walkMetaFiles(volume, func(_, path string) error {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("storage: remove %s: %w", path, mapErr(err))
			}
			s.syncDir(filepath.Dir(path))
			s.pruneEmptyDirs(filepath.Dir(path), volDir)
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err := os.Remove(prune); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("storage: remove %s: %w", prune, mapErr(err))
	}
	s.syncDir(dir)
	return nil
}

// convertToFiles writes the xl.meta file of every object of l, then
// removes the marker. PRUNE is written first: if the drive goes back to the
// log before the conversion ends, the files written so far are removed.
func (s *xlStorage) convertToFiles(l *metaLog, marker string) error {
	if err := writeMarker(filepath.Join(filepath.Dir(marker), metaLogPrune), s.opts.fsync); err != nil {
		return err
	}
	count := 0
	for _, volume := range l.volumes() {
		volDir, err := s.ensureVolume(volume)
		if err != nil {
			s.deps.Logger.Warn("storage: metadata log of a missing volume not converted", "volume", volume, "err", err)
			continue
		}
		for start := ""; ; {
			keys := l.scan(volume, start, 1024)
			for _, key := range keys {
				data, err := l.read(volume, key, true)
				if err != nil {
					return fmt.Errorf("storage: convert %s/%s to a file: %w", volume, key, err)
				}
				dir, err := s.objectDir(volume, key)
				if err != nil {
					// A key below a symbolic link added on the drive:
					// never written through it.
					s.deps.Logger.Error("storage: object of the metadata log not converted to a file", "volume", volume, "key", key, "err", err)
					continue
				}
				if err := s.writeAtomic(filepath.Join(dir, MetaFile), volDir, data, s.opts.fsync); err != nil {
					return fmt.Errorf("storage: convert %s/%s to a file: %w", volume, key, s.writeErr(volume, key, err))
				}
				count++
			}
			if len(keys) < 1024 {
				break
			}
			start = keys[len(keys)-1] + "\x00"
		}
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("storage: remove %s: %w", marker, mapErr(err))
	}
	syncDirPath(filepath.Dir(marker))
	s.deps.Logger.Info("storage: drive converted back to xl.meta files", "path", s.root, "objects", count)
	return nil
}

// userVolumes lists the user volumes of the drive.
func (s *xlStorage) userVolumes() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("storage: read %s: %w", s.root, mapErr(err))
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// walkMetaFiles calls fn with the key and the path of every xl.meta file of
// a volume. Symbolic links are not followed.
func (s *xlStorage) walkMetaFiles(volume string, fn func(key, path string) error) error {
	volDir := filepath.Join(s.root, volume)
	return filepath.WalkDir(volDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Name() != MetaFile || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(volDir, filepath.Dir(path))
		if err != nil || rel == "." {
			return nil //nolint:nilerr // an xl.meta at the volume root is not an object
		}
		segs := strings.Split(filepath.ToSlash(rel), "/")
		segs[len(segs)-1], _ = keyFromDirName(segs[len(segs)-1])
		return fn(strings.Join(segs, "/"), path)
	})
}

// writeMarker creates the marker file of a drive in log mode.
func writeMarker(path string, sync bool) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640) // #nosec G304 -- the marker of the log directory
	if err != nil {
		return fmt.Errorf("storage: write %s: %w", path, mapErr(err))
	}
	_, err = f.WriteString("nexo metadata log\n")
	if err == nil && sync {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("storage: write %s: %w", path, mapErr(err))
	}
	if sync {
		syncDirPath(filepath.Dir(path))
	}
	return nil
}
