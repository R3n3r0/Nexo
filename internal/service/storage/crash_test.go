package storage

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
)

// openDrive opens the drive rooted at dir.
func openDrive(t *testing.T, dir string, opts ...Option) *xlStorage {
	t.Helper()
	ep, err := domain.ParseEndpoint(dir)
	require.NoError(t, err)
	s, err := NewLocal(Deps{Logger: slog.New(slog.DiscardHandler)}, ep, append([]Option{WithoutFsync()}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s.(*xlStorage)
}

// stageData writes the parts of a new version into tmp/<id> as the object
// layer does before RenameData.
func stageData(t *testing.T, s StorageAPI, id string, parts ...string) FileInfo {
	t.Helper()
	ctx := context.Background()
	fi := FileInfo{ModTime: t0, DataDir: dd1}
	for i, p := range parts {
		require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, TmpDir+"/"+id+"/"+partName(i+1), int64(len(p)), strings.NewReader(p)))
		fi.Parts = append(fi.Parts, PartInfo{Number: i + 1, Size: int64(len(p))})
		fi.Size += int64(len(p))
	}
	return fi
}

func readPart(t *testing.T, s StorageAPI, volume, key string, fi FileInfo, part int) string {
	t.Helper()
	rc, err := s.ReadFileStream(context.Background(), volume, ObjectDataPath(key, fi.DataDir, part), 0, -1)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	return string(data)
}

func tmpEntries(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, domain.SystemBucket, TmpDir))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestCrashBeforeCommit simulates a process killed between the staging of
// a new version and the commit rename of xl.meta: the writer is frozen at
// the commit point and a new instance opens the same drive. The previous
// version stays readable, the staged xl.meta and data are swept from tmp,
// and the data directory moved before the crash is invisible.
func TestCrashBeforeCommit(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "drive")
	s := openDrive(t, dir)
	require.NoError(t, s.MakeVol(ctx, "b"))

	// Version 1 is committed normally.
	fi1 := stageData(t, s, "up1", "old")
	fi1.DataDir = dd2
	_, err := s.RenameData(ctx, domain.SystemBucket, TmpDir+"/up1", fi1, "b", "k", RenameOptions{})
	require.NoError(t, err)
	// A new object will crash before its first commit; its key uses another
	// lock stripe than "k" so that both writers reach the commit point.
	fresh := "fresh"
	for i := 0; s.stripe("b", fresh) == s.stripe("b", "k"); i++ {
		fresh = "fresh" + strconv.Itoa(i)
	}
	freshFI := stageData(t, s, "up3", "fresh")
	freshFI.DataDir = "0a0a0a0a-2222-4333-8444-555566667777"

	// The writers freeze right before xl.meta is renamed into place.
	frozen, release := make(chan struct{}, 2), make(chan struct{})
	s.rename = func(oldpath, newpath string) error {
		if filepath.Base(newpath) == MetaFile && strings.Contains(oldpath, TmpDir) {
			frozen <- struct{}{}
			<-release
			return errors.New("killed")
		}
		return os.Rename(oldpath, newpath)
	}
	fi2 := stageData(t, s, "up2", "new data")
	fi2.ModTime = t0.Add(1)
	done := make(chan error, 2)
	go func() {
		_, err := s.RenameData(ctx, domain.SystemBucket, TmpDir+"/up2", fi2, "b", "k", RenameOptions{})
		done <- err
	}()
	go func() {
		_, err := s.RenameData(ctx, domain.SystemBucket, TmpDir+"/up3", freshFI, "b", fresh, RenameOptions{})
		done <- err
	}()
	<-frozen
	<-frozen

	// The "killed" process left: the moved data directories, the staged
	// xl.meta of both writes, a partial part file.
	_, err = os.Stat(filepath.Join(dir, "b", "k", fi2.DataDir))
	require.NoError(t, err, "the data directory was moved before the crash")
	require.NoError(t, os.WriteFile(filepath.Join(dir, domain.SystemBucket, TmpDir, "partial"), []byte("half"), 0o600))
	require.Len(t, tmpEntries(t, dir), 3)

	// Restart: a new instance on the same drive.
	s2 := openDrive(t, dir)
	assert.Empty(t, tmpEntries(t, dir), "tmp is emptied at startup")
	got, err := s2.ReadVersion(ctx, "b", "k", "", ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, fi1.DataDir, got.DataDir, "the previous version is still the latest")
	assert.Equal(t, 1, got.NumVersions)
	assert.Equal(t, "old", readPart(t, s2, "b", "k", got, 1))
	_, err = s2.ReadVersion(ctx, "b", fresh, "", ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "an uncommitted object does not exist")

	for _, recursive := range []bool{false, true} {
		var names []string
		require.NoError(t, s2.WalkDir(ctx, WalkDirOptions{Bucket: "b", Recursive: recursive}, func(e WalkEntry) error {
			names = append(names, e.Name)
			return nil
		}))
		assert.Equal(t, []string{"k"}, names, "orphan data directories are not listed")
	}

	// Deleting the last version also removes the orphan data directories.
	require.NoError(t, s2.DeleteVersion(ctx, "b", "k", FileInfo{}, DeleteOptions{}))
	_, err = os.Stat(filepath.Join(dir, "b", "k"))
	assert.True(t, errors.Is(err, fs.ErrNotExist))

	// The frozen writers resume and fail.
	close(release)
	for range 2 {
		assert.Error(t, <-done)
	}
}

