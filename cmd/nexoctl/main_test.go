package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
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

func TestVersion(t *testing.T) {
	out, err := run(t, cmdDeps{}, "version")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "nexoctl dev"), out)
	out, err = run(t, cmdDeps{}, "version", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"goVersion"`)
}

func TestAliasLifecycleWithTemporaryHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(cli.EnvConfigDir, "")
	deps := cmdDeps{} // resolves ~/.nexoctl/config.json

	out, err := run(t, deps, "alias", "set", "local", "http://localhost:9000", "admin", "password123")
	require.NoError(t, err)
	assert.Contains(t, out, `Added alias "local"`)

	path := filepath.Join(home, cli.ConfigDirName, cli.ConfigFileName)
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

func TestExplicitConfigPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "config.json")
	deps := cmdDeps{configPath: path}
	_, err := run(t, deps, "alias", "set", "x", "http://h", "a", "b", "--json")
	require.NoError(t, err)
	_, err = os.Stat(path)
	require.NoError(t, err)
}
