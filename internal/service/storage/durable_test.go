package storage_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// TestDurableWrites runs the write paths with fsync enabled (the default):
// atomic small files, metadata, data files and their commit.
func TestDurableWrites(t *testing.T) {
	ctx := context.Background()
	ep, err := domain.ParseEndpoint(filepath.Join(t.TempDir(), "drive"))
	require.NoError(t, err)
	s, err := storage.NewLocal(storage.Deps{Logger: slog.New(slog.DiscardHandler)}, ep)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(ctx) })
	require.NoError(t, s.MakeVol(ctx, "b"))

	// Small files are written atomically.
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, "config/a.json", []byte(`{"a":1}`)))
	require.NoError(t, s.WriteAll(ctx, domain.SystemBucket, "config/a.json", []byte(`{"a":2}`)))
	data, err := s.ReadAll(ctx, domain.SystemBucket, "config/a.json")
	require.NoError(t, err)
	assert.Equal(t, `{"a":2}`, string(data))

	// Metadata-only versions (inline data).
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	require.NoError(t, s.WriteMetadata(ctx, "b", "inline", storage.FileInfo{ModTime: now, Size: 2, Data: []byte("hi")}))
	fi, err := s.ReadVersion(ctx, "b", "inline", "", storage.ReadOptions{ReadData: true})
	require.NoError(t, err)
	assert.Equal(t, "hi", string(fi.Data))
	// Non-critical updates skip the fsync and keep the content.
	fi.Metadata = map[string]string{"x-amz-meta-a": "1"}
	fi.Data = nil
	require.NoError(t, s.UpdateMetadata(ctx, "b", "inline", fi, storage.UpdateMetadataOptions{NoPersistence: true}))
	fi, err = s.ReadVersion(ctx, "b", "inline", "", storage.ReadOptions{ReadData: true})
	require.NoError(t, err)
	assert.Equal(t, "hi", string(fi.Data))
	assert.Equal(t, "1", fi.Metadata["x-amz-meta-a"])

	// Data files streamed to the temporary area, then committed.
	dataDir := "0d8a8a3e-6a8f-4f4a-9b4f-1b9b9b9b9b9b"
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/u1/part.1", -1, strings.NewReader("durable")))
	require.NoError(t, s.RenameFile(ctx, domain.SystemBucket, "tmp/u1/part.1", domain.SystemBucket, "tmp/final/part.1"))
	_, err = s.RenameData(ctx, domain.SystemBucket, "tmp/final", storage.FileInfo{
		ModTime: now, Size: 7, DataDir: dataDir, Parts: []storage.PartInfo{{Number: 1, Size: 7}},
	}, "b", "dir/obj", storage.RenameOptions{})
	require.NoError(t, err)
	rc, err := s.ReadFileStream(ctx, "b", storage.ObjectDataPath("dir/obj", dataDir, 1), 0, -1)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "durable", string(got))
}

func TestObjectDataPaths(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		part     int
		wantPath string
		wantDir  string
	}{
		{name: "object", key: "photos/a.jpg", part: 1, wantPath: "photos/a.jpg/dd/part.1", wantDir: "photos/a.jpg/dd"},
		{name: "second part", key: "k", part: 2, wantPath: "k/dd/part.2", wantDir: "k/dd"},
		{name: "directory object", key: "folder/", part: 1, wantPath: "folder__XLDIR__/dd/part.1", wantDir: "folder__XLDIR__/dd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantPath, storage.ObjectDataPath(tt.key, "dd", tt.part))
			assert.Equal(t, tt.wantDir, storage.ObjectDataDir(tt.key, "dd"))
		})
	}
}
