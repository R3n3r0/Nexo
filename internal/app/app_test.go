package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/app"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
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
	a, err := app.Build(ctx, testConfig(t), app.WithLogger(slog.New(slog.DiscardHandler)), app.WithStopTimeout(5*time.Second))
	require.NoError(t, err)
	assert.Equal(t, domain.ModeSNSD, a.Mode())
	require.NotNil(t, a.Layout())

	require.NoError(t, a.Start(ctx))
	defer func() { require.NoError(t, a.Stop(ctx)) }()
	assert.NotEqual(t, "127.0.0.1:0", a.S3Addr())
	assert.NotEqual(t, "127.0.0.1:0", a.ConsoleAddr())

	resp, err := http.Get("http://" + a.ConsoleAddr() + "/nexo/health/live")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Anonymous ListBuckets is refused by the policy engine with S3 XML.
	resp, err = http.Get("http://" + a.S3Addr() + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("x-amz-request-id"))
	var s3err httpx.S3ErrorResponse
	require.NoError(t, xml.Unmarshal(body, &s3err))
	assert.Equal(t, "AccessDenied", s3err.Code)
	assert.NotEmpty(t, a.DeploymentID())

	resp, err = http.Get("http://" + a.ConsoleAddr() + "/buckets")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "console not included")
	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// auditRecords returns the audit records logged so far.
func (b *syncBuffer) auditRecords(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(b.buf.Bytes(), []byte("\n")) {
		var rec map[string]any
		if len(line) == 0 || json.Unmarshal(line, &rec) != nil || rec["msg"] != "audit" {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// TestAuditRecordsRejectedRequests is a regression test: the S3 listener
// audits the requests the SigV4 authentication rejects (audit wraps auth).
func TestAuditRecordsRejectedRequests(t *testing.T) {
	ctx := context.Background()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	a, err := app.Build(ctx, testConfig(t), app.WithLogger(logger), app.WithStopTimeout(5*time.Second))
	require.NoError(t, err)
	require.NoError(t, a.Start(ctx))
	defer func() { require.NoError(t, a.Stop(ctx)) }()

	req, err := http.NewRequest(http.MethodGet, "http://"+a.S3Addr()+"/bkt/key", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=NOSUCHKEY/20261008/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00")
	req.Header.Set("X-Amz-Date", "20261008T120000Z")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.GreaterOrEqual(t, resp.StatusCode, http.StatusBadRequest, "the authentication rejects the request")

	var rejected map[string]any
	for _, rec := range logs.auditRecords(t) {
		if rec["bucket"] == "bkt" && rec["object"] == "key" {
			rejected = rec
		}
	}
	require.NotNil(t, rejected, "the rejected request is audited")
	assert.EqualValues(t, resp.StatusCode, rejected["status_code"])
	assert.Empty(t, rejected["access_key"], "no identity was established")
}

func TestBuildErasureModesNotYetServed(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, filepath.Join(dir, "d1"), filepath.Join(dir, "d2"), filepath.Join(dir, "d3"), filepath.Join(dir, "d4"))
	_, err := app.Build(context.Background(), cfg, app.WithLogger(slog.New(slog.DiscardHandler)))
	require.ErrorIs(t, err, domain.ErrNotImplemented)
	assert.Contains(t, err.Error(), "M5")
	assert.NoDirExists(t, filepath.Join(dir, "d1", domain.SystemBucket), "no drive is formatted")
}

func TestBuildPersistsIdentitiesAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	logger := app.WithLogger(slog.New(slog.DiscardHandler))
	a, err := app.Build(ctx, cfg, logger)
	require.NoError(t, err)
	_, err = a.IAM().CreateUser(ctx, iam.CreateUserInput{AccessKey: "reader", SecretKey: "reader-secret", PolicyNames: []string{"readonly"}})
	require.NoError(t, err)
	require.NoError(t, a.Stop(ctx))

	// The encrypted IAM store lives on the drive and is read back.
	b, err := app.Build(ctx, cfg, logger)
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Stop(ctx)) }()
	u, err := b.IAM().GetUser(ctx, "reader")
	require.NoError(t, err)
	assert.Equal(t, []string{"readonly"}, u.PolicyNames)
	assert.FileExists(t, filepath.Join(cfg.Endpoints[0], domain.SystemBucket, "config", "iam", "kdf.json"))
	assert.DirExists(t, filepath.Join(cfg.Endpoints[0], domain.SystemBucket, "config", "policies"))

	// Another root secret cannot open the store.
	cfg.RootPassword = "another-password"
	_, err = app.Build(ctx, cfg, logger)
	require.ErrorIs(t, err, iam.ErrWrongKey)
}

func TestBuildErrors(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.RootPassword = "short"
	_, err := app.Build(ctx, cfg, app.WithLogger(slog.New(slog.DiscardHandler)))
	assert.Error(t, err)

	cfg = testConfig(t, "/data", "http://node1:9000/data")
	_, err = app.Build(ctx, cfg, app.WithLogger(slog.New(slog.DiscardHandler)))
	assert.Error(t, err, "mixed local and URL endpoints")
}

func TestRunStopsOnContextCancel(t *testing.T) {
	a, err := app.Build(context.Background(), testConfig(t), app.WithLogger(slog.New(slog.DiscardHandler)), app.WithStopTimeout(5*time.Second))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + a.ConsoleAddr() + "/nexo/health/live")
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
