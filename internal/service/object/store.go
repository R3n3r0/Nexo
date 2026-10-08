package object

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// store is how the layer reaches the drives: a single drive (fs) or one
// erasure set (erasure). Paths follow the StorageAPI conventions; system
// paths are relative to domain.SystemBucket.
type store interface {
	MakeVol(ctx context.Context, volume string) error
	ListVols(ctx context.Context) ([]storage.VolInfo, error)
	StatVol(ctx context.Context, volume string) (storage.VolInfo, error)
	DeleteVol(ctx context.Context, volume string, force bool) error

	ReadVersion(ctx context.Context, volume, path, versionID string, readData bool) (storage.FileInfo, error)
	WriteMetadata(ctx context.Context, volume, path string, fi storage.FileInfo) error
	UpdateMetadata(ctx context.Context, volume, path string, fi storage.FileInfo) error
	DeleteVersion(ctx context.Context, volume, path string, fi storage.FileInfo) error

	// WriteData streams r into part partNum of the system directory dir
	// ("tmp/<id>") and returns the number of logical bytes consumed.
	WriteData(ctx context.Context, dir string, partNum int, size int64, r io.Reader) (int64, error)
	// ErasureInfo describes the layout WriteData produces (identity for fs).
	ErasureInfo() storage.ErasureInfo
	// CommitData moves the system directory tmpDir into place as the data
	// directory fi.DataDir of volume/path and records fi. It returns the
	// data directory of the replaced version, if any. On failure tmpDir is
	// left where it is (wherever the move did not happen): the caller
	// removes it or, for a multipart completion, restores its parts.
	CommitData(ctx context.Context, tmpDir string, fi storage.FileInfo, volume, path string) (string, error)
	// ReadData streams [offset, offset+length) of the logical content of
	// part of the version fi.
	ReadData(ctx context.Context, volume, path string, fi storage.FileInfo, part storage.PartInfo, offset, length int64) (io.ReadCloser, error)

	ReadAll(ctx context.Context, path string) ([]byte, error)
	WriteAll(ctx context.Context, path string, data []byte) error
	DeletePath(ctx context.Context, volume, path string, recursive bool) error
	RenameFile(ctx context.Context, srcPath, dstPath string) error
	StatGlob(ctx context.Context, pattern string) ([]storage.StatInfo, error)
	Walk(ctx context.Context, opts storage.WalkDirOptions, fn storage.WalkDirFunc) error

	// Online reports the online and total drive counts.
	Online() (online, total int)
}

// parallel runs fn for every index concurrently and collects the errors.
func parallel(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = fn(i)
		}(i)
	}
	wg.Wait()
	return errs
}

// knownSentinels returns the errors drives may legitimately agree on.
func knownSentinels() []error {
	return []error{
		domain.ErrBucketNotFound, domain.ErrBucketAlreadyExists, domain.ErrBucketNotEmpty, domain.ErrInvalidBucketName,
		domain.ErrObjectNotFound, domain.ErrInvalidObjectName, domain.ErrNoSuchVersion, domain.ErrInvalidRange,
		domain.ErrStorageFull, domain.ErrDiskNotFound, domain.ErrFaultyDisk, domain.ErrInvalidArgument,
		domain.ErrInvalidFormat, context.Canceled, context.DeadlineExceeded,
	}
}

// reduceErrs returns nil when at least quorum calls succeeded, the error at
// least quorum drives agree on otherwise, and fallback when there is no
// agreement. Agreement is by domain sentinel so that wrapped errors match.
func reduceErrs(errs []error, quorum int, fallback error) error {
	ok := 0
	sentinels := knownSentinels()
	counts := make(map[error]int)
	first := make(map[error]error)
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		key := err
		for _, s := range sentinels {
			if errors.Is(err, s) {
				key = s
				break
			}
		}
		counts[key]++
		if _, seen := first[key]; !seen {
			first[key] = err
		}
	}
	if ok >= quorum {
		return nil
	}
	var best error
	bestCount := 0
	for key, c := range counts {
		if c > bestCount {
			best, bestCount = first[key], c
		}
	}
	if best != nil && bestCount >= quorum {
		return best
	}
	if best != nil && fallback == nil {
		return best
	}
	return fallback
}
