package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

func intPtr(n int) *int { return &n }

// covers: S-BKT-08, S-OBJ-12, TS-U-12
func TestSDKClientObjectLock(t *testing.T) {
	ctx := context.Background()
	c := newSDKEnv(t).client
	require.NoError(t, c.MakeBucket(ctx, "plain", cli.MakeBucketOptions{}))
	cfg, err := c.GetObjectLock(ctx, "plain")
	require.NoError(t, err)
	assert.False(t, cfg.Enabled, "a bucket without Object Lock")

	require.NoError(t, c.MakeBucket(ctx, "vault", cli.MakeBucketOptions{ObjectLock: true}))
	v, err := c.GetVersioning(ctx, "vault")
	require.NoError(t, err)
	assert.Equal(t, cli.VersioningEnabled, v, "Object Lock implies versioning")
	cfg, err = c.GetObjectLock(ctx, "vault")
	require.NoError(t, err)
	assert.Equal(t, cli.ObjectLockConfig{Enabled: true}, *cfg)
	def := domain.DefaultRetention{Mode: domain.RetentionGovernance, Days: 1}
	require.NoError(t, c.SetObjectLock(ctx, "vault", def))
	cfg, err = c.GetObjectLock(ctx, "vault")
	require.NoError(t, err)
	assert.Equal(t, def, cfg.Default)
	assert.ErrorIs(t, c.SetObjectLock(ctx, "vault", domain.DefaultRetention{Mode: domain.RetentionGovernance, Days: 1, Years: 1}), cli.ErrUsage)
	assert.ErrorIs(t, c.SetObjectLock(ctx, "plain", def), domain.ErrConflict, "only at creation (InvalidBucketState)")

	// The default retention applies to the new versions.
	put, err := c.Put(ctx, "vault", "doc", strings.NewReader("data"), cli.PutOptions{Size: 4})
	require.NoError(t, err)
	info, err := c.Stat(ctx, "vault", "doc", cli.StatOptions{})
	require.NoError(t, err)
	assert.Equal(t, domain.RetentionGovernance, info.Retention.Mode)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), info.Retention.RetainUntil, time.Hour)
	ref := cli.ObjectRef{Bucket: "vault", Key: "doc", VersionID: put.VersionID}
	ret, err := c.GetRetention(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, info.Retention, ret)

	// Shortening a GOVERNANCE retention needs the bypass.
	shorter := domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	err = c.PutRetention(ctx, ref, shorter, false)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	assert.Equal(t, cli.ExitAccess, cli.ExitCode(err))
	require.NoError(t, c.PutRetention(ctx, ref, shorter, true))
	ret, err = c.GetRetention(ctx, ref)
	require.NoError(t, err)
	assert.True(t, shorter.RetainUntil.Equal(ret.RetainUntil), "%v != %v", shorter.RetainUntil, ret.RetainUntil)

	// Legal hold.
	hold, err := c.GetLegalHold(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, domain.LegalHoldOff, hold)
	require.NoError(t, c.PutLegalHold(ctx, ref, domain.LegalHoldOn))
	info, err = c.Stat(ctx, "vault", "doc", cli.StatOptions{VersionID: put.VersionID})
	require.NoError(t, err)
	assert.Equal(t, domain.LegalHoldOn, info.LegalHold)

	locked, err := cli.LockedVersions(ctx, c, "vault", time.Now(), 0)
	require.NoError(t, err)
	require.Len(t, locked, 1)
	assert.Equal(t, put.VersionID, locked[0].VersionID)
	assert.Contains(t, locked[0].Reason(time.Now()), "legal hold")
	none, err := cli.LockedVersions(ctx, c, "plain", time.Now(), 0)
	require.NoError(t, err)
	assert.Empty(t, none)

	// A protected version is not removed, even with the bypass (legal
	// hold); once released, the bypass lifts the GOVERNANCE retention.
	res, err := c.Remove(ctx, "vault", []cli.ObjectVersion{{Key: "doc", VersionID: put.VersionID}}, cli.DeleteOptions{BypassGovernance: true})
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.ErrorIs(t, res[0].Err, domain.ErrAccessDenied)
	require.NoError(t, c.PutLegalHold(ctx, ref, domain.LegalHoldOff))
	res, err = c.Remove(ctx, "vault", []cli.ObjectVersion{{Key: "doc", VersionID: put.VersionID}}, cli.DeleteOptions{})
	require.NoError(t, err)
	assert.Equal(t, cli.ExitAccess, cli.ExitCode(res[0].Err), "GOVERNANCE without bypass")
	res, err = c.Remove(ctx, "vault", []cli.ObjectVersion{{Key: "doc", VersionID: put.VersionID}}, cli.DeleteOptions{BypassGovernance: true})
	require.NoError(t, err)
	require.NoError(t, res[0].Err)

	// Retention needs a bucket with Object Lock.
	_, err = c.Put(ctx, "plain", "k", strings.NewReader("x"), cli.PutOptions{Size: 1})
	require.NoError(t, err)
	err = c.PutLegalHold(ctx, cli.ObjectRef{Bucket: "plain", Key: "k"}, domain.LegalHoldOn)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// covers: S-BKT-07, S-BKT-06, S-BKT-10
