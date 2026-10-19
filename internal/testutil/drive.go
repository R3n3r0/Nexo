package testutil

import (
	"context"
	"log/slog"
	"os"
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

// MetadataLogTests reports whether the tests run their drives with the
// metadata log (NEXO_TEST_METADATA_LOG=1): the suites of the object layer
// and of the server then cover both layouts of the metadata.
func MetadataLogTests() bool { return os.Getenv("NEXO_TEST_METADATA_LOG") == "1" }

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
	opts := []storage.Option{storage.WithoutFsync()}
	if MetadataLogTests() {
		opts = append(opts, storage.WithMetadataLog(0, 0))
	}
	d, err := storage.NewLocal(storage.Deps{Logger: slog.New(slog.DiscardHandler)}, ep, opts...)
	if err != nil {
		t.Fatalf("testutil: open drive %q: %v", dir, err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}
