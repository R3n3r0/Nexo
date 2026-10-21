package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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

// TestSecretsFromFiles: the credentials and the token can be read from
// files (Docker and Kubernetes secrets); a variable without _FILE wins.
func TestSecretsFromFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
		return path
	}
	deps, _, _ := testDeps(map[string]string{
		"NEXO_WORKER_SECRET_KEY_FILE": write("secret", "s3cr3t\n"),
		"NEXO_WORKER_AUTH_TOKEN_FILE": write("token", "tok\r\n"),
		"NEXO_WORKER_ACCESS_KEY":      "from-env",
		"NEXO_WORKER_ACCESS_KEY_FILE": write("access", "from-file"),
		"NEXO_WORKER_S3_REGION_FILE":  write("region", "ignored"),
	})
	cmd := newRootCmd(deps)
	require.NoError(t, applyEnv(cmd, deps.lookupEnv))
	for flag, want := range map[string]string{"secret-key": "s3cr3t", "auth-token": "tok", "access-key": "from-env", "s3-region": "us-east-1"} {
		got, _ := cmd.Flags().GetString(flag)
		assert.Equal(t, want, got, flag)
	}

	deps, _, _ = testDeps(map[string]string{"NEXO_WORKER_AUTH_TOKEN_FILE": filepath.Join(dir, "missing")})
	assert.ErrorContains(t, applyEnv(newRootCmd(deps), deps.lookupEnv), "NEXO_WORKER_AUTH_TOKEN_FILE")
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

func TestSpeechServices(t *testing.T) {
	tr, tl, err := speechServices(options{})
	require.NoError(t, err)
	assert.Nil(t, tr)
	assert.Nil(t, tl)

	tr, tl, err = speechServices(options{transcribeURL: "http://whisper:8080/inference", transcribeModel: "large-v3",
		translateURL: "http://lt:5000/translate", translateEngine: "libretranslate"})
	require.NoError(t, err)
	assert.Equal(t, "openai:large-v3", tr.Engine())
	assert.Equal(t, "libretranslate", tl.Engine())

	_, tl, err = speechServices(options{translateURL: "https://api.example/v1/chat/completions", translateEngine: "chat", translateModel: "m"})
	require.NoError(t, err)
	assert.Equal(t, "chat:m", tl.Engine())

	_, _, err = speechServices(options{translateURL: "http://x", translateEngine: "deepl"})
	require.ErrorContains(t, err, "--translate-engine")
	_, _, err = speechServices(options{transcribeURL: "whisper:8080"})
	require.ErrorContains(t, err, "--transcribe-url")
}
