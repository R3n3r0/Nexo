package storagetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// testDriveOffline removes the drive directory under a running instance
// (unmounted or failed drive): every operation reports ErrDiskNotFound, the
// drive is reported offline and nothing recreates the directory on the
// parent file system.
func testDriveOffline(t *testing.T, f Factory) {
	ctx := context.Background()
	s, dir := open(t, f)
	require.NoError(t, s.MakeVol(ctx, "b"))
	fi, err := putObject(ctx, s, "b", "k", "", time.Now(), []byte("data"))
	require.NoError(t, err)
	require.True(t, s.IsOnline())

	require.NoError(t, os.RemoveAll(dir))
	noop := func(storage.WalkEntry) error { return nil }
	ops := map[string]func() error{
		"DiskInfo":    func() error { _, err := s.DiskInfo(ctx, storage.DiskInfoOptions{}); return err },
		"MakeVol":     func() error { return s.MakeVol(ctx, "c") },
		"ListVols":    func() error { _, err := s.ListVols(ctx); return err },
		"StatVol":     func() error { _, err := s.StatVol(ctx, "b"); return err },
		"DeleteVol":   func() error { return s.DeleteVol(ctx, "b", true) },
		"ReadVersion": func() error { _, err := s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{}); return err },
		"WriteMeta":   func() error { return s.WriteMetadata(ctx, "b", "k2", fi) },
		"UpdateMeta":  func() error { return s.UpdateMetadata(ctx, "b", "k", fi, storage.UpdateMetadataOptions{}) },
		"DeleteVer":   func() error { return s.DeleteVersion(ctx, "b", "k", fi, storage.DeleteOptions{}) },
		"CreateFile":  func() error { return s.CreateFile(ctx, "b", "f", 1, strings.NewReader("x")) },
		"ReadStream":  func() error { _, err := s.ReadFileStream(ctx, "b", "f", 0, 1); return err },
		"RenameData": func() error {
			_, err := s.RenameData(ctx, domain.SystemBucket, "tmp/x", fi, "b", "k", storage.RenameOptions{})
			return err
		},
		"RenameFile":   func() error { return s.RenameFile(ctx, domain.SystemBucket, "tmp/a", "b", "x") },
		"WalkDir":      func() error { return s.WalkDir(ctx, storage.WalkDirOptions{Bucket: "b"}, noop) },
		"ReadAll":      func() error { _, err := s.ReadAll(ctx, domain.SystemBucket, storage.FormatFile); return err },
		"WriteAll":     func() error { return s.WriteAll(ctx, domain.SystemBucket, "config/x", []byte("x")) },
		"Delete":       func() error { return s.Delete(ctx, "b", "k", storage.DeleteOptions{Recursive: true}) },
		"CheckParts":   func() error { _, err := s.CheckParts(ctx, "b", "k", fi); return err },
		"StatInfoFile": func() error { _, err := s.StatInfoFile(ctx, "b", "k", false); return err },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, op(), domain.ErrDiskNotFound)
		})
	}
	assert.False(t, s.IsOnline())
	_, err = os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "the drive directory is never recreated")
}

