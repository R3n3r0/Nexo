package storagetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

func testDiskInfo(t *testing.T, f Factory) {
	ctx := context.Background()
	s, _ := open(t, f)
	assert.True(t, s.IsOnline())
	assert.Nil(t, s.Healing())

	info, err := s.DiskInfo(ctx, storage.DiskInfoOptions{})
	require.NoError(t, err)
	assert.Equal(t, domain.StateOnline, info.State)
	assert.Empty(t, info.ID, "unformatted drive has no id")
	assert.NotZero(t, info.Total)
	assert.NotEmpty(t, info.FSType)
	cached, err := s.DiskInfo(ctx, storage.DiskInfoOptions{NoOp: true})
	require.NoError(t, err)
	assert.Equal(t, info.Total, cached.Total, "NoOp returns the figures of the previous call")
	assert.Equal(t, info.FSType, cached.FSType)
	_, err = s.DiskInfo(ctx, storage.DiskInfoOptions{DiskID: "other"})
	assert.ErrorIs(t, err, domain.ErrDiskNotFound)

	// format.json gives the drive its id.
	doc := &topology.FormatV1{Version: topology.FormatVersion, ID: "deployment", Mode: topology.FormatModeErasure,
		Erasure: topology.FormatErasure{This: "disk-1", Sets: [][]string{{"disk-1", "disk-2"}}, DistributionAlgo: topology.DistributionAlgoV1}}
	raw, err := doc.Marshal()
	require.NoError(t, err)
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, storage.FormatFile, raw))
	info, err = s.DiskInfo(ctx, storage.DiskInfoOptions{DiskID: "disk-1"})
	require.NoError(t, err)
	assert.Equal(t, "disk-1", info.ID)

	// Close makes the drive offline and refuses operations.
	require.NoError(t, s.Close(ctx))
	assert.False(t, s.IsOnline())
	_, err = s.ListVols(ctx)
	assert.ErrorIs(t, err, domain.ErrDiskNotFound)
}

