package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/app"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/object"
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

// TestBuildSNMD: a single node with four drives runs the erasure-coded
// layer; the system documents (IAM, configuration) are replicated on the
// drives of the erasure set and survive a restart.
func TestBuildSNMD(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := testConfig(t, filepath.Join(dir, "d1"), filepath.Join(dir, "d2"), filepath.Join(dir, "d3"), filepath.Join(dir, "d4"))
	logger := app.WithLogger(slog.New(slog.DiscardHandler))
	a, err := app.Build(ctx, cfg, logger, app.WithStopTimeout(5*time.Second))
	require.NoError(t, err)
	assert.Equal(t, domain.ModeSNMD, a.Mode())
	assert.Equal(t, "local", a.NodeID())
	require.NoError(t, a.Start(ctx))
	assert.True(t, a.IsLeader(), "a single node leads its background jobs")

	_, err = a.IAM().CreateUser(ctx, iam.CreateUserInput{AccessKey: "reader", SecretKey: "reader-secret", PolicyNames: []string{"readonly"}})
	require.NoError(t, err)
	_, err = a.Buckets().Create(ctx, bucket.CreateInput{Name: "photos"})
	require.NoError(t, err)
	_, err = a.Objects().PutObject(ctx, object.PutObjectInput{Bucket: "photos", Key: "a.txt", Reader: strings.NewReader("hello"), Size: 5})
	require.NoError(t, err)
	for _, d := range []string{"d1", "d2", "d3", "d4"} {
		assert.FileExists(t, filepath.Join(dir, d, domain.SystemBucket, "format.json"))
		assert.FileExists(t, filepath.Join(dir, d, domain.SystemBucket, "config", "iam", "kdf.json"), "replicated on every drive of the set")
		assert.DirExists(t, filepath.Join(dir, d, "photos", "a.txt"))
	}
	require.NoError(t, a.Stop(ctx))

	b, err := app.Build(ctx, cfg, logger, app.WithStopTimeout(5*time.Second))
	require.NoError(t, err)
	require.NoError(t, b.Start(ctx))
	defer func() { require.NoError(t, b.Stop(ctx)) }()
	assert.Equal(t, a.DeploymentID(), b.DeploymentID())
	u, err := b.IAM().GetUser(ctx, "reader")
	require.NoError(t, err)
	assert.Equal(t, []string{"readonly"}, u.PolicyNames)
	r, err := b.Objects().GetObject(ctx, "photos", "a.txt", object.GetObjectOptions{})
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	_ = r.Close()
	assert.Equal(t, "hello", string(data))

	// Another root secret cannot open the IAM documents.
	cfg.RootPassword = "another-password"
	_, err = app.Build(ctx, cfg, logger)
	require.ErrorIs(t, err, iam.ErrWrongKey)
}

// TestBuildExpandsPools: restarted with a second pool (docs/03 §3.8), a
// deployment creates its buckets on the new pool before serving, so that
// the new objects placed there find them; the old objects stay readable
// and the buckets stay listed.
func TestBuildExpandsPools(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := filepath.Join(dir, "a{1...4}")
	cfg := testConfig(t, first)
	logger := app.WithLogger(slog.New(slog.DiscardHandler))
	a, err := app.Build(ctx, cfg, logger, app.WithStopTimeout(5*time.Second))
	require.NoError(t, err)
	_, err = a.Buckets().Create(ctx, bucket.CreateInput{Name: "photos"})
	require.NoError(t, err)
	_, err = a.Objects().PutObject(ctx, object.PutObjectInput{Bucket: "photos", Key: "old", Reader: strings.NewReader("old"), Size: 3})
	require.NoError(t, err)
	require.NoError(t, a.Stop(ctx))

	cfg.Endpoints = []string{first, filepath.Join(dir, "b{1...8}")}
	b, err := app.Build(ctx, cfg, logger, app.WithStopTimeout(5*time.Second))
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Stop(ctx)) }()
	for i := 1; i <= 8; i++ {
		assert.DirExists(t, filepath.Join(dir, "b"+strconv.Itoa(i), "photos"), "the bucket exists on every drive of the new pool")
	}
	buckets, err := b.Objects().ListBuckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, "photos", buckets[0].Name)
	r, err := b.Objects().GetObject(ctx, "photos", "old", object.GetObjectOptions{})
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	_ = r.Close()
	assert.Equal(t, "old", string(data))

	for i := range 20 {
		key := "new-" + strconv.Itoa(i)
		_, err := b.Objects().PutObject(ctx, object.PutObjectInput{Bucket: "photos", Key: key, Reader: strings.NewReader(key), Size: int64(len(key))})
		if errors.Is(err, domain.ErrStorageFull) {
			// Both pools live on the file system of the temporary
			// directory: above 95% of use, no pool takes new objects.
			t.Skipf("the file system of the temporary directory is nearly full: %v", err)
		}
		require.NoError(t, err, key)
	}
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
