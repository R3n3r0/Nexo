package object

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// fsObjects is the single-drive object layer (SNSD): objects are stored as
// plain files on one StorageAPI, without erasure coding.
type fsObjects struct {
	*layer
	drive storage.StorageAPI
}

var _ Service = (*fsObjects)(nil)

// NewFS returns the filesystem object layer over deps.Store.
func NewFS(deps Deps, opts ...Option) (Service, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("%w: object: NewFS requires a storage drive", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	st := &singleDrive{d: deps.Store, logger: deps.Logger}
	l := newLayer(deps, opts, []store{st}, "fs")
	return &fsObjects{layer: l, drive: deps.Store}, nil
}

// Health implements Service.
func (f *fsObjects) Health(_ context.Context, opts HealthOptions) HealthResult {
	res := HealthResult{Healthy: f.drive.IsOnline(), WriteQuorum: 1, ReadQuorum: 1}
	if !res.Healthy {
		res.Message = "drive " + f.drive.Endpoint().String() + " is offline"
	}
	if opts.Maintenance {
		res.Healthy = false
		res.Message = "a single-drive deployment cannot lose its drive"
	}
	return res
}

// singleDrive adapts one StorageAPI to the store contract.
type singleDrive struct {
	d      storage.StorageAPI
	logger *slog.Logger
}

var _ store = (*singleDrive)(nil)

func (s *singleDrive) MakeVol(ctx context.Context, volume string) error {
	return s.d.MakeVol(ctx, volume)
}

func (s *singleDrive) ListVols(ctx context.Context) ([]storage.VolInfo, error) {
	return s.d.ListVols(ctx)
}

func (s *singleDrive) StatVol(ctx context.Context, volume string) (storage.VolInfo, error) {
	return s.d.StatVol(ctx, volume)
}

func (s *singleDrive) DeleteVol(ctx context.Context, volume string, force bool) error {
	return s.d.DeleteVol(ctx, volume, force)
}

func (s *singleDrive) ReadVersion(ctx context.Context, volume, path, versionID string, readData bool) (storage.FileInfo, error) {
	return s.d.ReadVersion(ctx, volume, path, versionID, storage.ReadOptions{ReadData: readData})
}

func (s *singleDrive) WriteMetadata(ctx context.Context, volume, path string, fi storage.FileInfo) error {
	return s.d.WriteMetadata(ctx, volume, path, fi)
}

func (s *singleDrive) UpdateMetadata(ctx context.Context, volume, path string, fi storage.FileInfo) error {
	return s.d.UpdateMetadata(ctx, volume, path, fi, storage.UpdateMetadataOptions{})
}

func (s *singleDrive) DeleteVersion(ctx context.Context, volume, path string, fi storage.FileInfo) error {
	return s.d.DeleteVersion(ctx, volume, path, fi, storage.DeleteOptions{})
}

// countingReader counts the bytes handed to the drive.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *singleDrive) WriteData(ctx context.Context, dir string, partNum int, size int64, r io.Reader, _ storage.ErasureInfo) (int64, error) {
	cr := &countingReader{r: r}
	if err := s.d.CreateFile(ctx, domain.SystemBucket, dir+"/"+partName(partNum), size, cr); err != nil {
		return cr.n, err
	}
	return cr.n, nil
}

func (s *singleDrive) ErasureInfo(string, string) storage.ErasureInfo {
	return storage.ErasureInfo{Algorithm: "none", DataBlocks: 1, ParityBlocks: 0, Index: 1, Distribution: []int{1}}
}

func (s *singleDrive) CommitData(ctx context.Context, tmpDir string, fi storage.FileInfo, volume, path string) error {
	res, err := s.d.RenameData(ctx, domain.SystemBucket, tmpDir, fi, volume, path, storage.RenameOptions{})
	if err != nil {
		return err
	}
	if res.OldDataDir == "" || res.OldDataDir == fi.DataDir {
		return nil
	}
	if lockLost(ctx) {
		s.logger.WarnContext(ctx, "object: lock lost after the commit, replaced data left in place", "bucket", volume, "key", path)
		return nil
	}
	// The commit is done: the replaced data goes even when the client went
	// away in the meantime.
	ctx = context.WithoutCancel(ctx)
	err = s.d.Delete(ctx, volume, storage.ObjectDataDir(path, res.OldDataDir), storage.DeleteOptions{Recursive: true})
	if err != nil && !isNotFound(err) {
		s.logger.WarnContext(ctx, "object: cannot purge replaced data", "bucket", volume, "key", path, "data_dir", res.OldDataDir, "err", err)
	}
	return nil
}

func (s *singleDrive) ReadData(ctx context.Context, volume, path string, fi storage.FileInfo, part storage.PartInfo, offset, length int64) (io.ReadCloser, error) {
	return s.d.ReadFileStream(ctx, volume, storage.ObjectDataPath(path, fi.DataDir, part.Number), offset, length)
}

func (s *singleDrive) ReadAll(ctx context.Context, path string) ([]byte, error) {
	return s.d.ReadAll(ctx, domain.SystemBucket, path)
}

func (s *singleDrive) WriteAll(ctx context.Context, path string, data []byte) error {
	return s.d.WriteAll(ctx, domain.SystemBucket, path, data)
}

func (s *singleDrive) DeletePath(ctx context.Context, volume, path string, recursive bool) error {
	return s.d.Delete(ctx, volume, path, storage.DeleteOptions{Recursive: recursive})
}

func (s *singleDrive) MoveFiles(ctx context.Context, moves []fileMove, _ storage.ErasureInfo) error {
	return moveAll(ctx, s.d, moves, s.logger)
}

func (s *singleDrive) StageParts(ctx context.Context, moves []fileMove, _ string, _ storage.FileInfo) error {
	return moveAll(ctx, s.d, moves, s.logger)
}

func (s *singleDrive) UndoMoves(ctx context.Context, moves []fileMove) {
	undoMoves(ctx, s.d, moves, s.logger)
}

func (s *singleDrive) StatGlob(ctx context.Context, pattern string) ([]storage.StatInfo, error) {
	return s.d.StatInfoFile(ctx, domain.SystemBucket, pattern, true)
}

func (s *singleDrive) WalkVersions(ctx context.Context, opts storage.WalkDirOptions, fn func(walkEntry) error) error {
	opts.Limit = 0
	return s.d.WalkDir(ctx, opts, func(e storage.WalkEntry) error {
		if e.IsDir {
			return fn(walkEntry{Name: e.Name, IsDir: true})
		}
		versions, err := storage.DecodeFileInfos(opts.Bucket, e.Name, e.Metadata)
		if err != nil {
			s.logger.WarnContext(ctx, "object: skipping unreadable metadata", "bucket", opts.Bucket, "key", e.Name, "err", err)
			return nil
		}
		return fn(walkEntry{Name: e.Name, Versions: versions})
	})
}

func (s *singleDrive) Online() (int, int) {
	if s.d.IsOnline() {
		return 1, 1
	}
	return 0, 1
}