func testVolumes(t *testing.T, f Factory) {
	ctx := context.Background()
	s, _ := open(t, f)

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

func testMetadataLifecycle(t *testing.T, f Factory) {
	ctx := context.Background()
	s, dir := open(t, f)
	require.NoError(t, s.MakeVol(ctx, "b"))
	t0 := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)

	_, err := s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	_, err = s.ReadVersion(ctx, "nobucket", "k", "", storage.ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	assert.ErrorIs(t, s.WriteMetadata(ctx, "nobucket", "k", version("", t0, 1)), domain.ErrBucketNotFound)

	// Null version with inline data.
	v0 := version("", t0, 5)
	v0.Data = []byte("hello")
	require.NoError(t, s.WriteMetadata(ctx, "b", "k", v0))
	got, err := s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{ReadData: true})
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), got.Data)
	assert.True(t, got.IsLatest)
	assert.Equal(t, 1, got.NumVersions)
	assert.Equal(t, "b", got.Volume)
	assert.Equal(t, "k", got.Name)
	assert.Equal(t, "text/plain", got.Metadata["content-type"])
	assert.Equal(t, t0, got.ModTime)
	noData, err := s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{})
	require.NoError(t, err)
	assert.Nil(t, noData.Data)

	// Rewriting the null version replaces it.
	v0b := version("", t0.Add(time.Minute), 6)
	require.NoError(t, s.WriteMetadata(ctx, "b", "k", v0b))
	got, err = s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(6), got.Size)
	assert.Equal(t, 1, got.NumVersions)

	// Versioned writes accumulate, newest first.
	require.NoError(t, s.WriteMetadata(ctx, "b", "k", version("v1", t0.Add(2*time.Minute), 7)))
	require.NoError(t, s.WriteMetadata(ctx, "b", "k", version("v2", t0.Add(3*time.Minute), 8)))
	got, err = s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, "v2", got.VersionID)
	assert.Equal(t, 3, got.NumVersions)
	old, err := s.ReadVersion(ctx, "b", "k", "v1", storage.ReadOptions{})
	require.NoError(t, err)
	assert.False(t, old.IsLatest)
	assert.Equal(t, 1, old.Idx)
	assert.Equal(t, got.ModTime, old.SuccessorModTime)
	_, err = s.ReadVersion(ctx, "b", "k", "nope", storage.ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrNoSuchVersion)

	// UpdateMetadata changes a version in place and keeps its data.
	old.Metadata["x-amz-tagging"] = "a=1"
	require.NoError(t, s.UpdateMetadata(ctx, "b", "k", old, storage.UpdateMetadataOptions{}))
	old2, err := s.ReadVersion(ctx, "b", "k", "v1", storage.ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, "a=1", old2.Metadata["x-amz-tagging"])
	_, err = s.ReadVersion(ctx, "b", "inl", "", storage.ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	withData := version("", t0, 2)
	withData.Data = []byte("hi")
	require.NoError(t, s.WriteMetadata(ctx, "b", "inl", withData))
	inline, err := s.ReadVersion(ctx, "b", "inl", "", storage.ReadOptions{})
	require.NoError(t, err)
	inline.Metadata["x"] = "y"
	require.NoError(t, s.UpdateMetadata(ctx, "b", "inl", inline, storage.UpdateMetadataOptions{}))
	inline, err = s.ReadVersion(ctx, "b", "inl", "", storage.ReadOptions{ReadData: true})
	require.NoError(t, err)
	assert.Equal(t, "hi", string(inline.Data), "a metadata update without data keeps the inline content")
	assert.Equal(t, "y", inline.Metadata["x"])
	assert.ErrorIs(t, s.UpdateMetadata(ctx, "b", "k", version("missing", t0, 1), storage.UpdateMetadataOptions{NoPersistence: true}), domain.ErrNoSuchVersion)
	assert.ErrorIs(t, s.UpdateMetadata(ctx, "b", "nokey", old, storage.UpdateMetadataOptions{}), domain.ErrObjectNotFound)

	// Delete marker becomes the latest version.
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", storage.FileInfo{VersionID: "dm", ModTime: t0.Add(4 * time.Minute), MarkDeleted: true}, storage.DeleteOptions{}))
	got, err = s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{})
	require.NoError(t, err)
	assert.True(t, got.Deleted)
	assert.Equal(t, "dm", got.VersionID)
	assert.Equal(t, 4, got.NumVersions)

	// Removing versions one by one, including the null version.
	assert.ErrorIs(t, s.DeleteVersion(ctx, "b", "k", storage.FileInfo{VersionID: "ghost"}, storage.DeleteOptions{}), domain.ErrNoSuchVersion)
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", storage.FileInfo{VersionID: "dm"}, storage.DeleteOptions{}))
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", storage.FileInfo{VersionID: ""}, storage.DeleteOptions{}))
	got, err = s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, got.NumVersions)
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", storage.FileInfo{VersionID: "v2"}, storage.DeleteOptions{}))
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", storage.FileInfo{VersionID: "v1"}, storage.DeleteOptions{}))
	_, err = s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "last version removed → object gone")
	_, err = os.Stat(filepath.Join(dir, "b", "k"))
	assert.True(t, errors.Is(err, fs.ErrNotExist), "empty object directory is pruned")

	// A delete marker on a missing object is recorded (S3 semantics).
	require.NoError(t, s.DeleteVersion(ctx, "b", "new", storage.FileInfo{VersionID: "m1", ModTime: t0, MarkDeleted: true}, storage.DeleteOptions{}))
	got, err = s.ReadVersion(ctx, "b", "new", "", storage.ReadOptions{})
	require.NoError(t, err)
	assert.True(t, got.Deleted)
	assert.ErrorIs(t, s.DeleteVersion(ctx, "b", "absent", storage.FileInfo{VersionID: "x"}, storage.DeleteOptions{}), domain.ErrObjectNotFound)

	// Directory objects ("photos/") do not collide with the prefix.
	require.NoError(t, s.WriteMetadata(ctx, "b", "photos/", version("", t0, 0)))
	require.NoError(t, s.WriteMetadata(ctx, "b", "photos/a.jpg", version("", t0, 3)))
	_, err = s.ReadVersion(ctx, "b", "photos/", "", storage.ReadOptions{})
	require.NoError(t, err)
	_, err = s.ReadVersion(ctx, "b", "photos", "", storage.ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)

	// Invalid paths.
	assert.ErrorIs(t, s.WriteMetadata(ctx, "b", "../x", version("", t0, 0)), domain.ErrInvalidObjectName)
	assert.ErrorIs(t, s.WriteMetadata(ctx, "b", "", version("", t0, 0)), domain.ErrInvalidObjectName)
	assert.ErrorIs(t, s.WriteMetadata(ctx, "b", "a//b", version("", t0, 0)), domain.ErrInvalidObjectName)
}