// testSymlinksAreNotFollowed plants links pointing outside the drive.
func testSymlinksAreNotFollowed(t *testing.T, f Factory) {
	ctx := context.Background()
	base := t.TempDir()
	dir := filepath.Join(base, "drive")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "k"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600))
	s := f(t, dir)
	require.NoError(t, s.MakeVol(ctx, "b"))
	_, err := putObject(ctx, s, "b", "obj", "", time.Now(), []byte("data"))
	require.NoError(t, err)

	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "b", "link")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "vol")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "b", "file")))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "b", "metalink"), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "b", "metalink", storage.MetaFile)))

	denied := map[string]func() error{
		"read through dir link":  func() error { _, err := s.ReadAll(ctx, "b", "link/secret"); return err },
		"write through dir link": func() error { return s.WriteAll(ctx, "b", "link/new", []byte("x")) },
		"object below link":      func() error { _, err := s.ReadVersion(ctx, "b", "link/k", "", storage.ReadOptions{}); return err },
		"put below link":         func() error { _, err := putObject(ctx, s, "b", "link/k2", "", time.Now(), []byte("x")); return err },
		"linked volume":          func() error { _, err := s.StatVol(ctx, "vol"); return err },
		"walk linked volume": func() error {
			return s.WalkDir(ctx, storage.WalkDirOptions{Bucket: "vol"}, func(storage.WalkEntry) error { return nil })
		},
		"read file link":   func() error { _, err := s.ReadAll(ctx, "b", "file"); return err },
		"stream file link": func() error { _, err := s.ReadFileStream(ctx, "b", "file", 0, -1); return err },
		"linked xl.meta":   func() error { _, err := s.ReadVersion(ctx, "b", "metalink", "", storage.ReadOptions{}); return err },
		"linked xl.meta data": func() error {
			_, err := s.ReadVersion(ctx, "b", "metalink", "", storage.ReadOptions{ReadData: true})
			return err
		},
		"stat link":           func() error { _, err := s.StatInfoFile(ctx, "b", "file", false); return err },
		"delete through link": func() error { return s.Delete(ctx, "b", "link/secret", storage.DeleteOptions{}) },
	}
	for name, op := range denied {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, op(), domain.ErrAccessDenied)
		})
	}
	vols, err := s.ListVols(ctx)
	require.NoError(t, err)
	require.Len(t, vols, 1, "linked volumes are not listed")
	infos, err := s.StatInfoFile(ctx, "b", "*", true)
	require.NoError(t, err)
	var globbed []string
	for _, info := range infos {
		globbed = append(globbed, info.Name)
	}
	assert.ElementsMatch(t, []string{"obj", "metalink"}, globbed, "globs skip links")
	data, err := os.ReadFile(filepath.Join(outside, "secret")) // #nosec G304 -- a file of the test's temporary directory
	require.NoError(t, err)
	assert.Equal(t, "secret", string(data))
	_, err = os.Stat(filepath.Join(outside, "new"))
	assert.True(t, os.IsNotExist(err), "nothing is written outside the drive")
}

func testHealingTracker(t *testing.T, f Factory) {
	ctx := context.Background()
	s, dir := open(t, f)
	assert.Nil(t, s.Healing())

	tracker := storage.HealingTracker{ID: "disk-1", Endpoint: dir, Started: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), ObjectsHealed: 3}
	raw, err := json.Marshal(tracker)
	require.NoError(t, err)
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, storage.HealingFile, raw))
	got := s.Healing()
	require.NotNil(t, got)
	assert.Equal(t, tracker, *got)
	got.ObjectsHealed = 99
	assert.Equal(t, uint64(3), s.Healing().ObjectsHealed, "Healing returns a copy")
	info, err := s.DiskInfo(ctx, storage.DiskInfoOptions{})
	require.NoError(t, err)
	assert.True(t, info.Healing)
	assert.Equal(t, domain.StateHealing, info.State)

	// The tracker is loaded at startup.
	assert.NotNil(t, f(t, dir).Healing())

	// A finished heal leaves the drive online.
	tracker.Finished = true
	raw, _ = json.Marshal(tracker)
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, storage.HealingFile, raw))
	info, err = s.DiskInfo(ctx, storage.DiskInfoOptions{})
	require.NoError(t, err)
	assert.False(t, info.Healing)
	assert.Equal(t, domain.StateOnline, info.State)

	// Unreadable trackers are ignored; deleting the file ends the heal.
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, storage.HealingFile, []byte("{")))
	assert.Nil(t, s.Healing())
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, storage.HealingFile, raw))
	require.NotNil(t, s.Healing())
	require.NoError(t, s.Delete(ctx, domain.SystemBucket, storage.HealingFile, storage.DeleteOptions{}))
	assert.Nil(t, s.Healing())
}

