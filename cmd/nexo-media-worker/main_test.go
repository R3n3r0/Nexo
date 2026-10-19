package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/mediaworker/openh264"
)

func testDeps(env map[string]string) (cmdDeps, *bytes.Buffer, *bytes.Buffer) {
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	return cmdDeps{
		stdout: out, stderr: errBuf, client: http.DefaultClient,
		lookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	}, out, errBuf
}

func execute(t *testing.T, env map[string]string, args ...string) (string, error) {
	t.Helper()
	deps, out, _ := testDeps(env)
	cmd := newRootCmd(deps)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestVersion(t *testing.T) {
	out, err := execute(t, nil, "version")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "nexo-media-worker dev"), out)
	assert.Contains(t, out, "AGPL-3.0-only")
}

func TestRequiredSettings(t *testing.T) {
	dir := t.TempDir()
	_, err := execute(t, map[string]string{"NEXO_WORKER_DATA_DIR": dir})
	require.ErrorContains(t, err, "--auth-token")
	_, err = execute(t, map[string]string{"NEXO_WORKER_DATA_DIR": dir, "NEXO_WORKER_AUTH_TOKEN": "t"})
	require.ErrorContains(t, err, "S3 endpoint")
	_, err = execute(t, map[string]string{"NEXO_WORKER_DATA_DIR": dir, "NEXO_WORKER_AUTH_TOKEN": "t", "NEXO_WORKER_S3_ENDPOINT": "http://127.0.0.1:9000"})
	require.ErrorContains(t, err, "access key")
	_, err = execute(t, map[string]string{"NEXO_WORKER_CONCURRENCY": "many"})
	require.ErrorContains(t, err, "NEXO_WORKER_CONCURRENCY")
	_, err = execute(t, map[string]string{"NEXO_WORKER_AUTH_TOKEN": "t"}, "--log-format", "xml")
	require.ErrorContains(t, err, "--log-format")
}

// TestFlagsOverrideEnvironment: a flag given on the command line wins over
// its environment variable.
func TestFlagsOverrideEnvironment(t *testing.T) {
	deps, _, _ := testDeps(map[string]string{"NEXO_WORKER_ADDRESS": ":1", "NEXO_WORKER_S3_REGION": "eu-south-1"})
	cmd := newRootCmd(deps)
	require.NoError(t, cmd.ParseFlags([]string{"--address", ":2"}))
	require.NoError(t, applyEnv(cmd, deps.lookupEnv))
	address, _ := cmd.Flags().GetString("address")
	region, _ := cmd.Flags().GetString("s3-region")
	assert.Equal(t, ":2", address)
	assert.Equal(t, "eu-south-1", region)
}

func TestMissingLibrary(t *testing.T) {
	_, err := execute(t, map[string]string{
		"NEXO_WORKER_DATA_DIR": t.TempDir(), "NEXO_WORKER_AUTH_TOKEN": "t", "NEXO_WORKER_S3_ENDPOINT": "http://127.0.0.1:9000",
		"NEXO_WORKER_ACCESS_KEY": "a", "NEXO_WORKER_SECRET_KEY": "s", "NEXO_WORKER_OPENH264": "/nonexistent/libopenh264.so",
	})
	require.ErrorContains(t, err, "openh264")
}

func TestHealthcheck(t *testing.T) {
	healthy := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" || !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","pending":3}`))
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)

	out, err := execute(t, nil, "healthcheck", "--address", ":"+port)
	require.NoError(t, err)
	assert.Equal(t, "ok (3 pending)\n", out)
	out, err = execute(t, map[string]string{"NEXO_WORKER_ADDRESS": "127.0.0.1:" + port}, "healthcheck")
	require.NoError(t, err, out)
	healthy = false
	_, err = execute(t, nil, "healthcheck", "--address", ":"+port)
	require.Error(t, err)
	_, err = execute(t, nil, "healthcheck", "--address", "nope")
	require.ErrorContains(t, err, "--address")
}

func TestFetchCommand(t *testing.T) {
	name, _, ok := openh264.Platform()
	if !ok {
		t.Skip("no Cisco build for this platform")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not bzip2"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	_, err := execute(t, nil, "fetch-openh264", "--dir", dir, "--openh264-download-url", srv.URL+"/")
	require.Error(t, err, "a download that is not the library")
	assert.NoFileExists(t, filepath.Join(dir, name))
}
