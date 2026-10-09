package storage_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// TestJanitorSweepsStaleTmpEntries: the janitor removes the temporary
// entries (files, and directories whose every file is) older than the
// threshold, and keeps the fresh ones: a write in progress keeps its
// files fresh.
func TestJanitorSweepsStaleTmpEntries(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "drive")
	ep, err := domain.ParseEndpoint(dir)
	require.NoError(t, err)
	d, err := storage.NewLocal(storage.Deps{Logger: slog.New(slog.DiscardHandler)}, ep, storage.WithoutFsync())
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close(ctx) })

	now := time.Now()
	old := now.Add(-48 * time.Hour)
	tmp := filepath.Join(dir, domain.SystemBucket, storage.TmpDir)
	plant := func(rel string, at time.Time) {
		t.Helper()
		name := filepath.Join(tmp, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o750))
		require.NoError(t, os.WriteFile(name, []byte("x"), 0o640))
		require.NoError(t, os.Chtimes(name, at, at))
	}
	age := func(rel string) {
		t.Helper()
		require.NoError(t, os.Chtimes(filepath.Join(tmp, rel), old, old))
	}
	plant("stale-file", old)
	plant("fresh-file", now)
	plant("stale-dir/part.1", old)
	age("stale-dir")
	plant("busy-dir/part.1", old)
	plant("busy-dir/sub/part.2", now) // still being written
	age("busy-dir/sub")
	age("busy-dir")

	j, err := storage.NewJanitor(storage.JanitorDeps{Drives: []storage.StorageAPI{d}, Clock: clock.NewFake(now), Logger: slog.New(slog.DiscardHandler)},
		storage.WithMaxTmpAge(24*time.Hour))
	require.NoError(t, err)
	removed, err := j.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, removed)
	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"fresh-file", "busy-dir"}, names)

	// The background sweeps run until Stop.
	plant("later", old)
	j, err = storage.NewJanitor(storage.JanitorDeps{Drives: []storage.StorageAPI{d}, Logger: slog.New(slog.DiscardHandler)},
		storage.WithSweepInterval(10*time.Millisecond), storage.WithMaxTmpAge(time.Hour))
	require.NoError(t, err)
	require.NoError(t, j.Start(ctx))
	assert.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(tmp, "later"))
		return os.IsNotExist(err)
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, j.Stop(ctx))
	require.NoError(t, j.Stop(ctx), "Stop is idempotent")
}

func TestNewJanitorRefusesNilDrive(t *testing.T) {
	_, err := storage.NewJanitor(storage.JanitorDeps{Drives: []storage.StorageAPI{nil}})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
