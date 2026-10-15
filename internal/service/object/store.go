package object

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// store is how the layer reaches the drives: a single drive (fs) or one
// erasure set (erasure). Paths follow the StorageAPI conventions; system
// paths are relative to domain.SystemBucket. The erasure implementation
// applies the quorum rules of its set: callers see one logical drive.
type store interface {
	MakeVol(ctx context.Context, volume string) error
	ListVols(ctx context.Context) ([]storage.VolInfo, error)
	StatVol(ctx context.Context, volume string) (storage.VolInfo, error)
	DeleteVol(ctx context.Context, volume string, force bool) error

	ReadVersion(ctx context.Context, volume, path, versionID string, readData bool) (storage.FileInfo, error)
	WriteMetadata(ctx context.Context, volume, path string, fi storage.FileInfo) error
	UpdateMetadata(ctx context.Context, volume, path string, fi storage.FileInfo) error
	DeleteVersion(ctx context.Context, volume, path string, fi storage.FileInfo) error

	// ErasureInfo returns the layout of a new version of key in the given
	// storage class: data/parity split, block size and the distribution
	// of the shards over the drives (identity for fs).
	ErasureInfo(key, storageClass string) storage.ErasureInfo
	// WriteData streams r (size bytes, -1 when unknown) into part partNum
	// of the system directory dir ("tmp/<id>") with layout ei and returns
	// the number of logical bytes consumed. On failure nothing is left in
	// dir.
	WriteData(ctx context.Context, dir string, partNum int, size int64, r io.Reader, ei storage.ErasureInfo) (int64, error)
	// CommitData moves the system directory tmpDir into place as the data
	// directory fi.DataDir of volume/path and records fi, on the drives
	// whose tmpDir holds every part of fi, then purges the data directory
	// of the version it replaced on each of them (unless ctx reports a lost
	// namespace lock: another writer may be committing). On failure tmpDir
	// is left where it is (wherever the move did not happen, or moved back
	// where a partial commit was rolled back): the caller removes it or,
	// for a multipart completion, restores its parts.
	CommitData(ctx context.Context, tmpDir string, fi storage.FileInfo, volume, path string) error
	// ReadData streams [offset, offset+length) of the logical content of
	// part of the version fi.
	ReadData(ctx context.Context, volume, path string, fi storage.FileInfo, part storage.PartInfo, offset, length int64) (io.ReadCloser, error)
	// OpenData is ReadData with the files of the stream opened before it
	// returns (ReadData may open them at the first read). pinned reports
	// that every file the stream needs is open, so that it keeps reading
	// the version after a writer purged its data directory (an open file
	// survives its removal, see pinnedReads): the read lock of the object
	// can then be released before the stream is consumed.
	OpenData(ctx context.Context, volume, path string, fi storage.FileInfo, part storage.PartInfo, offset, length int64) (rc io.ReadCloser, pinned bool, err error)

	ReadAll(ctx context.Context, path string) ([]byte, error)
	WriteAll(ctx context.Context, path string, data []byte) error
	DeletePath(ctx context.Context, volume, path string, recursive bool) error
	// MoveFiles moves system files (each move from src to dst) on every
	// drive. On each drive the moves are all or nothing: a drive that fails
	// one of them moves back the files it moved. Drives that fail are left
	// out while the write quorum of layout ei holds (their files stay at
	// the source); otherwise the moves are undone everywhere and the
	// reduced error is returned.
	MoveFiles(ctx context.Context, moves []fileMove, ei storage.ErasureInfo) error
	// UndoMoves moves back, on every drive and best effort, the files of
	// moves found at their destination: the parts of a multipart upload
	// whose completion failed after StageParts, wherever they were moved.
	UndoMoves(ctx context.Context, moves []fileMove)
	// StageParts assembles the data of a multipart completion in
	// stagingDir: it moves the data files of the selected parts (moves[k]
	// holds part fi.Parts[k]) on every drive, file by file, then rebuilds,
	// on the online drives that miss a part, its shards from the drives
	// that hold it: the parts of an upload may have been acknowledged on
	// different drives (a rolling restart during a long upload), each on a
	// write quorum. It fails with domain.ErrObjectNotFound when a part can
	// no longer be decoded, with domain.ErrWriteQuorum when the drives
	// that would make it decodable are offline or failing. Whatever the
	// outcome, the caller restores the parts with UndoMoves when the
	// completion does not commit; CommitData then commits the drives whose
	// staged data is complete. A single drive moves all or nothing.
	StageParts(ctx context.Context, moves []fileMove, stagingDir string, fi storage.FileInfo) error
	StatGlob(ctx context.Context, pattern string) ([]storage.StatInfo, error)
	// WalkVersions walks the entries of a bucket in S3 key order (see
	// StorageAPI.WalkDir) with the versions of every object decoded (and,
	// on an erasure set, agreed by a read quorum of drives). opts.Limit is
	// ignored: fn stops the walk by returning an error, which WalkVersions
	// returns.
	WalkVersions(ctx context.Context, opts storage.WalkDirOptions, fn func(walkEntry) error) error

	// Online reports the online and total drive counts.
	Online() (online, total int)
}

