package storage

import (
	"context"
	"errors"
	"fmt"
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