func testStorageErrors(t *testing.T, f Factory) {
	ctx := context.Background()
	s, dir := open(t, f)
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()

	// Invalid data directories never become paths.
	bad := storage.FileInfo{ModTime: now, DataDir: "../../etc"}
	_, err := s.RenameData(ctx, domain.SystemBucket, "tmp/x", bad, "b", "k", storage.RenameOptions{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	assert.ErrorIs(t, s.WriteMetadata(ctx, "b", "k", bad), domain.ErrInvalidArgument)
	_, err = s.CheckParts(ctx, "b", "k", bad)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	assert.ErrorIs(t, s.DeleteVersion(ctx, "b", "k", storage.FileInfo{}, storage.DeleteOptions{OldDataDir: "../x"}), domain.ErrInvalidArgument)
	require.NoError(t, s.WriteMetadata(ctx, "b", "k", storage.FileInfo{ModTime: now, Data: []byte("x")}))
	assert.ErrorIs(t, s.UpdateMetadata(ctx, "b", "k", storage.FileInfo{ModTime: now, Size: -1}, storage.UpdateMetadataOptions{}), domain.ErrInvalidArgument)
	assert.ErrorIs(t, s.UpdateMetadata(ctx, "b", "k", storage.FileInfo{VersionID: "null", ModTime: now, DataDir: "x"}, storage.UpdateMetadataOptions{}), domain.ErrInvalidArgument)

	// "null" designates the null version everywhere.
	fi, err := s.ReadVersion(ctx, "b", "k", "null", storage.ReadOptions{ReadData: true})
	require.NoError(t, err)
	assert.Equal(t, "x", string(fi.Data))
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", storage.FileInfo{VersionID: "null"}, storage.DeleteOptions{}))

	// Streams: size checks and ranges.
	assert.ErrorIs(t, s.CreateFile(ctx, "b", "f", -2, strings.NewReader("")), domain.ErrInvalidArgument)
	err = s.CreateFile(ctx, "b", "f", 10, strings.NewReader("short"))
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	// A stream of unknown size cut midway (the reader fails, even with
	// io.ErrUnexpectedEOF) is an error and leaves no file behind.
	cut := io.MultiReader(strings.NewReader(strings.Repeat("x", 3<<20)), iotest.ErrReader(io.ErrUnexpectedEOF))
	assert.ErrorIs(t, s.CreateFile(ctx, "b", "cut", -1, cut), io.ErrUnexpectedEOF)
	_, err = s.ReadAll(ctx, "b", "cut")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "nothing is left of a cut stream")
	require.NoError(t, s.CreateFile(ctx, "b", "big", -1, strings.NewReader(strings.Repeat("y", 3<<20+5))))
	big, err := s.ReadAll(ctx, "b", "big")
	require.NoError(t, err)
	assert.Len(t, big, 3<<20+5)
	require.NoError(t, s.Delete(ctx, "b", "big", storage.DeleteOptions{}))
	require.NoError(t, s.CreateFile(ctx, "b", "f", 4, strings.NewReader("data and more")))
	all, err := s.ReadAll(ctx, "b", "f")
	require.NoError(t, err)
	assert.Equal(t, "data", string(all), "exactly size bytes are written")
	_, err = s.ReadFileStream(ctx, "b", "f", 2, 3)
	assert.ErrorIs(t, err, storage.ErrFileCorrupt)
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)
	_, err = s.ReadFileStream(ctx, "b", "f", -1, 1)
	assert.ErrorIs(t, err, domain.ErrInvalidRange)
	rc, err := s.ReadFileStream(ctx, "b", "f", 4, -1)
	require.NoError(t, err)
	rest, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.Empty(t, rest)
	_, err = s.ReadFileStream(ctx, "b", "nodir", 0, -1)
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	require.NoError(t, s.WriteAll(ctx, "b", "dir/file", []byte("x")))
	_, err = s.ReadFileStream(ctx, "b", "dir", 0, -1)
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "a directory is not a file")
	_, err = s.ReadAll(ctx, "b", "dir")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)

	// A file in the middle of a path makes it unusable.
	assert.ErrorIs(t, s.WriteAll(ctx, "b", "f/child", []byte("x")), domain.ErrInvalidObjectName)
	assert.ErrorIs(t, s.CreateFile(ctx, "b", "f/child", 1, strings.NewReader("x")), domain.ErrInvalidObjectName)
	_, err = s.ReadAll(ctx, "b", "f/child")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)

	// RenameData sources must be directories.
	_, err = s.RenameData(ctx, "b", "f", storage.FileInfo{ModTime: now, DataDir: uuid.NewString()}, "b", "obj", storage.RenameOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)

	// Damaged metadata is reported as such.
	require.NoError(t, s.WriteAll(ctx, "b", "broken/"+storage.MetaFile, []byte("garbage")))
	_, err = s.ReadVersion(ctx, "b", "broken", "", storage.ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrInvalidFormat)
	// A write starts the object over (the document goes to quarantine).
	assert.NoError(t, s.WriteMetadata(ctx, "b", "broken", storage.FileInfo{ModTime: now}))

	// Volumes holding only empty directories are empty.
	require.NoError(t, s.MakeVol(ctx, "leftovers"))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "leftovers", "a", "b"), 0o750))
	require.NoError(t, s.DeleteVol(ctx, "leftovers", false))
	_, err = s.StatVol(ctx, "leftovers")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	assert.ErrorIs(t, s.DeleteVol(ctx, "b", false), domain.ErrBucketNotEmpty)
	assert.ErrorIs(t, s.MakeVol(ctx, "a\\b"), domain.ErrInvalidBucketName)

	// The tmp directory is recreated when removed under a running drive.
	require.NoError(t, os.RemoveAll(filepath.Join(dir, domain.SystemBucket, storage.TmpDir)))
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, "config/after", []byte("ok")))
}

