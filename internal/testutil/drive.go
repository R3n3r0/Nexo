package testutil

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// NewLocalDrive opens a local drive rooted at dir (a fresh temporary
// directory when dir is empty) for the tests of the storage, object and
// bucket packages: fsync is disabled, logs are discarded and the drive is
// closed when the test ends. Any error fails the test.
func NewLocalDrive(t testing.TB, dir string) storage.StorageAPI {
	t.Helper()
	return NewLocalDriveIndex(t, dir, 0)
}

// NewLocalDriveIndex is NewLocalDrive for the drive at position idx of its
// erasure set (domain.Endpoint.DiskIdx).
func NewLocalDriveIndex(t testing.TB, dir string, idx int) storage.StorageAPI {
	t.Helper()
	if dir == "" {
		dir = filepath.Join(t.TempDir(), "drive")
	}
	ep, err := domain.ParseEndpoint(dir)
	if err != nil {
		t.Fatalf("testutil: parse endpoint %q: %v", dir, err)
	}
	ep.DiskIdx = idx
	d, err := storage.NewLocal(storage.Deps{Logger: slog.New(slog.DiscardHandler)}, ep, storage.WithoutFsync())
	if err != nil {
		t.Fatalf("testutil: open drive %q: %v", dir, err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}