func testObjectPlacementConflicts(t *testing.T, f Factory) {
	ctx := context.Background()
	s, _ := open(t, f)
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
	_, err = s.RenameData(ctx, domain.SystemBucket, "tmp/up1", fi, "b", "dir/file", storage.RenameOptions{})
	require.NoError(t, err)
	require.NoError(t, s.WriteMetadata(ctx, "b", "dir/file", version("", now.Add(time.Second), 4)))
}

// testObjectPlacementUUIDPrefix is a regression test: a child prefix whose
// name is a UUID ("x/<uuid>/y") is a prefix, not a data directory of "x".
// Writing "x" over it must be refused.
func testObjectPlacementUUIDPrefix(t *testing.T, f Factory) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	ctx := context.Background()
	now := time.Now()
	for _, child := range []string{"x/" + id + "/y", "x/" + id + "/deep/y", "x/" + id + "/"} {
		t.Run(child, func(t *testing.T) {
			s, _ := open(t, f)
			require.NoError(t, s.MakeVol(ctx, "b"))
			require.NoError(t, s.WriteMetadata(ctx, "b", child, version("", now, 1)))
			err := s.WriteMetadata(ctx, "b", "x", version("", now, 1))
			var s3err *domain.S3Error
			require.ErrorAs(t, err, &s3err)
			assert.Equal(t, "XNexoObjectExistsAsDirectory", s3err.Code)
			// A delete marker cannot create "x" either.
			err = s.DeleteVersion(ctx, "b", "x", storage.FileInfo{VersionID: "11111111-1111-1111-1111-111111111111", ModTime: now, MarkDeleted: true}, storage.DeleteOptions{})
			require.ErrorAs(t, err, &s3err)
			assert.Equal(t, "XNexoObjectExistsAsDirectory", s3err.Code)
			_, err = s.ReadVersion(ctx, "b", child, "", storage.ReadOptions{})
			require.NoError(t, err)
		})
	}
}

