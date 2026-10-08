package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	climock "github.com/r3n3r0/nexo/internal/cli/mock"
)

func run(t *testing.T, deps cmdDeps, args ...string) (string, error) {
	t.Helper()
	out := &bytes.Buffer{}
	deps.stdout, deps.stderr = out, out
	cmd := newRootCmd(deps)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// envOf returns a lookupEnv over a fixed map.
func envOf(env map[string]string) cli.LookupEnv {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func TestVersion(t *testing.T) {
	out, err := run(t, cmdDeps{}, "version")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "nexoctl dev"), out)
	out, err = run(t, cmdDeps{}, "version", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"goVersion"`)
}

func TestAliasLifecycleWithConfigDir(t *testing.T) {
	dir := t.TempDir()
	deps := cmdDeps{lookupEnv: envOf(map[string]string{cli.EnvConfigDir: dir})}

	out, err := run(t, deps, "alias", "set", "local", "http://localhost:9000", "admin", "password123")
	require.NoError(t, err)
	assert.Contains(t, out, `Added alias "local"`)

	path := filepath.Join(dir, cli.ConfigFileName)
	st, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"password123"`)

	_, err = run(t, deps, "alias", "set", "prod", "https://s3.example.com", "AK", "SK", "--path", "on")
	require.NoError(t, err)

	out, err = run(t, deps, "alias", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "NAME")
	assert.Contains(t, out, "local")
	assert.Contains(t, out, "prod")
	assert.NotContains(t, out, "password123", "secrets are never listed")

	out, err = run(t, deps, "alias", "ls", "--json")
	require.NoError(t, err)
	var views []aliasView
	require.NoError(t, json.Unmarshal([]byte(out), &views))
	require.Len(t, views, 2)
	assert.Equal(t, "local", views[0].Name)
	assert.Equal(t, "on", views[1].Path)

	out, err = run(t, deps, "alias", "remove", "local")
	require.NoError(t, err)
	assert.Contains(t, out, `Removed alias "local"`)
	_, err = run(t, deps, "alias", "rm", "local")
	assert.Error(t, err)

	_, err = run(t, deps, "alias", "set", "bad", "ftp://x", "a", "b")
	assert.Error(t, err)
	_, err = run(t, deps, "alias", "set", "missing-args")
	assert.Error(t, err)
}

func TestDefaultConfigPathUsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	deps := cmdDeps{lookupEnv: envOf(nil)} // no NEXOCTL_CONFIG_DIR: ~/.nexoctl/config.json
	_, err := run(t, deps, "alias", "set", "x", "http://h", "a", "b", "--json")
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(home, cli.ConfigDirName, cli.ConfigFileName))
	require.NoError(t, err)
}

func TestAliasCommandsUseTheInjectedService(t *testing.T) {
	svc := climock.NewAliasService(t)
	deps := cmdDeps{aliases: func() (cli.AliasService, error) { return svc, nil }}

	svc.EXPECT().Set(mock.Anything, cli.Alias{Name: "local", URL: "http://localhost:9000", AccessKey: "admin", SecretKey: "secret", API: "s3v4", Path: "auto"}).Return(nil).Once()
	out, err := run(t, deps, "alias", "set", "local", "http://localhost:9000", "admin", "secret", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"secretKey": "***"`, "the secret is redacted in the output")

	svc.EXPECT().List(mock.Anything).Return([]cli.Alias{{Name: "local", URL: "http://localhost:9000", AccessKey: "admin", API: "s3v4", Path: "auto"}}, nil).Once()
	out, err = run(t, deps, "alias", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "local")

	svc.EXPECT().Remove(mock.Anything, "local").Return(errors.New("boom")).Once()
	_, err = run(t, deps, "alias", "remove", "local")
	assert.EqualError(t, err, "boom")

	failing := cmdDeps{aliases: func() (cli.AliasService, error) { return nil, errors.New("no config") }}
	_, err = run(t, failing, "alias", "list")
	assert.EqualError(t, err, "no config")
}