// fixture reads a file of the storage package testdata directory.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "locate the storagetest sources")
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "testdata", "xlmeta", name)) // #nosec G304 -- a test fixture of the repository
	require.NoError(t, err)
	return data
}

func testLegacyMetadataOnDisk(t *testing.T, f Factory) {
	ctx := context.Background()
	s, _ := open(t, f)
	require.NoError(t, s.MakeVol(ctx, "b"))
	require.NoError(t, s.WriteAll(ctx, "b", "old/"+storage.MetaFile, fixture(t, "legacy-json.meta")))

	fi, err := s.ReadVersion(ctx, "b", "old", "null", storage.ReadOptions{ReadData: true})
	require.NoError(t, err)
	assert.Equal(t, "hello", string(fi.Data))
	assert.Equal(t, []string{"old"}, objectNames(t, s, storage.WalkDirOptions{Bucket: "b", Recursive: true}))

	// The first update rewrites the document in the binary format.
	fi.Metadata["x-amz-meta-new"] = "1"
	require.NoError(t, s.UpdateMetadata(ctx, "b", "old", fi, storage.UpdateMetadataOptions{}))
	raw, err := s.ReadAll(ctx, "b", "old/"+storage.MetaFile)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(raw, []byte("NXLM")))
	fi, err = s.ReadVersion(ctx, "b", "old", "", storage.ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, fi.NumVersions)
}

func testSharedDataDirSurvives(t *testing.T, f Factory) {
	ctx := context.Background()
	s, _ := open(t, f)
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()
	v1, err := putObject(ctx, s, "b", "k", "v1", now, bytes.Repeat([]byte("s"), storage.SmallFileThreshold))
	require.NoError(t, err)
	// A second version referencing the same data (in-place copy).
	v2 := v1
	v2.VersionID, v2.ModTime = "v2", now.Add(time.Second)
	require.NoError(t, s.WriteMetadata(ctx, "b", "k", v2))
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", storage.FileInfo{VersionID: "v1"}, storage.DeleteOptions{}))
	got, err := getObject(ctx, s, "b", "k", "v2")
	require.NoError(t, err)
	assert.Len(t, got, storage.SmallFileThreshold, "the data directory still referenced by v2 is kept")

	// Inline versions have no part files: CheckParts looks at xl.meta.
	inline, err := putObject(ctx, s, "b", "small", "", now, []byte("x"))
	require.NoError(t, err)
	inline.Parts = []storage.PartInfo{{Number: 1, Size: 1}}
	check, err := s.CheckParts(ctx, "b", "small", inline)
	require.NoError(t, err)
	assert.Equal(t, []int{storage.CheckPartSuccess}, check.Results)
	require.NoError(t, s.WriteAll(ctx, "b", "nometa/other", []byte("x")))
	check, err = s.CheckParts(ctx, "b", "nometa", inline)
	require.NoError(t, err)
	assert.Equal(t, []int{storage.CheckPartFileNotFound}, check.Results)
}

