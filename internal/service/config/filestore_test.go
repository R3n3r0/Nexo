package config_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
)

func TestFileStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sys", "config", "config.json")
	store, err := config.NewFileStore(path)
	require.NoError(t, err)

	all, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Empty(t, all, "a missing file is an empty configuration")

	want := map[string]config.KV{"scanner": {"speed": "slow"}, "notify_webhook": {"endpoint": "http://hook", "auth_token": "secret"}, "empty": {}}
	require.NoError(t, store.Save(ctx, want))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "settings may hold credentials")

	// A new store on the same file (a restart) reads the same document.
	again, err := config.NewFileStore(path)
	require.NoError(t, err)
	got, err := again.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// Save replaces the whole document; no temporary file is left.
	require.NoError(t, again.Save(ctx, map[string]config.KV{"scanner": {"speed": "fast"}}))
	got, err = store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]config.KV{"scanner": {"speed": "fast"}}, got)
	require.NoError(t, store.Save(ctx, nil))
	got, err = store.Load(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "config.json", entries[0].Name())
}

func TestFileStoreErrors(t *testing.T) {
	ctx := context.Background()
	_, err := config.NewFileStore("")
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
		wantErr error
		want    map[string]config.KV
	}{
		{name: "damaged", content: "{not json", wantErr: domain.ErrInvalidFormat},
		{name: "wrong shape", content: `{"scanner": "fast"}`, wantErr: domain.ErrInvalidFormat},
		{name: "null subsystem", content: `{"scanner": null}`, want: map[string]config.KV{"scanner": {}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			store, err := config.NewFileStore(path)
			require.NoError(t, err)
			got, err := store.Load(ctx)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	// Canceled contexts and unwritable locations fail.
	store, err := config.NewFileStore(filepath.Join(dir, "ok.json"))
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.Load(canceled)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, store.Save(canceled, nil), context.Canceled)

	blocker := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	_, err = config.NewFileStore(filepath.Join(blocker, "sub", "config.json"))
	require.Error(t, err, "the parent cannot be created under a file")
	gone := filepath.Join(dir, "gone")
	store, err = config.NewFileStore(filepath.Join(gone, "config.json"))
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(gone))
	require.Error(t, store.Save(ctx, map[string]config.KV{"a": {}}), "the directory disappeared")
	require.NoError(t, os.MkdirAll(filepath.Join(gone, "config.json"), 0o700))
	_, err = store.Load(ctx)
	require.Error(t, err, "the path is a directory")
}

// The service persists through the file store across restarts.
func TestServiceWithFileStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := config.NewFileStore(path)
	require.NoError(t, err)
	svc := config.New(config.Deps{Store: store})
	require.NoError(t, svc.Set(ctx, "scanner", config.KV{"speed": "slow"}))
	require.NoError(t, svc.Set(ctx, "region", config.KV{"name": "eu-south-1"}))
	require.NoError(t, svc.Delete(ctx, "region"))

	store, err = config.NewFileStore(path)
	require.NoError(t, err)
	restarted := config.New(config.Deps{Store: store})
	kv, err := restarted.Get(ctx, "scanner")
	require.NoError(t, err)
	assert.Equal(t, config.KV{"speed": "slow"}, kv)
	names, err := restarted.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"scanner"}, names)
}
