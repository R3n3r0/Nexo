package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

func TestDefaultConfigPathUsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	none := func(string) (string, bool) { return "", false }
	p, err := cli.DefaultConfigPath(none)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, cli.ConfigDirName, cli.ConfigFileName), p)

	custom := func(k string) (string, bool) { return "/custom/dir", k == cli.EnvConfigDir }
	p, err = cli.DefaultConfigPath(custom)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/custom/dir", cli.ConfigFileName), p)

	t.Setenv(cli.EnvConfigDir, "/from/env")
	p, err = cli.DefaultConfigPath(nil)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/from/env", cli.ConfigFileName), p, "nil lookup reads the process environment")
}

func TestFileStoreRoundTripAndPermissions(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	path := filepath.Join(home, cli.ConfigDirName, cli.ConfigFileName)
	store := cli.NewFileStore(path)
	assert.Equal(t, path, store.Path())

	cfg, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Empty(t, cfg.Aliases)
	assert.Equal(t, cli.ConfigVersion, cfg.Version)

	cfg.Aliases["local"] = cli.Alias{URL: "http://localhost:9000", AccessKey: "a", SecretKey: "s"}
	require.NoError(t, store.Save(ctx, cfg))

	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
		dst, err := os.Stat(filepath.Dir(path))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), dst.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temp files left behind")

	again, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, "local", again.Aliases["local"].Name)
	assert.Equal(t, "http://localhost:9000", again.Aliases["local"].URL)

	require.NoError(t, os.WriteFile(path, []byte("{bad"), 0o600))
	_, err = store.Load(ctx)
	assert.Error(t, err)
	assert.Error(t, store.Save(ctx, nil))
}

func TestAliasService(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), cli.ConfigDirName, cli.ConfigFileName)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc := cli.NewAliasService(cli.AliasDeps{Store: cli.NewFileStore(path), Clock: clock.NewFake(now)})

	require.NoError(t, svc.Set(ctx, cli.Alias{Name: "prod", URL: "https://s3.example.com", AccessKey: "AK", SecretKey: "SK"}))
	require.NoError(t, svc.Set(ctx, cli.Alias{Name: "local", URL: "http://127.0.0.1:9000", AccessKey: "admin", SecretKey: "password123"}))

	list, err := svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "local", list[0].Name)
	assert.Equal(t, "prod", list[1].Name)
	assert.Equal(t, "s3v4", list[0].API)
	assert.Equal(t, "auto", list[0].Path)
	assert.Equal(t, now, list[0].UpdatedAt)

	got, err := svc.Get(ctx, "prod")
	require.NoError(t, err)
	assert.Equal(t, "AK", got.AccessKey)

	// Overwrite.
	require.NoError(t, svc.Set(ctx, cli.Alias{Name: "prod", URL: "https://s3.example.com", AccessKey: "AK2", SecretKey: "SK2"}))
	got, _ = svc.Get(ctx, "prod")
	assert.Equal(t, "AK2", got.AccessKey)

	require.NoError(t, svc.Remove(ctx, "prod"))
	_, err = svc.Get(ctx, "prod")
	assert.ErrorIs(t, err, domain.ErrUserNotFound)
	assert.ErrorIs(t, svc.Remove(ctx, "prod"), domain.ErrUserNotFound)

	for _, bad := range []cli.Alias{
		{Name: "", URL: "http://x", AccessKey: "a", SecretKey: "b"},
		{Name: "bad name", URL: "http://x", AccessKey: "a", SecretKey: "b"},
		{Name: "x", URL: "ftp://x", AccessKey: "a", SecretKey: "b"},
		{Name: "x", URL: "http://x", AccessKey: "", SecretKey: "b"},
	} {
		assert.ErrorIs(t, svc.Set(ctx, bad), domain.ErrInvalidArgument)
	}
}