func testFilesAndRenameData(t *testing.T, f Factory) {
	ctx := context.Background()
	s, dir := open(t, f)
	require.NoError(t, s.MakeVol(ctx, "b"))

	// CreateFile with known and unknown size, with a size mismatch.
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/u1/part.1", 5, strings.NewReader("hello")))
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/u1/part.2", -1, strings.NewReader("world!")))
	err := s.CreateFile(ctx, domain.SystemBucket, "tmp/u1/part.3", 10, strings.NewReader("short"))
	assert.Error(t, err)
	_, statErr := os.Stat(filepath.Join(dir, domain.SystemBucket, "tmp/u1/part.3"))
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
	_, statErr = os.Stat(filepath.Join(dir, domain.SystemBucket, "tmp/u1"))
	assert.True(t, errors.Is(statErr, fs.ErrNotExist), "emptied source directory is pruned")

	now := time.Now().UTC().Truncate(time.Millisecond)
	fi := storage.FileInfo{VersionID: "", ModTime: now, Size: 11, DataDir: "3f2b2b48-5c3a-4a1e-8a2a-123456789abc",
		Parts: []storage.PartInfo{{Number: 1, Size: 5}, {Number: 2, Size: 6}}}
	res, err := s.RenameData(ctx, domain.SystemBucket, "tmp/final", fi, "b", "docs/readme", storage.RenameOptions{})
	require.NoError(t, err)
	assert.Empty(t, res.OldDataDir)
	assert.NotEqual(t, [4]byte{}, res.Signature)
	got, err := s.ReadVersion(ctx, "b", "docs/readme", "", storage.ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, fi.DataDir, got.DataDir)
	assert.Equal(t, now, got.ModTime)
	assert.Len(t, got.Parts, 2)
	rc, err = s.ReadFileStream(ctx, "b", "docs/readme/"+fi.DataDir+"/part.1", 0, -1)
	require.NoError(t, err)
	data, _ = io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, "hello", string(data))

	check, err := s.CheckParts(ctx, "b", "docs/readme", got)
	require.NoError(t, err)
	assert.Equal(t, []int{storage.CheckPartSuccess, storage.CheckPartSuccess}, check.Results)
	bad := got
	bad.Parts = []storage.PartInfo{{Number: 1, Size: 99}, {Number: 7, Size: 1}}
	check, err = s.CheckParts(ctx, "b", "docs/readme", bad)
	require.NoError(t, err)
	assert.Equal(t, []int{storage.CheckPartFileCorrupt, storage.CheckPartFileNotFound}, check.Results)
	check, err = s.CheckParts(ctx, "b", "docs/none", bad)
	require.NoError(t, err)
	assert.Equal(t, []int{storage.CheckPartFileNotFound, storage.CheckPartFileNotFound}, check.Results)
	check, err = s.CheckParts(ctx, "nobucket", "docs/none", bad)
	require.NoError(t, err)
	assert.Equal(t, []int{storage.CheckPartVolumeNotFound, storage.CheckPartVolumeNotFound}, check.Results)

	// Replacing the null version reports the old data dir, purged on request.
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/u2/part.1", 3, strings.NewReader("new")))
	fi2 := storage.FileInfo{ModTime: now.Add(time.Second), Size: 3, DataDir: "9d4e0c3a-1111-4222-8333-444455556666", Parts: []storage.PartInfo{{Number: 1, Size: 3}}}
	res, err = s.RenameData(ctx, domain.SystemBucket, "tmp/u2", fi2, "b", "docs/readme", storage.RenameOptions{})
	require.NoError(t, err)
	assert.Equal(t, fi.DataDir, res.OldDataDir)
	_, statErr = os.Stat(filepath.Join(dir, "b", "docs/readme", fi.DataDir))
	assert.NoError(t, statErr, "old data dir is left for the caller to purge")
	require.NoError(t, s.Delete(ctx, "b", "docs/readme/"+fi.DataDir, storage.DeleteOptions{Recursive: true}))
	_, statErr = os.Stat(filepath.Join(dir, "b", "docs/readme", fi.DataDir))
	assert.True(t, errors.Is(statErr, fs.ErrNotExist))

	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/u3/part.1", 3, strings.NewReader("ver")))
	fi3 := storage.FileInfo{ModTime: now.Add(2 * time.Second), Size: 3, DataDir: "0a0a0a0a-2222-4333-8444-555566667777", Parts: []storage.PartInfo{{Number: 1, Size: 3}}}
	res, err = s.RenameData(ctx, domain.SystemBucket, "tmp/u3", fi3, "b", "docs/readme", storage.RenameOptions{BaseOptions: storage.DeleteOptions{Immediate: true}})
	require.NoError(t, err)
	assert.Empty(t, res.OldDataDir, "purged immediately")
	_, statErr = os.Stat(filepath.Join(dir, "b", "docs/readme", fi2.DataDir))
	assert.True(t, errors.Is(statErr, fs.ErrNotExist))

	// Deleting the version removes the data directory as well.
	got, err = s.ReadVersion(ctx, "b", "docs/readme", "", storage.ReadOptions{})
	require.NoError(t, err)
	require.NoError(t, s.DeleteVersion(ctx, "b", "docs/readme", got, storage.DeleteOptions{}))
	_, statErr = os.Stat(filepath.Join(dir, "b", "docs"))
	assert.True(t, errors.Is(statErr, fs.ErrNotExist), "empty parents are pruned")

	// Missing source data directory.
	_, err = s.RenameData(ctx, domain.SystemBucket, "tmp/none", fi3, "b", "x", storage.RenameOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	// Metadata-only commit (inline data, no data dir).
	_, err = s.RenameData(ctx, domain.SystemBucket, "tmp/none", storage.FileInfo{ModTime: now, Data: []byte("in")}, "b", "inline", storage.RenameOptions{})
	require.NoError(t, err)
	got, err = s.ReadVersion(ctx, "b", "inline", "", storage.ReadOptions{ReadData: true})
	require.NoError(t, err)
	assert.Equal(t, "in", string(got.Data))
}