// TestRenameDataRollback checks that a failed commit puts the data
// directory back in the temporary area and keeps the previous version.
func TestRenameDataRollback(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "drive")
	s := openDrive(t, dir)
	require.NoError(t, s.MakeVol(ctx, "b"))
	fi1 := stageData(t, s, "up1", "v1")
	_, err := s.RenameData(ctx, domain.SystemBucket, TmpDir+"/up1", fi1, "b", "k", RenameOptions{})
	require.NoError(t, err)

	failCommit := func(oldpath, newpath string) error {
		if filepath.Base(newpath) == MetaFile {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errors.New("injected")}
		}
		return os.Rename(oldpath, newpath)
	}

	t.Run("new data directory", func(t *testing.T) {
		s.rename = failCommit
		defer func() { s.rename = os.Rename }()
		fi2 := stageData(t, s, "up2", "v2")
		fi2.DataDir, fi2.ModTime = dd2, t0.Add(1)
		_, err := s.RenameData(ctx, domain.SystemBucket, TmpDir+"/up2", fi2, "b", "k", RenameOptions{})
		require.Error(t, err)
		_, err = os.Stat(filepath.Join(dir, domain.SystemBucket, TmpDir, "up2", "part.1"))
		assert.NoError(t, err, "the data is back in tmp for the caller to retry or clean")
		_, err = os.Stat(filepath.Join(dir, "b", "k", dd2))
		assert.True(t, errors.Is(err, fs.ErrNotExist))
		got, err := s.ReadVersion(ctx, "b", "k", "", ReadOptions{})
		require.NoError(t, err)
		assert.Equal(t, fi1.DataDir, got.DataDir)
	})

	t.Run("same data directory", func(t *testing.T) {
		s.rename = failCommit
		defer func() { s.rename = os.Rename }()
		again := stageData(t, s, "up3", "XX")
		again.ModTime = t0.Add(2)
		_, err := s.RenameData(ctx, domain.SystemBucket, TmpDir+"/up3", again, "b", "k", RenameOptions{})
		require.Error(t, err)
		got, err := s.ReadVersion(ctx, "b", "k", "", ReadOptions{})
		require.NoError(t, err)
		assert.Equal(t, "v1", readPart(t, s, "b", "k", got, 1), "the displaced data directory is restored")
		_, err = os.Stat(filepath.Join(dir, domain.SystemBucket, TmpDir, "up3", "part.1"))
		assert.NoError(t, err)
	})

	t.Run("same data directory committed", func(t *testing.T) {
		again := FileInfo{ModTime: t0.Add(3), DataDir: dd1, Size: 2, Parts: []PartInfo{{Number: 1, Size: 2}}}
		_, err := s.RenameData(ctx, domain.SystemBucket, TmpDir+"/up3", again, "b", "k", RenameOptions{})
		require.NoError(t, err)
		got, err := s.ReadVersion(ctx, "b", "k", "", ReadOptions{})
		require.NoError(t, err)
		assert.Equal(t, "XX", readPart(t, s, "b", "k", got, 1))
		assert.Equal(t, []string{"up2"}, tmpEntries(t, dir), "the displaced copy is removed after the commit (up2 is the rolled back upload)")
	})

	t.Run("data rename fails", func(t *testing.T) {
		s.rename = func(oldpath, newpath string) error {
			if strings.HasSuffix(newpath, dd2) {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errors.New("injected")}
			}
			return os.Rename(oldpath, newpath)
		}
		defer func() { s.rename = os.Rename }()
		fi := stageData(t, s, "up4", "zz")
		fi.DataDir, fi.ModTime = dd2, t0.Add(4)
		_, err := s.RenameData(ctx, domain.SystemBucket, TmpDir+"/up4", fi, "b", "new/key", RenameOptions{})
		require.Error(t, err)
		_, err = os.Stat(filepath.Join(dir, "b", "new"))
		assert.True(t, errors.Is(err, fs.ErrNotExist), "the object directory created for the commit is pruned")
	})
}