// fileMove is one file move of MoveFiles: system paths, relative to
// domain.SystemBucket.
type fileMove struct {
	src, dst string
}

// reversed returns the moves that undo moves.
func reversed(moves []fileMove) []fileMove {
	out := make([]fileMove, len(moves))
	for i, m := range moves {
		out[i] = fileMove{src: m.dst, dst: m.src}
	}
	return out
}

// moveAll moves the files of moves on drive d, all or nothing: when a move
// fails, the moves made so far are undone, and so is the failed one, which
// a remote drive may have made although the call reported a failure.
func moveAll(ctx context.Context, d storage.StorageAPI, moves []fileMove, logger *slog.Logger) error {
	for j, m := range moves {
		if err := d.RenameFile(ctx, domain.SystemBucket, m.src, domain.SystemBucket, m.dst); err != nil {
			undoMoves(ctx, d, moves[:j+1], logger)
			return err
		}
	}
	return nil
}

// undoMoves moves back the files of moves found at their destination on
// drive d, best effort and even when ctx is canceled: a move that did not
// happen (destination missing) is skipped.
func undoMoves(ctx context.Context, d storage.StorageAPI, moves []fileMove, logger *slog.Logger) {
	ctx = context.WithoutCancel(ctx)
	for _, m := range reversed(moves) {
		err := d.RenameFile(ctx, domain.SystemBucket, m.src, domain.SystemBucket, m.dst)
		if err != nil && !isNotFound(err) {
			logger.WarnContext(ctx, "object: cannot move a file back", "from", m.src, "to", m.dst,
				"drive", d.Endpoint().String(), "err", err)
		}
	}
}

// walkEntry is one entry of WalkVersions: a prefix (IsDir, the name ends
// with a slash) or an object with its versions, latest first.
type walkEntry struct {
	Name     string
	IsDir    bool
	Versions []storage.FileInfo
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
	// Errors are grouped by sentinel, in order of first appearance, so
	// that ties are broken deterministically.
	var keys []error
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
		if _, seen := first[key]; !seen {
			first[key] = err
			keys = append(keys, key)
		}
		counts[key]++
	}
	if ok >= quorum {
		return nil
	}
	var best error
	bestCount := 0
	for _, key := range keys {
		if c := counts[key]; c > bestCount {
			best, bestCount = first[key], c
		}
	}
	if best != nil && bestCount >= quorum {
		if fallback != nil && errors.Is(best, domain.ErrDiskNotFound) {
			// The drives that agree are the offline ones: the quorum is lost.
			return fmt.Errorf("%w: %w", fallback, best)
		}
		return best
	}
	if best != nil && fallback == nil {
		return best
	}
	return fallback
}