func TestSDKClientBucketConfigurations(t *testing.T) {
	ctx := context.Background()
	e := newSDKEnv(t)
	c := e.client
	require.NoError(t, c.MakeBucket(ctx, "conf", cli.MakeBucketOptions{}))

	t.Run("lifecycle", func(t *testing.T) {
		got, err := c.GetLifecycle(ctx, "conf")
		require.NoError(t, err)
		assert.Empty(t, got.Rules)
		gt := int64(1024)
		enabled, disabled := true, false
		want := cli.LifecycleConfiguration{Rules: []cli.LifecycleRule{
			{ID: "logs", Enabled: &enabled, Filter: &cli.LifecycleFilter{Prefix: "logs/", Tags: map[string]string{"tier": "cold"}, SizeGreaterThan: &gt},
				Expiration: &cli.LifecycleExpiration{Days: intPtr(30)}},
			{ID: "dated", Enabled: &disabled, Expiration: &cli.LifecycleExpiration{Date: "2030-01-01"}},
			{ID: "versions", Enabled: &enabled, Filter: &cli.LifecycleFilter{Prefix: "docs/"},
				NoncurrentExpiration:  &cli.LifecycleNoncurrentExpiration{NoncurrentDays: intPtr(7), NewerNoncurrentVersions: intPtr(2)},
				AbortIncompleteUpload: &cli.LifecycleAbortUpload{DaysAfterInitiation: intPtr(3)}},
			{ID: "markers", Enabled: &enabled, Expiration: &cli.LifecycleExpiration{ExpiredObjectDeleteMarker: true}},
		}}
		require.NoError(t, c.PutLifecycle(ctx, "conf", want))
		got, err = c.GetLifecycle(ctx, "conf")
		require.NoError(t, err)
		assert.Equal(t, want, *got)

		// The XML form round-trips to the same configuration.
		doc, err := cli.EncodeLifecycleXML(want)
		require.NoError(t, err)
		parsed, err := cli.ParseLifecycle(doc)
		require.NoError(t, err)
		assert.Equal(t, want, parsed)

		err = c.PutLifecycle(ctx, "conf", cli.LifecycleConfiguration{Rules: []cli.LifecycleRule{{ID: "zero", Expiration: &cli.LifecycleExpiration{Days: intPtr(0)}}}})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		require.NoError(t, c.PutLifecycle(ctx, "conf", cli.LifecycleConfiguration{}))
		got, err = c.GetLifecycle(ctx, "conf")
		require.NoError(t, err)
		assert.Empty(t, got.Rules, "no rules removes the configuration")
	})

	t.Run("cors", func(t *testing.T) {
		got, err := c.GetCORS(ctx, "conf")
		require.NoError(t, err)
		assert.Empty(t, got.Rules)
		want := cli.CORSConfiguration{Rules: []cli.CORSRule{
			{ID: "web", AllowedOrigins: []string{"https://app.example.com"}, AllowedMethods: []string{"GET", "PUT"},
				AllowedHeaders: []string{"*"}, ExposeHeaders: []string{"ETag"}, MaxAgeSeconds: intPtr(600)},
		}}
		require.NoError(t, c.PutCORS(ctx, "conf", want))
		got, err = c.GetCORS(ctx, "conf")
		require.NoError(t, err)
		assert.Equal(t, want, *got)
		require.NoError(t, c.PutCORS(ctx, "conf", cli.CORSConfiguration{}))
		got, err = c.GetCORS(ctx, "conf")
		require.NoError(t, err)
		assert.Empty(t, got.Rules)
	})

	t.Run("notifications", func(t *testing.T) {
		sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		t.Cleanup(sink.Close)
		admin, err := cli.NewAdminFactory(cli.AdminFactoryDeps{}).Client(ctx,
			cli.Alias{Name: "local", URL: e.srv.S3URL, AdminURL: e.srv.ConsoleURL, AccessKey: e.srv.RootUser, SecretKey: e.srv.RootPassword}, cli.ClientOptions{})
		require.NoError(t, err)
		_, err = admin.SetConfig(ctx, "notify_webhook", map[string]string{"enable": "on", "endpoint": sink.URL})
		require.NoError(t, err)
		var arn string
		require.Eventually(t, func() bool {
			targets, err := admin.ListNotifyTargets(ctx)
			if err != nil || len(targets) == 0 {
				return false
			}
			arn = targets[0].ARN
			return true
		}, 10*time.Second, 20*time.Millisecond)

		got, err := c.GetNotifications(ctx, "conf")
		require.NoError(t, err)
		assert.Empty(t, got.Subscriptions)
		want := cli.BucketNotifications{Subscriptions: []cli.EventSubscription{
			{ID: "uploads", ARN: arn, Events: []string{"s3:ObjectCreated:*"}, Prefix: "in/", Suffix: ".jpg"},
		}}
		require.NoError(t, c.PutNotifications(ctx, "conf", want))
		got, err = c.GetNotifications(ctx, "conf")
		require.NoError(t, err)
		assert.Equal(t, want, *got)
		err = c.PutNotifications(ctx, "conf", cli.BucketNotifications{Subscriptions: []cli.EventSubscription{
			{ARN: "arn:nexo:sqs:us-east-1:missing:webhook", Events: []string{"s3:ObjectCreated:*"}},
		}})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, "an unknown target")
		require.NoError(t, c.PutNotifications(ctx, "conf", cli.BucketNotifications{}))
		got, err = c.GetNotifications(ctx, "conf")
		require.NoError(t, err)
		assert.Empty(t, got.Subscriptions)
	})
}

