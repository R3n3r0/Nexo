package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
)

func newDrive(t *testing.T) StorageAPI {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "drive")
	ep, err := domain.ParseEndpoint(dir)
	require.NoError(t, err)
	s, err := NewLocal(Deps{Logger: slog.New(slog.DiscardHandler)}, ep, WithoutFsync())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func version(id string, at time.Time, size int64) FileInfo {
	return FileInfo{VersionID: id, ModTime: at, Size: size, Metadata: map[string]string{"content-type": "text/plain"}}
}

func TestNewLocalInitializesDrive(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "d1")
	// Stale temporary entries are removed at startup.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, domain.SystemBucket, TmpDir, "stale"), 0o750))
	ep, _ := domain.ParseEndpoint(dir)
	s, err := NewLocal(Deps{}, ep, WithoutFsync())
	require.NoError(t, err)
	for _, d := range []string{TmpDir, MultipartDir, BucketsDir} {
		st, err := os.Stat(filepath.Join(dir, domain.SystemBucket, d))
		require.NoError(t, err, d)
		assert.True(t, st.IsDir())
	}
	_, err = os.Stat(filepath.Join(dir, domain.SystemBucket, TmpDir, "stale"))
	assert.True(t, errors.Is(err, fs.ErrNotExist), "tmp is emptied")
	assert.Equal(t, ep, s.Endpoint())
	assert.True(t, s.IsOnline())
	assert.Nil(t, s.Healing())

	info, err := s.DiskInfo(ctx, DiskInfoOptions{})
	require.NoError(t, err)
	assert.Equal(t, domain.StateOnline, info.State)
	assert.Empty(t, info.ID, "unformatted drive has no id")
	assert.NotZero(t, info.Total)
	assert.NotEmpty(t, info.FSType)
	cached, err := s.DiskInfo(ctx, DiskInfoOptions{NoOp: true})
	require.NoError(t, err)
	assert.Equal(t, info.Total, cached.Total, "NoOp returns the figures of the previous call")
	assert.Equal(t, info.FSType, cached.FSType)
	_, err = s.DiskInfo(ctx, DiskInfoOptions{DiskID: "other"})
	assert.ErrorIs(t, err, domain.ErrDiskNotFound)

	// Keeping tmp content is an option.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, domain.SystemBucket, TmpDir, "keep"), 0o750))
	_, err = NewLocal(Deps{}, ep, WithoutTmpCleanup())
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, domain.SystemBucket, TmpDir, "keep"))
	assert.NoError(t, err)

	// Close makes the drive offline and refuses operations.
	require.NoError(t, s.Close(ctx))
	assert.False(t, s.IsOnline())
	_, err = s.ListVols(ctx)
	assert.ErrorIs(t, err, domain.ErrDiskNotFound)

	// URL endpoints and empty paths are refused.
	_, err = NewLocal(Deps{}, domain.Endpoint{Scheme: "http", Host: "h:9000", Path: "/d"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = NewLocal(Deps{}, domain.Endpoint{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	// An unwritable location fails the probe.
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	ep2, _ := domain.ParseEndpoint(filepath.Join(file, "sub"))
	_, err = NewLocal(Deps{}, ep2)
	assert.Error(t, err)
}

func TestVolumes(t *testing.T) {
	ctx := context.Background()
	s := newDrive(t)

	require.NoError(t, s.MakeVol(ctx, "photos"))
	assert.ErrorIs(t, s.MakeVol(ctx, "photos"), domain.ErrBucketAlreadyExists)
	assert.ErrorIs(t, s.MakeVol(ctx, "a/b"), domain.ErrInvalidBucketName)
	assert.ErrorIs(t, s.MakeVol(ctx, ".."), domain.ErrInvalidBucketName)
	require.NoError(t, s.MakeVol(ctx, "docs"))

	vols, err := s.ListVols(ctx)
	require.NoError(t, err)
	names := make([]string, 0, len(vols))
	for _, v := range vols {
		names = append(names, v.Name)
		assert.False(t, v.Created.IsZero())
	}
	assert.ElementsMatch(t, []string{"photos", "docs"}, names, "the system volume is hidden")

	vi, err := s.StatVol(ctx, "photos")
	require.NoError(t, err)
	assert.Equal(t, "photos", vi.Name)
	_, err = s.StatVol(ctx, "missing")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)

	require.NoError(t, s.WriteAll(ctx, "photos", "a/b.txt", []byte("x")))
	assert.ErrorIs(t, s.DeleteVol(ctx, "photos", false), domain.ErrBucketNotEmpty)
	require.NoError(t, s.DeleteVol(ctx, "photos", true))
	require.NoError(t, s.DeleteVol(ctx, "docs", false))
	assert.ErrorIs(t, s.DeleteVol(ctx, "docs", false), domain.ErrBucketNotFound)
	assert.ErrorIs(t, s.DeleteVol(ctx, domain.SystemBucket, true), domain.ErrInvalidArgument)
}

func TestMetadataLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newDrive(t)
	require.NoError(t, s.MakeVol(ctx, "b"))
	t0 := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)

	_, err := s.ReadVersion(ctx, "b", "k", "", ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	_, err = s.ReadVersion(ctx, "nobucket", "k", "", ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	assert.ErrorIs(t, s.WriteMetadata(ctx, "nobucket", "k", version("", t0, 1)), domain.ErrBucketNotFound)

	// Null version with inline data.
	v0 := version("", t0, 5)
	v0.Data = []byte("hello")
	require.NoError(t, s.WriteMetadata(ctx, "b", "k", v0))
	got, err := s.ReadVersion(ctx, "b", "k", "", ReadOptions{ReadData: true})
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), got.Data)
	assert.True(t, got.IsLatest)
	assert.Equal(t, 1, got.NumVersions)
	assert.Equal(t, "b", got.Volume)
	assert.Equal(t, "k", got.Name)
	assert.Equal(t, "text/plain", got.Metadata["content-type"])
	noData, err := s.ReadVersion(ctx, "b", "k", "", ReadOptions{})
	require.NoError(t, err)
	assert.Nil(t, noData.Data)

	// Rewriting the null version replaces it.
	v0b := version("", t0.Add(time.Minute), 6)
	require.NoError(t, s.WriteMetadata(ctx, "b", "k", v0b))
	got, err = s.ReadVersion(ctx, "b", "k", "", ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(6), got.Size)
	assert.Equal(t, 1, got.NumVersions)

	// Versioned writes accumulate, newest first.
	require.NoError(t, s.WriteMetadata(ctx, "b", "k", version("v1", t0.Add(2*time.Minute), 7)))
	require.NoError(t, s.WriteMetadata(ctx, "b", "k", version("v2", t0.Add(3*time.Minute), 8)))
	got, err = s.ReadVersion(ctx, "b", "k", "", ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, "v2", got.VersionID)
	assert.Equal(t, 3, got.NumVersions)
	old, err := s.ReadVersion(ctx, "b", "k", "v1", ReadOptions{})
	require.NoError(t, err)
	assert.False(t, old.IsLatest)
	assert.Equal(t, 1, old.Idx)
	assert.Equal(t, got.ModTime, old.SuccessorModTime)
	_, err = s.ReadVersion(ctx, "b", "k", "nope", ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrNoSuchVersion)

	// UpdateMetadata changes a version in place and keeps its data.
	old.Metadata["x-amz-tagging"] = "a=1"
	require.NoError(t, s.UpdateMetadata(ctx, "b", "k", old, UpdateMetadataOptions{}))
	old2, err := s.ReadVersion(ctx, "b", "k", "v1", ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, "a=1", old2.Metadata["x-amz-tagging"])
	inline, err := s.ReadVersion(ctx, "b", "inl", "", ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	withData := version("", t0, 2)
	withData.Data = []byte("hi")
	require.NoError(t, s.WriteMetadata(ctx, "b", "inl", withData))
	inline, err = s.ReadVersion(ctx, "b", "inl", "", ReadOptions{})
	require.NoError(t, err)
	inline.Metadata["x"] = "y"
	require.NoError(t, s.UpdateMetadata(ctx, "b", "inl", inline, UpdateMetadataOptions{}))
	inline, err = s.ReadVersion(ctx, "b", "inl", "", ReadOptions{ReadData: true})
	require.NoError(t, err)
	assert.Equal(t, "hi", string(inline.Data), "a metadata update without data keeps the inline content")
	assert.Equal(t, "y", inline.Metadata["x"])
	assert.ErrorIs(t, s.UpdateMetadata(ctx, "b", "k", version("missing", t0, 1), UpdateMetadataOptions{NoPersistence: true}), domain.ErrNoSuchVersion)
	assert.ErrorIs(t, s.UpdateMetadata(ctx, "b", "nokey", old, UpdateMetadataOptions{}), domain.ErrObjectNotFound)

	// Delete marker becomes the latest version.
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", FileInfo{VersionID: "dm", ModTime: t0.Add(4 * time.Minute), MarkDeleted: true}, DeleteOptions{}))
	got, err = s.ReadVersion(ctx, "b", "k", "", ReadOptions{})
	require.NoError(t, err)
	assert.True(t, got.Deleted)
	assert.Equal(t, "dm", got.VersionID)
	assert.Equal(t, 4, got.NumVersions)

	// Removing versions one by one, including the null version.
	assert.ErrorIs(t, s.DeleteVersion(ctx, "b", "k", FileInfo{VersionID: "ghost"}, DeleteOptions{}), domain.ErrNoSuchVersion)
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", FileInfo{VersionID: "dm"}, DeleteOptions{}))
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", FileInfo{VersionID: ""}, DeleteOptions{}))
	got, err = s.ReadVersion(ctx, "b", "k", "", ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, got.NumVersions)
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", FileInfo{VersionID: "v2"}, DeleteOptions{}))
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", FileInfo{VersionID: "v1"}, DeleteOptions{}))
	_, err = s.ReadVersion(ctx, "b", "k", "", ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "last version removed → object gone")
	_, err = os.Stat(filepath.Join(drivePath(s), "b", "k"))
	assert.True(t, errors.Is(err, fs.ErrNotExist), "empty object directory is pruned")

	// A delete marker on a missing object is recorded (S3 semantics).
	require.NoError(t, s.DeleteVersion(ctx, "b", "new", FileInfo{VersionID: "m1", ModTime: t0, MarkDeleted: true}, DeleteOptions{}))
	got, err = s.ReadVersion(ctx, "b", "new", "", ReadOptions{})
	require.NoError(t, err)
	assert.True(t, got.Deleted)
	assert.ErrorIs(t, s.DeleteVersion(ctx, "b", "absent", FileInfo{VersionID: "x"}, DeleteOptions{}), domain.ErrObjectNotFound)

	// Directory objects ("photos/") do not collide with the prefix.
	require.NoError(t, s.WriteMetadata(ctx, "b", "photos/", version("", t0, 0)))
	require.NoError(t, s.WriteMetadata(ctx, "b", "photos/a.jpg", version("", t0, 3)))
	_, err = s.ReadVersion(ctx, "b", "photos/", "", ReadOptions{})
	require.NoError(t, err)
	_, err = s.ReadVersion(ctx, "b", "photos", "", ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)

	// Invalid paths.
	assert.ErrorIs(t, s.WriteMetadata(ctx, "b", "../x", version("", t0, 0)), domain.ErrInvalidObjectName)
	assert.ErrorIs(t, s.WriteMetadata(ctx, "b", "", version("", t0, 0)), domain.ErrInvalidObjectName)
	assert.ErrorIs(t, s.WriteMetadata(ctx, "b", "a//b", version("", t0, 0)), domain.ErrInvalidObjectName)
}

func TestObjectPlacementConflicts(t *testing.T) {
	ctx := context.Background()
	s := newDrive(t)
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()
	require.NoError(t, s.WriteMetadata(ctx, "b", "dir/file", version("", now, 1)))

	// The parent prefix "dir" cannot become an object...
	err := s.WriteMetadata(ctx, "b", "dir", version("", now, 1))
	assert.ErrorIs(t, err, domain.ErrInvalidObjectName)
	var s3err *domain.S3Error
	require.ErrorAs(t, err, &s3err)
	assert.Equal(t, "XNexoObjectExistsAsDirectory", s3err.Code)

	// ...and no object can be created below an existing object.
	err = s.WriteMetadata(ctx, "b", "dir/file/child", version("", now, 1))
	assert.ErrorIs(t, err, domain.ErrInvalidObjectName)
	require.ErrorAs(t, err, &s3err)
	assert.Equal(t, "XNexoParentIsObject", s3err.Code)

	// Data directories inside an object are not prefixes: rewriting is fine.
	fi := version("", now, 4)
	fi.DataDir = "0d8a8a3e-6a8f-4f4a-9b4f-1b9b9b9b9b9b"
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/up1/part.1", 4, strings.NewReader("data")))
	_, err = s.RenameData(ctx, domain.SystemBucket, "tmp/up1", fi, "b", "dir/file", RenameOptions{})
	require.NoError(t, err)
	require.NoError(t, s.WriteMetadata(ctx, "b", "dir/file", version("", now.Add(time.Second), 4)))
}

// TestObjectPlacementUUIDPrefix is a regression test: a child prefix whose
// name is a UUID ("x/<uuid>/y") is a prefix, not a data directory of "x".
// Writing "x" over it must be refused, and deleting "x" must never purge it.
func TestObjectPlacementUUIDPrefix(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	ctx := context.Background()
	now := time.Now()
	for _, child := range []string{"x/" + id + "/y", "x/" + id + "/deep/y", "x/" + id + "/"} {
		t.Run(child, func(t *testing.T) {
			s := newDrive(t)
			require.NoError(t, s.MakeVol(ctx, "b"))
			require.NoError(t, s.WriteMetadata(ctx, "b", child, version("", now, 1)))
			err := s.WriteMetadata(ctx, "b", "x", version("", now, 1))
			var s3err *domain.S3Error
			require.ErrorAs(t, err, &s3err)
			assert.Equal(t, "XNexoObjectExistsAsDirectory", s3err.Code)
			// A delete marker cannot create "x" either.
			err = s.DeleteVersion(ctx, "b", "x", FileInfo{VersionID: "11111111-1111-1111-1111-111111111111", ModTime: now, MarkDeleted: true}, DeleteOptions{})
			require.ErrorAs(t, err, &s3err)
			assert.Equal(t, "XNexoObjectExistsAsDirectory", s3err.Code)
			_, err = s.ReadVersion(ctx, "b", child, "", ReadOptions{})
			require.NoError(t, err)
		})
	}
}

// TestDeleteKeepsUUIDChildPrefix is a regression test: on a drive where an
// object "x" coexists with a UUID-named child prefix (written before the
// placement check knew about it), removing the last version of "x" purges
// its data directories but never the child prefix.
func TestDeleteKeepsUUIDChildPrefix(t *testing.T) {
	const (
		childID = "0f8fad5b-d9cb-469f-a165-70867728950e"
		dataID  = "3f2b2b48-5c3a-4a1e-8a2a-123456789abc"
		orphan  = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	)
	ctx := context.Background()
	now := time.Now()
	s := newDrive(t)
	xs := s.(*xlStorage)
	require.NoError(t, s.MakeVol(ctx, "b"))
	child := "x/" + childID + "/y"
	require.NoError(t, s.WriteMetadata(ctx, "b", child, version("", now, 1)))
	// Forge the conflicting object "x" directly on disk.
	fi := version("", now, 4)
	fi.DataDir = dataID
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/up/part.1", 4, strings.NewReader("data")))
	objDir, err := xs.objectDir("b", "x")
	require.NoError(t, err)
	require.NoError(t, os.Rename(filepath.Join(xs.root, domain.SystemBucket, "tmp/up"), filepath.Join(objDir, dataID)))
	require.NoError(t, os.MkdirAll(filepath.Join(objDir, orphan), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(objDir, orphan, "part.1"), []byte("old"), 0o600))
	meta := &xlMeta{}
	_, err = meta.AddVersion(versionFromFileInfo(fi))
	require.NoError(t, err)
	volDir, err := xs.volumeDir("b")
	require.NoError(t, err)
	require.NoError(t, xs.writeMeta(objDir, volDir, meta, false))

	// Naming the child prefix as the old data directory must not purge it.
	require.NoError(t, s.DeleteVersion(ctx, "b", "x", FileInfo{}, DeleteOptions{OldDataDir: childID}))
	_, err = s.ReadVersion(ctx, "b", child, "", ReadOptions{})
	require.NoError(t, err, "the child object survives the delete of its parent key")
	for _, gone := range []string{dataID, orphan} {
		_, err = os.Stat(filepath.Join(objDir, gone))
		assert.ErrorIs(t, err, fs.ErrNotExist, "data dir %s is purged", gone)
	}
	_, err = s.ReadVersion(ctx, "b", "x", "", ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
}

func TestIsDataDirOnDisk(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	tests := []struct {
		name  string
		dir   string
		setup func(t *testing.T, dir string)
		want  bool
		err   error
	}{
		{name: "parts only", dir: id, setup: func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "part.1"), nil, 0o600))
		}, want: true},
		{name: "empty", dir: id, want: true},
		{name: "not a uuid", dir: "sub", want: false},
		{name: "child object", dir: id, setup: func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, MetaFile), nil, 0o600))
		}, want: false},
		{name: "child prefix", dir: id, setup: func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "part.1"), nil, 0o600))
			require.NoError(t, os.Mkdir(filepath.Join(dir, "y"), 0o750))
		}, want: false},
		{name: "missing", dir: id, setup: func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(dir))
		}, want: false, err: fs.ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), tt.dir)
			require.NoError(t, os.Mkdir(dir, 0o750))
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			got, err := isDataDirOnDisk(dir)
			if tt.err != nil {
				assert.ErrorIs(t, err, tt.err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFilesAndRenameData(t *testing.T) {
	ctx := context.Background()
	s := newDrive(t)
	require.NoError(t, s.MakeVol(ctx, "b"))

	// CreateFile with known and unknown size, with a size mismatch.
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/u1/part.1", 5, strings.NewReader("hello")))
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/u1/part.2", -1, strings.NewReader("world!")))
	err := s.CreateFile(ctx, domain.SystemBucket, "tmp/u1/part.3", 10, strings.NewReader("short"))
	assert.Error(t, err)
	_, statErr := os.Stat(filepath.Join(drivePath(s), domain.SystemBucket, "tmp/u1/part.3"))
	assert.True(t, errors.Is(statErr, fs.ErrNotExist), "failed writes leave nothing behind")
	assert.ErrorIs(t, s.CreateFile(ctx, "nobucket", "x", 1, strings.NewReader("x")), domain.ErrBucketNotFound)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	assert.ErrorIs(t, s.CreateFile(canceled, domain.SystemBucket, "tmp/u1/part.4", -1, strings.NewReader("x")), context.Canceled)

	// Streams honor offset and length.
	rc, err := s.ReadFileStream(ctx, domain.SystemBucket, "tmp/u1/part.2", 1, 3)
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "orl", string(data))
	rc, err = s.ReadFileStream(ctx, domain.SystemBucket, "tmp/u1/part.2", 4, -1)
	require.NoError(t, err)
	data, _ = io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, "d!", string(data))
	_, err = s.ReadFileStream(ctx, domain.SystemBucket, "tmp/u1/part.2", 99, 1)
	assert.ErrorIs(t, err, domain.ErrInvalidRange)
	_, err = s.ReadFileStream(ctx, domain.SystemBucket, "tmp/u1/missing", 0, 1)
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)

	// Stat and glob.
	infos, err := s.StatInfoFile(ctx, domain.SystemBucket, "tmp/u1/part.*", true)
	require.NoError(t, err)
	require.Len(t, infos, 2)
	assert.Equal(t, "tmp/u1/part.1", infos[0].Name)
	assert.Equal(t, int64(5), infos[0].Size)
	assert.False(t, infos[0].Dir)
	infos, err = s.StatInfoFile(ctx, domain.SystemBucket, "tmp/u1", false)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.True(t, infos[0].Dir)
	_, err = s.StatInfoFile(ctx, domain.SystemBucket, "tmp/none", false)
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	infos, err = s.StatInfoFile(ctx, domain.SystemBucket, "tmp/none/*", true)
	require.NoError(t, err)
	assert.Empty(t, infos)

	// RenameFile assembles parts; RenameData commits the version.
	require.NoError(t, s.RenameFile(ctx, domain.SystemBucket, "tmp/u1/part.2", domain.SystemBucket, "tmp/final/part.2"))
	require.NoError(t, s.RenameFile(ctx, domain.SystemBucket, "tmp/u1/part.1", domain.SystemBucket, "tmp/final/part.1"))
	assert.ErrorIs(t, s.RenameFile(ctx, domain.SystemBucket, "tmp/u1/part.1", domain.SystemBucket, "tmp/final/part.9"), domain.ErrObjectNotFound)
	_, statErr = os.Stat(filepath.Join(drivePath(s), domain.SystemBucket, "tmp/u1"))
	assert.True(t, errors.Is(statErr, fs.ErrNotExist), "emptied source directory is pruned")

	now := time.Now().UTC().Truncate(time.Millisecond)
	fi := FileInfo{VersionID: "", ModTime: now, Size: 11, DataDir: "3f2b2b48-5c3a-4a1e-8a2a-123456789abc",
		Parts: []PartInfo{{Number: 1, Size: 5}, {Number: 2, Size: 6}}}
	res, err := s.RenameData(ctx, domain.SystemBucket, "tmp/final", fi, "b", "docs/readme", RenameOptions{})
	require.NoError(t, err)
	assert.Empty(t, res.OldDataDir)
	assert.NotEqual(t, [4]byte{}, res.Signature)
	got, err := s.ReadVersion(ctx, "b", "docs/readme", "", ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, fi.DataDir, got.DataDir)
	assert.Len(t, got.Parts, 2)
	rc, err = s.ReadFileStream(ctx, "b", "docs/readme/"+fi.DataDir+"/part.1", 0, -1)
	require.NoError(t, err)
	data, _ = io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, "hello", string(data))

	check, err := s.CheckParts(ctx, "b", "docs/readme", got)
	require.NoError(t, err)
	assert.Equal(t, []int{CheckPartSuccess, CheckPartSuccess}, check.Results)
	bad := got
	bad.Parts = []PartInfo{{Number: 1, Size: 99}, {Number: 7, Size: 1}}
	check, err = s.CheckParts(ctx, "b", "docs/readme", bad)
	require.NoError(t, err)
	assert.Equal(t, []int{CheckPartFileCorrupt, CheckPartFileNotFound}, check.Results)
	check, err = s.CheckParts(ctx, "b", "docs/none", bad)
	require.NoError(t, err)
	assert.Equal(t, []int{CheckPartFileNotFound, CheckPartFileNotFound}, check.Results)
	check, err = s.CheckParts(ctx, "nobucket", "docs/none", bad)
	require.NoError(t, err)
	assert.Equal(t, []int{CheckPartVolumeNotFound, CheckPartVolumeNotFound}, check.Results)

	// Replacing the null version reports the old data dir, purged on request.
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/u2/part.1", 3, strings.NewReader("new")))
	fi2 := FileInfo{ModTime: now.Add(time.Second), Size: 3, DataDir: "9d4e0c3a-1111-4222-8333-444455556666", Parts: []PartInfo{{Number: 1, Size: 3}}}
	res, err = s.RenameData(ctx, domain.SystemBucket, "tmp/u2", fi2, "b", "docs/readme", RenameOptions{})
	require.NoError(t, err)
	assert.Equal(t, fi.DataDir, res.OldDataDir)
	_, statErr = os.Stat(filepath.Join(drivePath(s), "b", "docs/readme", fi.DataDir))
	assert.NoError(t, statErr, "old data dir is left for the caller to purge")
	require.NoError(t, s.Delete(ctx, "b", "docs/readme/"+fi.DataDir, DeleteOptions{Recursive: true}))
	_, statErr = os.Stat(filepath.Join(drivePath(s), "b", "docs/readme", fi.DataDir))
	assert.True(t, errors.Is(statErr, fs.ErrNotExist))

	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/u3/part.1", 3, strings.NewReader("ver")))
	fi3 := FileInfo{ModTime: now.Add(2 * time.Second), Size: 3, DataDir: "0a0a0a0a-2222-4333-8444-555566667777", Parts: []PartInfo{{Number: 1, Size: 3}}}
	res, err = s.RenameData(ctx, domain.SystemBucket, "tmp/u3", fi3, "b", "docs/readme", RenameOptions{BaseOptions: DeleteOptions{Immediate: true}})
	require.NoError(t, err)
	assert.Empty(t, res.OldDataDir, "purged immediately")
	_, statErr = os.Stat(filepath.Join(drivePath(s), "b", "docs/readme", fi2.DataDir))
	assert.True(t, errors.Is(statErr, fs.ErrNotExist))

	// Deleting the version removes the data directory as well.
	got, err = s.ReadVersion(ctx, "b", "docs/readme", "", ReadOptions{})
	require.NoError(t, err)
	require.NoError(t, s.DeleteVersion(ctx, "b", "docs/readme", got, DeleteOptions{}))
	_, statErr = os.Stat(filepath.Join(drivePath(s), "b", "docs"))
	assert.True(t, errors.Is(statErr, fs.ErrNotExist), "empty parents are pruned")

	// Missing source data directory.
	_, err = s.RenameData(ctx, domain.SystemBucket, "tmp/none", fi3, "b", "x", RenameOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	// Metadata-only commit (inline data, no data dir).
	_, err = s.RenameData(ctx, domain.SystemBucket, "tmp/none", FileInfo{ModTime: now, Data: []byte("in")}, "b", "inline", RenameOptions{})
	require.NoError(t, err)
	got, err = s.ReadVersion(ctx, "b", "inline", "", ReadOptions{ReadData: true})
	require.NoError(t, err)
	assert.Equal(t, "in", string(got.Data))
}