// testCorruptMetadataQuarantined: an undecodable xl.meta fails the reads
// and the in-place updates, but a write that records a version (commit,
// metadata write, delete marker) or removes one moves it into the
// quarantine area and starts the object over instead of failing for ever.
func testCorruptMetadataQuarantined(t *testing.T, f Factory) {
	ctx := context.Background()
	s, dir := open(t, f)
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()
	corrupt := func(key string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "b", key, storage.MetaFile), []byte("garbage, not an xl.meta"), 0o640)) // #nosec G306 -- a corrupt fixture written by a test
	}
	quarantined := func() int {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(dir, domain.SystemBucket, storage.QuarantineDir))
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		require.NoError(t, err)
		return len(entries)
	}

	_, err := putObject(ctx, s, "b", "k", "v1", now, []byte("one"))
	require.NoError(t, err)
	corrupt("k")
	_, err = s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{})
	require.ErrorIs(t, err, domain.ErrInvalidFormat)
	err = s.UpdateMetadata(ctx, "b", "k", version("v1", now, 3), storage.UpdateMetadataOptions{})
	require.ErrorIs(t, err, domain.ErrInvalidFormat, "an update has nothing to update")
	assert.Equal(t, 0, quarantined())

	// Inline write and commit of a data directory.
	_, err = putObject(ctx, s, "b", "k", "v2", now.Add(time.Second), []byte("two"))
	require.NoError(t, err)
	assert.Equal(t, 1, quarantined())
	got, err := getObject(ctx, s, "b", "k", "v2")
	require.NoError(t, err)
	assert.Equal(t, "two", string(got))
	_, err = s.ReadVersion(ctx, "b", "k", "v1", storage.ReadOptions{})
	require.ErrorIs(t, err, domain.ErrNoSuchVersion, "the versions of the corrupt document are gone from this drive")
	corrupt("k")
	big := bytes.Repeat([]byte("B"), storage.SmallFileThreshold)
	_, err = putObject(ctx, s, "b", "k", "v3", now.Add(2*time.Second), big)
	require.NoError(t, err)
	assert.Equal(t, 2, quarantined())
	got, err = getObject(ctx, s, "b", "k", "v3")
	require.NoError(t, err)
	assert.Equal(t, big, got)

	// Delete marker, then removal of a version.
	corrupt("k")
	marker := storage.FileInfo{VersionID: "m1", ModTime: now.Add(3 * time.Second), MarkDeleted: true, Versioned: true}
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", marker, storage.DeleteOptions{}))
	fi, err := s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{})
	require.NoError(t, err)
	assert.True(t, fi.Deleted)
	assert.Equal(t, 1, fi.NumVersions)
	corrupt("k")
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", storage.FileInfo{VersionID: "m1"}, storage.DeleteOptions{}),
		"the drive no longer holds the version")
	_, err = s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{})
	require.ErrorIs(t, err, domain.ErrObjectNotFound)
	assert.Equal(t, 4, quarantined())
}

// testDeleteVersionKeepData: with KeepData, DeleteVersion leaves the data
// directory of the removed version (or of the null version a delete
// marker replaces) in place, for the caller to purge or restore.
func testDeleteVersionKeepData(t *testing.T, f Factory) {
	ctx := context.Background()
	s, _ := open(t, f)
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()
	big := bytes.Repeat([]byte("K"), storage.SmallFileThreshold)
	for _, last := range []bool{true, false} {
		key := fmt.Sprintf("k-%v", last)
		fi, err := putObject(ctx, s, "b", key, "", now, big)
		require.NoError(t, err)
		if !last {
			_, err = putObject(ctx, s, "b", key, "v2", now.Add(time.Second), []byte("x"))
			require.NoError(t, err)
		}
		require.NoError(t, s.DeleteVersion(ctx, "b", key, storage.FileInfo{}, storage.DeleteOptions{KeepData: true}))
		_, err = s.ReadVersion(ctx, "b", key, "null", storage.ReadOptions{})
		require.Error(t, err, "the version is removed")
		check, err := s.CheckParts(ctx, "b", key, fi)
		require.NoError(t, err)
		assert.Equal(t, []int{storage.CheckPartSuccess}, check.Results, "its data stays in place")
		// Restored by its metadata.
		require.NoError(t, s.WriteMetadata(ctx, "b", key, fi))
		got, err := getObject(ctx, s, "b", key, "null")
		require.NoError(t, err)
		assert.Equal(t, big, got)
	}

	// A suspended-versioning delete marker replacing the null version.
	fi, err := putObject(ctx, s, "b", "m", "", now, big)
	require.NoError(t, err)
	marker := storage.FileInfo{ModTime: now.Add(time.Second), MarkDeleted: true, Versioned: true}
	require.NoError(t, s.DeleteVersion(ctx, "b", "m", marker, storage.DeleteOptions{KeepData: true}))
	check, err := s.CheckParts(ctx, "b", "m", fi)
	require.NoError(t, err)
	assert.Equal(t, []int{storage.CheckPartSuccess}, check.Results)
}