func testReadWriteAllAndDelete(t *testing.T, f Factory) {
	ctx := context.Background()
	s, dir := open(t, f)
	require.NoError(t, s.MakeVol(ctx, "b"))

	_, err := s.ReadAll(ctx, domain.SystemBucket, storage.FormatFile)
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
	require.NoError(t, s.WriteAll(ctx, "b", "empty", nil))
	data, err = s.ReadAll(ctx, "b", "empty")
	require.NoError(t, err)
	assert.Empty(t, data)

	require.NoError(t, s.Delete(ctx, domain.SystemBucket, "config/iam.json", storage.DeleteOptions{}))
	assert.ErrorIs(t, s.Delete(ctx, domain.SystemBucket, "config/iam.json", storage.DeleteOptions{}), domain.ErrObjectNotFound)
	_, err = os.Stat(filepath.Join(dir, domain.SystemBucket, "config"))
	assert.NoError(t, err, "top-level system directories are never pruned")
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, "tmp/u9/part.1", []byte("x")))
	require.NoError(t, s.Delete(ctx, domain.SystemBucket, "tmp/u9", storage.DeleteOptions{Recursive: true}))
	_, err = os.Stat(filepath.Join(dir, domain.SystemBucket, storage.TmpDir))
	assert.NoError(t, err, "the tmp directory survives the removal of its last entry")
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, "multipart/h1/u1/upload.json", []byte("{}")))
	require.NoError(t, s.Delete(ctx, domain.SystemBucket, "multipart/h1/u1", storage.DeleteOptions{Recursive: true}))
	_, err = os.Stat(filepath.Join(dir, domain.SystemBucket, storage.MultipartDir, "h1"))
	assert.True(t, errors.Is(err, fs.ErrNotExist), "intermediate directories are pruned")

	require.NoError(t, s.WriteAll(ctx, "b", "tree/a/1", []byte("1")))
	require.NoError(t, s.WriteAll(ctx, "b", "tree/b/2", []byte("2")))
	assert.ErrorIs(t, s.Delete(ctx, "b", "tree/none", storage.DeleteOptions{Recursive: true}), domain.ErrObjectNotFound)
	require.NoError(t, s.Delete(ctx, "b", "tree", storage.DeleteOptions{Recursive: true}))
	_, err = os.Stat(filepath.Join(dir, "b", "tree"))
	assert.True(t, errors.Is(err, fs.ErrNotExist))
	// The volume directory itself is never pruned.
	_, err = s.StatVol(ctx, "b")
	require.NoError(t, err)
}

// testFaultyDrive makes the drive unwritable; root bypasses permissions.
func testFaultyDrive(t *testing.T, f Factory) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	ctx := context.Background()
	s, dir := open(t, f)
	require.NoError(t, s.MakeVol(ctx, "b"))
	volDir := filepath.Join(dir, "b")
	require.NoError(t, os.Chmod(volDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(volDir, 0o750) })
	err := s.WriteMetadata(ctx, "b", "k", version("", time.Now(), 1))
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)
}

func testCreateFileLargeStream(t *testing.T, f Factory) {
	ctx := context.Background()
	s, _ := open(t, f)
	payload := bytes.Repeat([]byte("0123456789"), 100_000) // 1 MB
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/big/part.1", int64(len(payload)), bytes.NewReader(payload)))
	rc, err := s.ReadFileStream(ctx, domain.SystemBucket, "tmp/big/part.1", 999_990, 10)
	require.NoError(t, err)
	tail, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	assert.Equal(t, "0123456789", string(tail))

	// A reader closed early releases the stream.
	rc, err = s.ReadFileStream(ctx, domain.SystemBucket, "tmp/big/part.1", 0, -1)
	require.NoError(t, err)
	head := make([]byte, 10)
	_, err = io.ReadFull(rc, head)
	require.NoError(t, err)
	assert.Equal(t, "0123456789", string(head))
	require.NoError(t, rc.Close())
	all, err := s.ReadAll(ctx, domain.SystemBucket, "tmp/big/part.1")
	require.NoError(t, err)
	assert.Equal(t, payload, all)
}