// covers: S-OBJ-09
func TestSDKClientChecksums(t *testing.T) {
	ctx := context.Background()
	c := newSDKEnv(t).client
	require.NoError(t, c.MakeBucket(ctx, "sums", cli.MakeBucketOptions{}))
	data := random(t, 11<<20)
	sum := sha256.Sum256(data)
	tests := []struct {
		name     string
		algo     domain.ChecksumAlgorithm
		size     int
		partSize int64
	}{
		{name: "sha256 single", algo: domain.ChecksumSHA256, size: 1 << 10},
		{name: "crc32c single", algo: domain.ChecksumCRC32C, size: 1 << 10},
		{name: "crc64nvme single", algo: domain.ChecksumCRC64NVME, size: 1 << 10},
		{name: "sha256 multipart", algo: domain.ChecksumSHA256, size: len(data), partSize: cli.MinPartSize},
		{name: "crc32 multipart", algo: domain.ChecksumCRC32, size: len(data), partSize: cli.MinPartSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := strings.ReplaceAll(tt.name, " ", "-")
			res, err := c.Put(ctx, "sums", key, bytes.NewReader(data[:tt.size]), cli.PutOptions{Size: int64(tt.size), PartSize: tt.partSize, Checksum: tt.algo})
			require.NoError(t, err)
			assert.Equal(t, tt.algo, res.Checksum.Algorithm)
			assert.NotEmpty(t, res.Checksum.Value)
			info, err := c.Stat(ctx, "sums", key, cli.StatOptions{Checksum: true})
			require.NoError(t, err)
			assert.Equal(t, res.Checksum.Algorithm, info.Checksum.Algorithm)
			assert.Equal(t, res.Checksum.Value, info.Checksum.Value)
			if tt.algo == domain.ChecksumSHA256 && tt.partSize == 0 {
				one := sha256.Sum256(data[:tt.size])
				assert.Equal(t, base64.StdEncoding.EncodeToString(one[:]), info.Checksum.Value)
			}
			plain, err := c.Stat(ctx, "sums", key, cli.StatOptions{})
			require.NoError(t, err)
			assert.True(t, plain.Checksum.IsZero(), "the checksum only with the checksum mode")
		})
	}
	// A server-side copy with a new checksum.
	res, err := c.Copy(ctx, cli.ObjectRef{Bucket: "sums", Key: "sha256-multipart"}, cli.ObjectRef{Bucket: "sums", Key: "copy"}, cli.CopyOptions{Checksum: domain.ChecksumSHA256})
	require.NoError(t, err)
	assert.Equal(t, domain.ChecksumSHA256, res.Checksum.Algorithm)
	assert.Equal(t, base64.StdEncoding.EncodeToString(sum[:]), res.Checksum.Value)
}