func TestReadWriteAllAndDelete(t *testing.T) {
	ctx := context.Background()
	s := newDrive(t)
	require.NoError(t, s.MakeVol(ctx, "b"))

	_, err := s.ReadAll(ctx, domain.SystemBucket, FormatFile)
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	_, err = s.ReadAll(ctx, "nobucket", "x")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, "config/iam.json", []byte(`{}`)))
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, "config/iam.json", []byte(`{"v":2}`)))
	data, err := s.ReadAll(ctx, domain.SystemBucket, "config/iam.json")
	require.NoError(t, err)
	assert.Equal(t, `{"v":2}`, string(data))
	assert.ErrorIs(t, s.WriteAll(ctx, "nobucket", "x", nil), domain.ErrBucketNotFound)
	assert.ErrorIs(t, s.WriteAll(ctx, "b", "/abs", nil), domain.ErrInvalidObjectName)

	require.NoError(t, s.Delete(ctx, domain.SystemBucket, "config/iam.json", DeleteOptions{}))
	assert.ErrorIs(t, s.Delete(ctx, domain.SystemBucket, "config/iam.json", DeleteOptions{}), domain.ErrObjectNotFound)
	_, err = os.Stat(filepath.Join(drivePath(s), domain.SystemBucket, "config"))
	assert.NoError(t, err, "top-level system directories are never pruned")
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, "tmp/u9/part.1", []byte("x")))
	require.NoError(t, s.Delete(ctx, domain.SystemBucket, "tmp/u9", DeleteOptions{Recursive: true}))
	_, err = os.Stat(filepath.Join(drivePath(s), domain.SystemBucket, TmpDir))
	assert.NoError(t, err, "the tmp directory survives the removal of its last entry")
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, "multipart/h1/u1/upload.json", []byte("{}")))
	require.NoError(t, s.Delete(ctx, domain.SystemBucket, "multipart/h1/u1", DeleteOptions{Recursive: true}))
	_, err = os.Stat(filepath.Join(drivePath(s), domain.SystemBucket, MultipartDir, "h1"))
	assert.True(t, errors.Is(err, fs.ErrNotExist), "intermediate directories are pruned")

	require.NoError(t, s.WriteAll(ctx, "b", "tree/a/1", []byte("1")))
	require.NoError(t, s.WriteAll(ctx, "b", "tree/b/2", []byte("2")))
	assert.ErrorIs(t, s.Delete(ctx, "b", "tree/none", DeleteOptions{Recursive: true}), domain.ErrObjectNotFound)
	require.NoError(t, s.Delete(ctx, "b", "tree", DeleteOptions{Recursive: true}))
	_, err = os.Stat(filepath.Join(drivePath(s), "b", "tree"))
	assert.True(t, errors.Is(err, fs.ErrNotExist))
	// The volume directory itself is never pruned.
	_, err = s.StatVol(ctx, "b")
	require.NoError(t, err)
}

