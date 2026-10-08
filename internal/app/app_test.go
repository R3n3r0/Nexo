package app

import (
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

func testConfig(t *testing.T, endpoints ...string) config.Static {
	t.Helper()
	cfg := config.Default()
	cfg.Address = "127.0.0.1:0"
	cfg.ConsoleAddress = "127.0.0.1:0"
	cfg.RootUser = "admin"
	cfg.RootPassword = "password123"
	if len(endpoints) == 0 {
		endpoints = []string{filepath.Join(t.TempDir(), "data")}
	}
	cfg.Endpoints = endpoints
	return cfg
}

func TestBuildStartStop(t *testing.T) {
	ctx := context.Background()
	app, err := Build(ctx, testConfig(t), WithLogger(slog.New(slog.DiscardHandler)), WithStopTimeout(5*time.Second))
	require.NoError(t, err)
	assert.Equal(t, domain.ModeSNSD, app.Mode())
	require.NotNil(t, app.Layout())

	require.NoError(t, app.Start(ctx))
	defer func() { require.NoError(t, app.Stop(ctx)) }()
	assert.NotEqual(t, "127.0.0.1:0", app.S3Addr())
	assert.NotEqual(t, "127.0.0.1:0", app.ConsoleAddr())

	resp, err := http.Get("http://" + app.ConsoleAddr() + "/nexo/health/live")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	resp, err = http.Get("http://" + app.S3Addr() + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("x-amz-request-id"))
	var s3err httpx.S3ErrorResponse
	require.NoError(t, xml.Unmarshal(body, &s3err))
	assert.Equal(t, "NotImplemented", s3err.Code)

	resp, err = http.Get("http://" + app.ConsoleAddr() + "/buckets")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "console not included")
	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
}

func TestBuildSNMDMode(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, filepath.Join(dir, "d1"), filepath.Join(dir, "d2"), filepath.Join(dir, "d3"), filepath.Join(dir, "d4"))
	app, err := Build(context.Background(), cfg, WithLogger(slog.New(slog.DiscardHandler)))
	require.NoError(t, err)
	assert.Equal(t, domain.ModeSNMD, app.Mode())
}

func TestBuildErrors(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.RootPassword = "short"
	_, err := Build(ctx, cfg, WithLogger(slog.New(slog.DiscardHandler)))
	assert.Error(t, err)

	cfg = testConfig(t, "/data", "http://node1:9000/data")
	_, err = Build(ctx, cfg, WithLogger(slog.New(slog.DiscardHandler)))
	assert.Error(t, err, "mixed local and URL endpoints")
}

func TestRunStopsOnContextCancel(t *testing.T) {
	app, err := Build(context.Background(), testConfig(t), WithLogger(slog.New(slog.DiscardHandler)), WithStopTimeout(5*time.Second))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + app.ConsoleAddr() + "/nexo/health/live")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