// newSSEEnv starts a TLS server with the builtin KMS (SSE-C needs TLS)
// and returns a client of it, and a client dumping its requests to debug.
func newSSEEnv(t *testing.T, debug io.Writer) (cli.S3Client, cli.S3Client) {
	t.Helper()
	srv := testutil.StartServer(t, testutil.WithTLS(), testutil.WithKMS())
	tr, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok)
	tr = tr.Clone()
	tr.TLSClientConfig = srv.TLS.Clone()
	factory := cli.NewS3Factory(cli.S3FactoryDeps{Transport: tr})
	alias := cli.Alias{Name: "tls", URL: srv.S3URL, AccessKey: srv.RootUser, SecretKey: srv.RootPassword, Path: "on"}
	c, err := factory.Client(context.Background(), alias, cli.ClientOptions{})
	require.NoError(t, err)
	dc, err := factory.Client(context.Background(), alias, cli.ClientOptions{Debug: debug})
	require.NoError(t, err)
	return c, dc
}

// covers: S-OBJ-13, TS-U-13
func TestSDKClientServerSideEncryption(t *testing.T) {
	ctx := context.Background()
	var debug bytes.Buffer
	c, debugClient := newSSEEnv(t, &debug)
	require.NoError(t, c.MakeBucket(ctx, "enc", cli.MakeBucketOptions{}))
	key, err := cli.ParseSSECustomerKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	require.NoError(t, err)
	other, err := cli.ParseSSECustomerKey(strings.Repeat("ab", 32))
	require.NoError(t, err)
	ssec := domain.SSERequest{Type: domain.SSEC, CustomerKey: key}
	data := random(t, 11<<20)

	tests := []struct {
		name     string
		sse      domain.SSERequest
		size     int
		partSize int64
		want     domain.EncryptionInfo
	}{
		{name: "sse-s3", sse: domain.SSERequest{Type: domain.SSES3}, size: 100, want: domain.EncryptionInfo{Type: domain.SSES3}},
		{name: "sse-kms", sse: domain.SSERequest{Type: domain.SSEKMS, KMSKeyID: testutil.KMSKeyName}, size: 100,
			want: domain.EncryptionInfo{Type: domain.SSEKMS, KMSKeyID: testutil.KMSKeyName}},
		{name: "sse-c", sse: ssec, size: 100, want: domain.EncryptionInfo{Type: domain.SSEC, CustomerKeyMD5: key.KeyMD5}},
		{name: "sse-c-multipart", sse: ssec, size: len(data), partSize: cli.MinPartSize, want: domain.EncryptionInfo{Type: domain.SSEC, CustomerKeyMD5: key.KeyMD5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.Put(ctx, "enc", tt.name, bytes.NewReader(data[:tt.size]), cli.PutOptions{Size: int64(tt.size), PartSize: tt.partSize, SSE: tt.sse})
			require.NoError(t, err)
			read := cli.StatOptions{}
			if tt.sse.Type == domain.SSEC {
				_, err := c.Stat(ctx, "enc", tt.name, cli.StatOptions{})
				assert.ErrorIs(t, err, domain.ErrInvalidArgument, "an SSE-C object needs its key")
				_, err = c.Stat(ctx, "enc", tt.name, cli.StatOptions{SSE: domain.SSERequest{Type: domain.SSEC, CustomerKey: other}})
				assert.Equal(t, cli.ExitAccess, cli.ExitCode(err), "another key is denied")
				read.SSE = ssec
			}
			info, err := c.Stat(ctx, "enc", tt.name, read)
			require.NoError(t, err)
			assert.Equal(t, tt.want.Type, info.Encryption.Type)
			assert.Equal(t, tt.want.KMSKeyID, info.Encryption.KMSKeyID)
			body, _, err := c.Get(ctx, "enc", tt.name, cli.GetOptions{SSE: read.SSE, Range: &cli.ByteRange{Offset: 10, Length: 50}})
			require.NoError(t, err)
			got, err := io.ReadAll(body)
			require.NoError(t, body.Close())
			require.NoError(t, err)
			assert.Equal(t, data[10:60], got, "range of an encrypted object")
		})
	}

	// A server-side copy of an SSE-C source re-encrypted with SSE-S3; the
	// keys never appear in the debug dump.
	_, err = debugClient.Copy(ctx, cli.ObjectRef{Bucket: "enc", Key: "sse-c"}, cli.ObjectRef{Bucket: "enc", Key: "copy"}, cli.CopyOptions{SourceSSE: ssec, SSE: domain.SSERequest{Type: domain.SSES3}})
	require.NoError(t, err)
	assert.Contains(t, debug.String(), "X-Amz-Copy-Source-Server-Side-Encryption-Customer-Key: REDACTED")
	assert.NotContains(t, debug.String(), base64.StdEncoding.EncodeToString(key.Key))
	info, err := c.Stat(ctx, "enc", "copy", cli.StatOptions{})
	require.NoError(t, err)
	assert.Equal(t, domain.SSES3, info.Encryption.Type)
	assert.Equal(t, data[:100], readAll(t, c, "enc", "copy", cli.GetOptions{}))
}

