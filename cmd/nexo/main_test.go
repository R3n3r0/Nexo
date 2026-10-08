package main

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testDeps(env map[string]string, client *http.Client) (cmdDeps, *bytes.Buffer, *bytes.Buffer) {
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	if client == nil {
		client = http.DefaultClient
	}
	return cmdDeps{
		stdout: out, stderr: errBuf, client: client,
		lookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	}, out, errBuf
}

func TestVersionCommand(t *testing.T) {
	deps, out, _ := testDeps(nil, nil)
	cmd := newRootCmd(deps)
	cmd.SetArgs([]string{"version"})
	require.NoError(t, cmd.Execute())
	assert.True(t, strings.HasPrefix(out.String(), "nexo dev"), out.String())

	out.Reset()
	cmd = newRootCmd(deps)
	cmd.SetArgs([]string{"version", "--json"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), `"version": "dev"`)
}

func TestHealthcheckCommand(t *testing.T) {
	healthy := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/nexo/health/live" && healthy {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	deps, out, _ := testDeps(map[string]string{"NEXO_CONSOLE_ADDRESS": addr}, srv.Client())
	cmd := newRootCmd(deps)
	cmd.SetArgs([]string{"healthcheck"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "ok:")

	healthy = false
	cmd = newRootCmd(deps)
	cmd.SetArgs([]string{"healthcheck"})
	assert.Error(t, cmd.Execute())

	cmd = newRootCmd(deps)
	cmd.SetArgs([]string{"healthcheck", "--url", srv.URL + "/nexo/health/live"})
	assert.Error(t, cmd.Execute(), "explicit URL is used as is")

	u, err := healthURL(":9001", "/nexo/health/live")
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9001/nexo/health/live", u)
	_, err = healthURL("9001", "/x")
	assert.Error(t, err)
}

func TestServerDryRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	deps, out, _ := testDeps(map[string]string{
		"NEXO_ROOT_USER":     "admin",
		"NEXO_ROOT_PASSWORD": "password123",
	}, nil)
	cmd := newRootCmd(deps)
	cmd.SetArgs([]string{"server", "--dry-run", "--address", "127.0.0.1:0", "--console-address", "127.0.0.1:0", dir})
	require.NoError(t, cmd.ExecuteContext(context.Background()))
	assert.Contains(t, out.String(), "configuration OK: mode=snsd")

	// Missing credentials fail validation.
	deps, _, _ = testDeps(nil, nil)
	cmd = newRootCmd(deps)
	cmd.SetArgs([]string{"server", "--dry-run", dir})
	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "root user")

	// Custom assets provider is accepted.
	deps, _, _ = testDeps(map[string]string{"NEXO_ROOT_USER": "admin", "NEXO_ROOT_PASSWORD": "password123"}, nil)
	custom := newServerCmdWith(deps, func() (fs.FS, bool) { return nil, false })
	custom.SetArgs([]string{"--dry-run", "--address", "127.0.0.1:0", "--console-address", "127.0.0.1:0", dir})
	require.NoError(t, custom.ExecuteContext(context.Background()))
}