func TestWalkDir(t *testing.T) {
	ctx := context.Background()
	s := newDrive(t)
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()
	for _, k := range []string{"a-b", "a.txt", "a/x", "a/y/z", "b.txt", "photos/", "photos/1.jpg", "photos/2024/a.jpg", "zeta"} {
		require.NoError(t, s.WriteMetadata(ctx, "b", k, version("", now, 1)), k)
	}
	// A stray file at the volume root is ignored.
	require.NoError(t, os.WriteFile(filepath.Join(drivePath(s), "b", "stray"), []byte("x"), 0o600))

	collect := func(opts WalkDirOptions, fn func(e WalkEntry) error) ([]string, error) {
		var out []string
		err := s.WalkDir(ctx, opts, func(e WalkEntry) error {
			name := e.Name
			if e.IsDir {
				name = "D:" + name
			} else {
				require.NotEmpty(t, e.Metadata, name)
				fis, err := DecodeFileInfos("b", e.Name, e.Metadata)
				require.NoError(t, err)
				require.Len(t, fis, 1)
				assert.Equal(t, e.Name, fis[0].Name)
			}
			out = append(out, name)
			if fn != nil {
				return fn(e)
			}
			return nil
		})
		return out, err
	}

	// Non-recursive: S3 key order ("a-b" < "a.txt" < "a/"), the directory
	// object "photos/" before the prefix of the same name.
	got, err := collect(WalkDirOptions{Bucket: "b"}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b", "a.txt", "D:a/", "b.txt", "photos/", "D:photos/", "zeta"}, got)

	// Recursive.
	got, err = collect(WalkDirOptions{Bucket: "b", Recursive: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b", "a.txt", "D:a/", "a/x", "D:a/y/", "a/y/z", "b.txt", "photos/", "D:photos/", "photos/1.jpg", "D:photos/2024/", "photos/2024/a.jpg", "zeta"}, got)

	// SkipDir skips the content of a prefix.
	got, err = collect(WalkDirOptions{Bucket: "b", Recursive: true}, func(e WalkEntry) error {
		if e.IsDir && e.Name == "photos/" {
			return fs.SkipDir
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b", "a.txt", "D:a/", "a/x", "D:a/y/", "a/y/z", "b.txt", "photos/", "D:photos/", "zeta"}, got)

	// BaseDir + FilterPrefix + ForwardTo + Limit.
	got, err = collect(WalkDirOptions{Bucket: "b", BaseDir: "photos", Recursive: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"photos/1.jpg", "D:photos/2024/", "photos/2024/a.jpg"}, got)
	got, err = collect(WalkDirOptions{Bucket: "b", FilterPrefix: "a", Recursive: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b", "a.txt", "D:a/", "a/x", "D:a/y/", "a/y/z"}, got)
	got, err = collect(WalkDirOptions{Bucket: "b", FilterPrefix: "a/y/", Recursive: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"D:a/", "D:a/y/", "a/y/z"}, got)
	got, err = collect(WalkDirOptions{Bucket: "b", Recursive: true, ForwardTo: "b"}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"b.txt", "photos/", "D:photos/", "photos/1.jpg", "D:photos/2024/", "photos/2024/a.jpg", "zeta"}, got)
	got, err = collect(WalkDirOptions{Bucket: "b", Recursive: true, ForwardTo: "photos/2"}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"D:photos/", "D:photos/2024/", "photos/2024/a.jpg", "zeta"}, got)
	got, err = collect(WalkDirOptions{Bucket: "b", Recursive: true, Limit: 3}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b", "a.txt", "D:a/", "a/x"}, got, "prefixes do not count toward the limit")

	// Errors from the callback propagate; missing base dirs are reported on request.
	boom := errors.New("boom")
	_, err = collect(WalkDirOptions{Bucket: "b"}, func(WalkEntry) error { return boom })
	assert.ErrorIs(t, err, boom)
	got, err = collect(WalkDirOptions{Bucket: "b", BaseDir: "nothing"}, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
	_, err = collect(WalkDirOptions{Bucket: "b", BaseDir: "nothing", ReportNotFound: true}, nil)
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	_, err = collect(WalkDirOptions{Bucket: "nobucket"}, nil)
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	assert.ErrorIs(t, s.WalkDir(ctx, WalkDirOptions{Bucket: "b"}, nil), domain.ErrInvalidArgument)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	assert.ErrorIs(t, s.WalkDir(canceled, WalkDirOptions{Bucket: "b"}, func(WalkEntry) error { return nil }), context.Canceled)
}

func TestMapErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "not exist", err: fs.ErrNotExist, want: domain.ErrObjectNotFound},
		{name: "not a directory", err: syscall.ENOTDIR, want: domain.ErrObjectNotFound},
		{name: "no space", err: syscall.ENOSPC, want: domain.ErrStorageFull},
		{name: "quota", err: syscall.EDQUOT, want: domain.ErrStorageFull},
		{name: "i/o error", err: syscall.EIO, want: domain.ErrFaultyDisk},
		{name: "read-only", err: syscall.EROFS, want: domain.ErrFaultyDisk},
		{name: "access", err: syscall.EACCES, want: domain.ErrFaultyDisk},
		{name: "permission", err: fs.ErrPermission, want: domain.ErrFaultyDisk},
		{name: "name too long", err: syscall.ENAMETOOLONG, want: domain.ErrInvalidObjectName},
		{name: "symlink loop", err: syscall.ELOOP, want: domain.ErrAccessDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := &fs.PathError{Op: "open", Path: "/x", Err: tt.err}
			got := mapErr(wrapped)
			assert.ErrorIs(t, got, tt.want)
			assert.ErrorIs(t, got, tt.err, "the original error stays in the chain")
		})
	}
	assert.NoError(t, mapErr(nil))
	plain := errors.New("plain")
	assert.Equal(t, plain, mapErr(plain))
	already := fmt.Errorf("%w: x", domain.ErrBucketNotFound)
	assert.Equal(t, already, mapErr(already), "domain errors are kept as they are")
	assert.Equal(t, []int{2, 2, 2}, fill(3, 2))
	assert.Equal(t, "part.7", partName(7))
	seg, dir := keyFromDirName("photos" + xlDirSuffix)
	assert.Equal(t, "photos/", seg)
	assert.True(t, dir)
	assert.True(t, isDataDir("3f2b2b48-5c3a-4a1e-8a2a-123456789abc"))
	assert.False(t, isDataDir("sub"))
	assert.False(t, isDataDir("{3f2b2b48-5c3a-4a1e-8a2a-123456789abc}"))
}

// TestFaultyDrive makes the drive unwritable; root bypasses permissions.
func TestFaultyDrive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	ctx := context.Background()
	s := newDrive(t)
	require.NoError(t, s.MakeVol(ctx, "b"))
	volDir := filepath.Join(drivePath(s), "b")
	require.NoError(t, os.Chmod(volDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(volDir, 0o750) })
	err := s.WriteMetadata(ctx, "b", "k", version("", time.Now(), 1))
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)
}

func TestCreateFileLargeStream(t *testing.T) {
	ctx := context.Background()
	s := newDrive(t)
	payload := bytes.Repeat([]byte("0123456789"), 100_000) // 1 MB
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/big/part.1", int64(len(payload)), bytes.NewReader(payload)))
	rc, err := s.ReadFileStream(ctx, domain.SystemBucket, "tmp/big/part.1", 999_990, 10)
	require.NoError(t, err)
	tail, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	assert.Equal(t, "0123456789", string(tail))
}

// drivePath returns the root directory of a local drive under test.
func drivePath(s StorageAPI) string { return s.(*xlStorage).root }

func TestOrderStripes(t *testing.T) {
	tests := []struct {
		name string
		in   []stripeLock
		want []stripeLock
	}{
		{name: "single", in: []stripeLock{{idx: 3, write: true}}, want: []stripeLock{{idx: 3, write: true}}},
		{name: "sorted by index", in: []stripeLock{{idx: 9, write: true}, {idx: 2}, {idx: 5}},
			want: []stripeLock{{idx: 2}, {idx: 5}, {idx: 9, write: true}}},
		{name: "write absorbs read", in: []stripeLock{{idx: 4}, {idx: 4, write: true}, {idx: 1}},
			want: []stripeLock{{idx: 1}, {idx: 4, write: true}}},
		{name: "duplicate reads merge", in: []stripeLock{{idx: 7, write: true}, {idx: 4}, {idx: 4}},
			want: []stripeLock{{idx: 4}, {idx: 7, write: true}}},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, orderStripes(tt.in), tt.name)
	}
}

// The placement locks of a key include its ancestors: a writer of the
// parent waits for a writer of the child.
func TestLockPlacementExcludesAncestorWriters(t *testing.T) {
	s, ok := newDrive(t).(*xlStorage)
	require.True(t, ok)
	unlock := s.lockPlacement("b", "k/x/y")
	acquired := make(chan struct{})
	go func() {
		release := s.lockPlacement("b", "k")
		close(acquired)
		release()
	}()
	select {
	case <-acquired:
		t.Fatal("the parent writer did not wait for the child writer")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("the parent writer never acquired the lock")
	}
}