// covers: C-ADM-* (admin kms, notify targets)
func TestAdminClientKMSAndTargets(t *testing.T) {
	srv := testutil.StartServer(t, testutil.WithKMS())
	ctx := context.Background()
	client, err := cli.NewAdminFactory(cli.AdminFactoryDeps{}).Client(ctx,
		cli.Alias{Name: "local", URL: srv.S3URL, AdminURL: srv.ConsoleURL, AccessKey: srv.RootUser, SecretKey: srv.RootPassword}, cli.ClientOptions{})
	require.NoError(t, err)
	st, err := client.KMSStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, testutil.KMSKeyName, st.DefaultKeyID)
	_, err = client.CreateKMSKey(ctx, "cli-key")
	require.NoError(t, err)
	_, err = client.CreateKMSKey(ctx, "cli-key")
	assert.ErrorIs(t, err, domain.ErrConflict)
	keys, err := client.ListKMSKeys(ctx, "cli")
	require.NoError(t, err)
	require.Len(t, keys, 1)
	targets, err := client.ListNotifyTargets(ctx)
	require.NoError(t, err)
	assert.Empty(t, targets)
	_, err = client.TestNotifyTarget(ctx, "_:webhook")
	assert.Equal(t, cli.ExitNotFound, cli.ExitCode(err))

	_, err = client.GetBucket(ctx, "missing")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	_, err = client.ApplyBucketLifecycle(ctx, "missing")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)

	noKMS := testutil.StartServer(t)
	plain, err := cli.NewAdminFactory(cli.AdminFactoryDeps{}).Client(ctx,
		cli.Alias{Name: "plain", URL: noKMS.S3URL, AdminURL: noKMS.ConsoleURL, AccessKey: noKMS.RootUser, SecretKey: noKMS.RootPassword}, cli.ClientOptions{})
	require.NoError(t, err)
	st, err = plain.KMSStatus(ctx)
	if err == nil {
		assert.False(t, st.Configured, "no KMS configured")
	} else {
		assert.ErrorIs(t, err, domain.ErrNotImplemented)
	}
	_ = nexoadmin.KMSBackendBuiltin
}
