//go:build upgrade

package upgrade_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // the SSE-C key digest of the S3 API
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The buckets of the data set.
const (
	bucketData   = "data"   // versioned: versions, delete marker, SSE, multipart, tags, lifecycle
	bucketSecure = "secure" // default encryption SSE-S3
	bucketLocked = "locked" // Object Lock: retention and legal hold
	bucketPublic = "public" // bucket policy (anonymous reads) and CORS
	bucketEvents = "events" // notifications to the webhook
)

// The IAM entities of the data set.
const (
	readerUser   = "upgrade-reader"
	readerSecret = "upgrade-reader-secret"
	readerPolicy = "upgrade-read-data"
)

// partSize is the minimum part size of S3 (every part but the last).
const partSize = 5 << 20

// payload returns n deterministic bytes named by name.
func payload(name string, n int) []byte {
	sum := sha256.Sum256([]byte(name))
	rng := rand.New(rand.NewPCG(binary.BigEndian.Uint64(sum[:8]), binary.BigEndian.Uint64(sum[8:16]))) //nolint:gosec // test data
	out := make([]byte, n)
	for i := 0; i < n; i += 8 {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], rng.Uint64())
		copy(out[i:], b[:])
	}
	return out
}

// digest is the hex sha256 of data.
func digest(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// version is a version of an object of the data set and what it holds.
type version struct {
	bucket, key, id string
	sha             string
	size            int64
	etag            string
	sse             types.ServerSideEncryption // "" for none
	kmsKey          string
	ssec            bool
	metadata        map[string]string
}

// dataset is what the deployment holds: every version that must read back
// (by id), the latest content of the keys, and the other state written
// along the way.
type dataset struct {
	versions []version
	// latest is the sha256 of the latest version of "bucket/key", "" when
	// a delete marker (or nothing) is the latest.
	latest map[string]string
	// marker is the delete marker of data/doc.
	marker string
	// mp is the multipart object data/mp: its composite CRC32C and the
	// bytes around the boundary of its first two parts.
	mpChecksum string
	mpBoundary []byte
	// pending is the multipart upload data/pending left open with two
	// parts.
	pendingID    string
	pendingParts [][]byte
	pendingETags []string
	// held is the version of locked/held under governance retention
	// until heldUntil; legal the version of locked/legal under legal hold.
	held      string
	heldUntil time.Time
	legal     string
	// keep is the version of data/keep carrying the tags.
	keep string
	// childKey and childSecret are an access key of the reader user.
	childKey, childSecret string
	// ssecKey is the SSE-C key of the SSE-C versions.
	ssecKey []byte
}

func newDataset() *dataset {
	return &dataset{latest: map[string]string{}, ssecKey: bytes.Repeat([]byte{0x5c}, 32)}
}

// ssec returns the SSE-C parameters of the key of the data set.
func (ds *dataset) ssec() (algo, key, keyMD5 *string) {
	sum := md5.Sum(ds.ssecKey) //nolint:gosec // the SSE-C key digest of the S3 API
	return aws.String("AES256"), aws.String(base64.StdEncoding.EncodeToString(ds.ssecKey)),
		aws.String(base64.StdEncoding.EncodeToString(sum[:]))
}

// put writes data to bucket/key with the options of v (encryption,
// metadata) and records the version.
func (ds *dataset) put(t *testing.T, c *s3.Client, v version, data []byte) version {
	t.Helper()
	in := &s3.PutObjectInput{Bucket: aws.String(v.bucket), Key: aws.String(v.key), Body: bytes.NewReader(data), Metadata: v.metadata}
	if v.sse != "" {
		in.ServerSideEncryption = v.sse
		if v.kmsKey != "" {
			in.SSEKMSKeyId = aws.String(v.kmsKey)
		}
	}
	if v.ssec {
		in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5 = ds.ssec()
	}
	out, err := c.PutObject(context.Background(), in)
	require.NoError(t, err, "put %s/%s", v.bucket, v.key)
	v.id, v.etag = aws.ToString(out.VersionId), aws.ToString(out.ETag)
	v.sha, v.size = digest(data), int64(len(data))
	ds.versions = append(ds.versions, v)
	ds.latest[v.bucket+"/"+v.key] = v.sha
	return v
}

// forget removes the version id of bucket/key from the expectations.
func (ds *dataset) forget(bucket, key, id string) {
	ds.versions = slices.DeleteFunc(ds.versions, func(v version) bool { return v.bucket == bucket && v.key == key && v.id == id })
}

// webhook is the endpoint of the bucket notifications: it records the keys
// of the events it accepts and answers 503 while offline.
type webhook struct {
	srv     *httptest.Server
	offline atomic.Bool
	mu      sync.Mutex
	keys    map[string]int
}

// webhookToken authenticates Nexo to the webhook.
const webhookToken = "upgrade-webhook-token"

func newWebhook(t *testing.T) *webhook {
	t.Helper()
	w := &webhook{keys: map[string]int{}}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w.offline.Load() {
			http.Error(rw, "offline", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+webhookToken {
			http.Error(rw, "no token", http.StatusUnauthorized)
			return
		}
		var body struct {
			Records []struct {
				S3 struct {
					Object struct {
						Key string `json:"key"`
					} `json:"object"`
				} `json:"s3"`
			} `json:"Records"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		w.mu.Lock()
		for _, rec := range body.Records {
			w.keys[rec.S3.Object.Key]++
		}
		w.mu.Unlock()
	}))
	t.Cleanup(w.srv.Close)
	return w
}

// received reports whether an event of key was delivered.
func (w *webhook) received(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.keys[key] > 0
}

// waitEvent waits until an event of key is delivered.
func (w *webhook) waitEvent(t *testing.T, key string) {
	t.Helper()
	waitFor(t, 60*time.Second, "event of "+key, func() error {
		if w.received(key) {
			return nil
		}
		return fmt.Errorf("no event of %s yet", key)
	})
}

// webhookARN is the ARN of the default webhook target.
func webhookARN() string { return "arn:nexo:sqs:" + region + ":_:webhook" }

// writeDataset writes the data set with the root client c and the Admin
// API client admin of the deployment; with a webhook it also configures
// the notifications of bucketEvents and leaves an event queued while the
// webhook is offline.
func writeDataset(t *testing.T, c *s3.Client, admin nexoadmin.AdminClient, hook *webhook) *dataset {
	t.Helper()
	ctx := context.Background()
	ds := newDataset()
	for _, b := range []string{bucketData, bucketSecure, bucketPublic} {
		_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(b)})
		require.NoError(t, err, "create %s", b)
	}
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketLocked), ObjectLockEnabledForBucket: aws.Bool(true)})
	require.NoError(t, err)
	_, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(bucketData),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	require.NoError(t, err)

	// Versions (inline, erasure-coded) and a delete marker.
	ds.put(t, c, version{bucket: bucketData, key: "doc"}, payload("doc-v1", 1000))
	ds.put(t, c, version{bucket: bucketData, key: "doc"}, payload("doc-v2", 300<<10))
	ds.put(t, c, version{bucket: bucketData, key: "doc"}, payload("doc-v3", 64))
	del, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucketData), Key: aws.String("doc")})
	require.NoError(t, err)
	require.True(t, aws.ToBool(del.DeleteMarker))
	ds.marker, ds.latest[bucketData+"/doc"] = aws.ToString(del.VersionId), ""

	// Metadata and tags.
	ds.keep = ds.put(t, c, version{bucket: bucketData, key: "keep", metadata: map[string]string{"origin": "old"}}, payload("keep", 200<<10)).id
	_, err = c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: aws.String(bucketData), Key: aws.String("keep"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("team"), Value: aws.String("storage")}}}})
	require.NoError(t, err)
	_, err = c.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String(bucketData),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("env"), Value: aws.String("upgrade")}}}})
	require.NoError(t, err)

	// Server-side encryption: SSE-S3, SSE-KMS (builtin KMS), SSE-C.
	ds.put(t, c, version{bucket: bucketData, key: "sse-s3", sse: types.ServerSideEncryptionAes256}, payload("sse-s3", 100<<10))
	ds.put(t, c, version{bucket: bucketData, key: "sse-kms", sse: types.ServerSideEncryptionAwsKms, kmsKey: kmsKeyName}, payload("sse-kms", 100<<10))
	ds.put(t, c, version{bucket: bucketData, key: "sse-c", ssec: true}, payload("sse-c", 100<<10))

	// A completed multipart upload with CRC32C checksums, and one left
	// open with two parts.
	ds.multipart(t, c, bucketData, "mp", [][]byte{payload("mp-1", partSize), payload("mp-2", partSize), payload("mp-3", 1<<20)}, "")
	ds.openUpload(t, c, bucketData, "pending", [][]byte{payload("pending-1", partSize), payload("pending-2", partSize)})

	// Default encryption: an object without headers and a multipart one.
	_, err = c.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{Bucket: aws.String(bucketSecure),
		ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
			ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256},
		}}}})
	require.NoError(t, err)
	ds.put(t, c, version{bucket: bucketSecure, key: "auto", sse: ""}, payload("auto", 300<<10))
	ds.versions[len(ds.versions)-1].sse = types.ServerSideEncryptionAes256
	ds.multipart(t, c, bucketSecure, "mp-enc", [][]byte{payload("mp-enc-1", partSize), payload("mp-enc-2", 2<<20)}, types.ServerSideEncryptionAes256)

	// Object Lock: governance retention and legal hold.
	ds.heldUntil = time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	held, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucketLocked), Key: aws.String("held"),
		Body: bytes.NewReader(payload("held", 4096)), ObjectLockMode: types.ObjectLockModeGovernance,
		ObjectLockRetainUntilDate: aws.Time(ds.heldUntil), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32})
	require.NoError(t, err)
	ds.held = aws.ToString(held.VersionId)
	ds.versions = append(ds.versions, version{bucket: bucketLocked, key: "held", id: ds.held, sha: digest(payload("held", 4096)), size: 4096, etag: aws.ToString(held.ETag)})
	legal, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucketLocked), Key: aws.String("legal"),
		Body: bytes.NewReader(payload("legal", 4096)), ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOn,
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32})
	require.NoError(t, err)
	ds.legal = aws.ToString(legal.VersionId)
	ds.versions = append(ds.versions, version{bucket: bucketLocked, key: "legal", id: ds.legal, sha: digest(payload("legal", 4096)), size: 4096, etag: aws.ToString(legal.ETag)})

	// Bucket policy (anonymous reads of public/pub/*) and CORS.
	ds.put(t, c, version{bucket: bucketPublic, key: "pub/hello"}, []byte("hello, world"))
	ds.put(t, c, version{bucket: bucketPublic, key: "private/secret"}, []byte("secret"))
	_, err = c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(bucketPublic), Policy: aws.String(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::public/pub/*"]}]}`)})
	require.NoError(t, err)
	_, err = c.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: aws.String(bucketPublic), CORSConfiguration: &types.CORSConfiguration{
		CORSRules: []types.CORSRule{{AllowedOrigins: []string{"https://app.example.com"}, AllowedMethods: []string{"GET"}, AllowedHeaders: []string{"*"}, MaxAgeSeconds: aws.Int32(600)}},
	}})
	require.NoError(t, err)

	// Lifecycle.
	_, err = c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{Bucket: aws.String(bucketData),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{
			{ID: aws.String("tmp-expire"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("tmp/")},
				Expiration: &types.LifecycleExpiration{Days: aws.Int32(30)}},
			{ID: aws.String("old-versions"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")},
				NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(365)}},
		}}})
	require.NoError(t, err)

	// IAM: a user with a policy reading bucketData, and an access key of
	// the user.
	_, err = admin.CreatePolicy(ctx, readerPolicy, []byte(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::data","arn:aws:s3:::data/*"]}]}`))
	require.NoError(t, err)
	_, err = admin.CreateUser(ctx, nexoadmin.CreateUserRequest{AccessKey: readerUser, SecretKey: readerSecret, PolicyNames: []string{readerPolicy}})
	require.NoError(t, err)
	child, err := admin.CreateAccessKey(ctx, nexoadmin.CreateAccessKeyRequest{ParentUser: readerUser, Name: "upgrade"})
	require.NoError(t, err)
	ds.childKey, ds.childSecret = child.AccessKey.AccessKey, child.SecretKey

	if hook != nil {
		writeEvents(t, c, admin, hook)
	}
	return ds
}

// multipart uploads parts as bucket/key with CRC32C checksums (and the
// encryption sse, "" for the bucket default) and records the version.
func (ds *dataset) multipart(t *testing.T, c *s3.Client, bucket, key string, parts [][]byte, sse types.ServerSideEncryption) {
	t.Helper()
	ctx := context.Background()
	create := &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32c}
	if sse != "" {
		create.ServerSideEncryption = sse
	}
	up, err := c.CreateMultipartUpload(ctx, create)
	require.NoError(t, err)
	var completed []types.CompletedPart
	for i, p := range parts {
		out, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: up.UploadId,
			PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(p), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32c}) //nolint:gosec // a few parts
		require.NoError(t, err)
		completed = append(completed, types.CompletedPart{ETag: out.ETag, ChecksumCRC32C: out.ChecksumCRC32C, PartNumber: aws.Int32(int32(i + 1))}) //nolint:gosec // a few parts
	}
	done, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: up.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: completed}})
	require.NoError(t, err)
	all := bytes.Join(parts, nil)
	v := version{bucket: bucket, key: key, id: aws.ToString(done.VersionId), sha: digest(all), size: int64(len(all)), etag: aws.ToString(done.ETag), sse: sse}
	if bucket == bucketSecure {
		v.sse = types.ServerSideEncryptionAes256
	}
	ds.versions = append(ds.versions, v)
	ds.latest[bucket+"/"+key] = v.sha
	if bucket == bucketData && key == "mp" {
		ds.mpChecksum = aws.ToString(done.ChecksumCRC32C)
		ds.mpBoundary = all[partSize-10 : partSize+10]
	}
}

// openUpload starts the multipart upload bucket/key and uploads parts,
// leaving it open.
func (ds *dataset) openUpload(t *testing.T, c *s3.Client, bucket, key string, parts [][]byte) {
	t.Helper()
	ctx := context.Background()
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err)
	ds.pendingID = aws.ToString(up.UploadId)
	for i, p := range parts {
		out, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: up.UploadId,
			PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(p)}) //nolint:gosec // a few parts
		require.NoError(t, err)
		ds.pendingParts = append(ds.pendingParts, p)
		ds.pendingETags = append(ds.pendingETags, aws.ToString(out.ETag))
	}
}

// writeEvents configures the webhook target and the notifications of
// bucketEvents, checks a delivery, then leaves the event of
// "queued-before-upgrade" in the persistent queue (webhook offline).
func writeEvents(t *testing.T, c *s3.Client, admin nexoadmin.AdminClient, hook *webhook) {
	t.Helper()
	ctx := context.Background()
	_, err := admin.SetConfig(ctx, "notify_webhook", map[string]string{"enable": "on", "endpoint": hook.srv.URL, "auth_token": webhookToken})
	require.NoError(t, err)
	_, err = c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketEvents)})
	require.NoError(t, err)
	// The target is loaded asynchronously: retry the configuration until
	// the ARN is known.
	waitFor(t, 30*time.Second, "notification configuration", func() error {
		_, err := c.PutBucketNotificationConfiguration(ctx, notificationInput())
		return err
	})
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucketEvents), Key: aws.String("delivered-before-upgrade"), Body: strings.NewReader("x")})
	require.NoError(t, err)
	hook.waitEvent(t, "delivered-before-upgrade")
	hook.offline.Store(true)
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucketEvents), Key: aws.String("queued-before-upgrade"), Body: strings.NewReader("x")})
	require.NoError(t, err)
}

func notificationInput() *s3.PutBucketNotificationConfigurationInput {
	return &s3.PutBucketNotificationConfigurationInput{Bucket: aws.String(bucketEvents), NotificationConfiguration: &types.NotificationConfiguration{
		QueueConfigurations: []types.QueueConfiguration{{Id: aws.String("to-webhook"), QueueArn: aws.String(webhookARN()),
			Events: []types.Event{"s3:ObjectCreated:*"}}},
	}}
}

// apiCode returns the S3 error code of an SDK error ("" when none).
func apiCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// get reads a version of the data set.
func (ds *dataset) get(ctx context.Context, c *s3.Client, v version) ([]byte, *s3.GetObjectOutput, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(v.bucket), Key: aws.String(v.key)}
	if v.id != "" && v.id != "null" {
		in.VersionId = aws.String(v.id)
	}
	if v.ssec {
		in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5 = ds.ssec()
	}
	out, err := c.GetObject(ctx, in)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	return data, out, err
}

// verifyObjects checks every version of the data set (content, ETag,
// encryption, metadata) and the latest content of every key, through c.
func (ds *dataset) verifyObjects(t *testing.T, c *s3.Client, stage string) {
	t.Helper()
	ctx := context.Background()
	for _, v := range ds.versions {
		data, out, err := ds.get(ctx, c, v)
		if !assert.NoError(t, err, "%s: get %s/%s?versionId=%s", stage, v.bucket, v.key, v.id) {
			continue
		}
		assert.Equal(t, v.sha, digest(data), "%s: content of %s/%s?versionId=%s", stage, v.bucket, v.key, v.id)
		assert.Equal(t, v.etag, aws.ToString(out.ETag), "%s: ETag of %s/%s?versionId=%s", stage, v.bucket, v.key, v.id)
		if v.sse != "" {
			assert.Equal(t, v.sse, out.ServerSideEncryption, "%s: encryption of %s/%s", stage, v.bucket, v.key)
		}
		if v.kmsKey != "" {
			assert.Contains(t, aws.ToString(out.SSEKMSKeyId), v.kmsKey, "%s: KMS key of %s/%s", stage, v.bucket, v.key)
		}
		for k, val := range v.metadata {
			assert.Equal(t, val, out.Metadata[k], "%s: metadata %s of %s/%s", stage, k, v.bucket, v.key)
		}
		if v.ssec {
			_, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(v.bucket), Key: aws.String(v.key), VersionId: aws.String(v.id)})
			assert.Error(t, err, "%s: an SSE-C version needs its key", stage)
		}
	}
	t.Logf("%s: %d versions and %d keys checked", stage, len(ds.versions), len(ds.latest))
	for name, sha := range ds.latest {
		bucket, key, _ := strings.Cut(name, "/")
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if sha == "" {
			assert.Equal(t, "NoSuchKey", apiCode(err), "%s: %s has a delete marker as latest version", stage, name)
			continue
		}
		if key == "sse-c" {
			assert.Error(t, err, "%s: %s needs its key", stage, name)
			continue
		}
		if !assert.NoError(t, err, "%s: get %s", stage, name) {
			continue
		}
		data, err := io.ReadAll(out.Body)
		_ = out.Body.Close()
		require.NoError(t, err)
		assert.Equal(t, sha, digest(data), "%s: latest content of %s", stage, name)
	}
}

// verifyState checks the rest of the data set through the root client c
// of node n: versions and delete marker,
// multipart checksums and ranges, the open upload, tags, bucket
// configurations, Object Lock, anonymous access and IAM.
func (ds *dataset) verifyState(t *testing.T, n *node, stage string) {
	t.Helper()
	ctx := context.Background()
	c := n.rootClient()

	// The versions of data/doc, the delete marker first.
	lv, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucketData), Prefix: aws.String("doc")})
	require.NoError(t, err, stage)
	var markers, ids []string
	for _, m := range lv.DeleteMarkers {
		markers = append(markers, aws.ToString(m.VersionId))
	}
	for _, v := range lv.Versions {
		ids = append(ids, aws.ToString(v.VersionId))
	}
	assert.Contains(t, markers, ds.marker, "%s: the delete marker of data/doc", stage)
	for _, v := range ds.versions {
		if v.bucket == bucketData && v.key == "doc" {
			assert.Contains(t, ids, v.id, "%s: version %s of data/doc listed", stage, v.id)
		}
	}

	// The multipart object: composite checksum, parts and a range across
	// the boundary of its first two parts.
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucketData), Key: aws.String("mp"), ChecksumMode: types.ChecksumModeEnabled})
	require.NoError(t, err, stage)
	assert.Equal(t, ds.mpChecksum, aws.ToString(head.ChecksumCRC32C), "%s: checksum of data/mp", stage)
	attrs, err := c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: aws.String(bucketData), Key: aws.String("mp"),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesObjectParts, types.ObjectAttributesChecksum}})
	require.NoError(t, err, stage)
	require.NotNil(t, attrs.ObjectParts, stage)
	assert.Equal(t, int32(3), aws.ToInt32(attrs.ObjectParts.TotalPartsCount), "%s: parts of data/mp", stage)
	rng, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucketData), Key: aws.String("mp"),
		Range: aws.String(fmt.Sprintf("bytes=%d-%d", partSize-10, partSize+9))})
	require.NoError(t, err, stage)
	got, _ := io.ReadAll(rng.Body)
	_ = rng.Body.Close()
	assert.Equal(t, ds.mpBoundary, got, "%s: range across the parts of data/mp", stage)

	// The open upload, until it is completed.
	if ds.pendingID != "" {
		parts, err := c.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(bucketData), Key: aws.String("pending"), UploadId: aws.String(ds.pendingID)})
		require.NoError(t, err, "%s: parts of the open upload", stage)
		var etags []string
		for _, p := range parts.Parts {
			etags = append(etags, aws.ToString(p.ETag))
		}
		assert.Equal(t, ds.pendingETags, etags, "%s: parts of the open upload", stage)
	}

	// Tags and bucket configurations.
	tags, err := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucketData), Key: aws.String("keep"), VersionId: aws.String(ds.keep)})
	require.NoError(t, err, stage)
	require.Len(t, tags.TagSet, 1, stage)
	assert.Equal(t, "storage", aws.ToString(tags.TagSet[0].Value), stage)
	btags, err := c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(bucketData)})
	require.NoError(t, err, stage)
	require.Len(t, btags.TagSet, 1, stage)
	assert.Equal(t, "upgrade", aws.ToString(btags.TagSet[0].Value), stage)
	ver, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucketData)})
	require.NoError(t, err, stage)
	assert.Equal(t, types.BucketVersioningStatusEnabled, ver.Status, stage)
	enc, err := c.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: aws.String(bucketSecure)})
	require.NoError(t, err, stage)
	require.Len(t, enc.ServerSideEncryptionConfiguration.Rules, 1, stage)
	assert.Equal(t, types.ServerSideEncryptionAes256, enc.ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault.SSEAlgorithm, stage)
	lc, err := c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucketData)})
	require.NoError(t, err, stage)
	var rules []string
	for _, r := range lc.Rules {
		rules = append(rules, aws.ToString(r.ID))
	}
	assert.ElementsMatch(t, []string{"tmp-expire", "old-versions"}, rules, stage)
	cors, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(bucketPublic)})
	require.NoError(t, err, stage)
	require.Len(t, cors.CORSRules, 1, stage)
	assert.Equal(t, []string{"https://app.example.com"}, cors.CORSRules[0].AllowedOrigins, stage)
	pol, err := c.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(bucketPublic)})
	require.NoError(t, err, stage)
	assert.Contains(t, aws.ToString(pol.Policy), "public/pub/*", stage)

	// Object Lock.
	lock, err := c.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(bucketLocked)})
	require.NoError(t, err, stage)
	assert.Equal(t, types.ObjectLockEnabledEnabled, lock.ObjectLockConfiguration.ObjectLockEnabled, stage)
	ret, err := c.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{Bucket: aws.String(bucketLocked), Key: aws.String("held"), VersionId: aws.String(ds.held)})
	require.NoError(t, err, stage)
	assert.Equal(t, types.ObjectLockRetentionModeGovernance, ret.Retention.Mode, stage)
	assert.True(t, ds.heldUntil.Equal(aws.ToTime(ret.Retention.RetainUntilDate)), "%s: retention date %v", stage, aws.ToTime(ret.Retention.RetainUntilDate))
	hold, err := c.GetObjectLegalHold(ctx, &s3.GetObjectLegalHoldInput{Bucket: aws.String(bucketLocked), Key: aws.String("legal"), VersionId: aws.String(ds.legal)})
	require.NoError(t, err, stage)
	assert.Equal(t, types.ObjectLockLegalHoldStatusOn, hold.LegalHold.Status, stage)
	for _, v := range []struct{ key, id string }{{"held", ds.held}, {"legal", ds.legal}} {
		_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucketLocked), Key: aws.String(v.key), VersionId: aws.String(v.id)})
		assert.Error(t, err, "%s: the locked version %s stays", stage, v.key)
	}

	// Anonymous access through the bucket policy.
	for path, want := range map[string]int{"/public/pub/hello": http.StatusOK, "/public/private/secret": http.StatusForbidden} {
		resp, err := n.d.pki.httpClient(time.Minute).Get(n.url() + path) //nolint:noctx // a URL of the test deployment
		require.NoError(t, err, stage)
		_ = resp.Body.Close()
		assert.Equal(t, want, resp.StatusCode, "%s: anonymous GET %s", stage, path)
	}

	// IAM: the user and its access key read bucketData and nothing else.
	for _, creds := range [][2]string{{readerUser, readerSecret}, {ds.childKey, ds.childSecret}} {
		rc := n.d.client(n.url(), creds[0], creds[1], 3)
		_, err := rc.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucketData), Key: aws.String("keep")})
		assert.NoError(t, err, "%s: %s reads data/keep", stage, creds[0])
		_, err = rc.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucketData), Key: aws.String("forbidden"), Body: strings.NewReader("x")})
		assert.Equal(t, "AccessDenied", apiCode(err), "%s: %s cannot write", stage, creds[0])
		_, err = rc.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucketPublic), Key: aws.String("private/secret")})
		assert.Error(t, err, "%s: %s cannot read another bucket", stage, creds[0])
	}
}

// change modifies the data set through c (the release under test): a new
// version over the delete marker, a version removed for good, the open
// upload completed, an overwrite and a new SSE-KMS object.
func (ds *dataset) change(t *testing.T, c *s3.Client, tag string) {
	t.Helper()
	ctx := context.Background()
	ds.put(t, c, version{bucket: bucketData, key: "doc"}, payload("doc-"+tag, 200<<10))
	for _, v := range ds.versions {
		if v.bucket == bucketData && v.key == "doc" {
			_, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucketData), Key: aws.String("doc"), VersionId: aws.String(v.id)})
			require.NoError(t, err)
			ds.forget(bucketData, "doc", v.id)
			break
		}
	}
	ds.put(t, c, version{bucket: bucketData, key: "keep", metadata: map[string]string{"origin": tag}}, payload("keep-"+tag, 50<<10))
	ds.put(t, c, version{bucket: bucketData, key: "sse-kms-" + tag, sse: types.ServerSideEncryptionAwsKms, kmsKey: kmsKeyName}, payload("kms-"+tag, 64<<10))
	ds.put(t, c, version{bucket: bucketSecure, key: "auto-" + tag}, payload("auto-"+tag, 64<<10))
	ds.versions[len(ds.versions)-1].sse = types.ServerSideEncryptionAes256
	if ds.pendingID != "" {
		last := payload("pending-"+tag, 1<<20)
		out, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucketData), Key: aws.String("pending"), UploadId: aws.String(ds.pendingID),
			PartNumber: aws.Int32(int32(len(ds.pendingParts) + 1)), Body: bytes.NewReader(last)}) //nolint:gosec // a few parts
		require.NoError(t, err, "a part of the upload the old release started")
		var parts []types.CompletedPart
		for i, etag := range append(slices.Clone(ds.pendingETags), aws.ToString(out.ETag)) {
			parts = append(parts, types.CompletedPart{ETag: aws.String(etag), PartNumber: aws.Int32(int32(i + 1))}) //nolint:gosec // a few parts
		}
		done, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucketData), Key: aws.String("pending"),
			UploadId: aws.String(ds.pendingID), MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
		require.NoError(t, err, "completion of the upload the old release started")
		all := bytes.Join(append(slices.Clone(ds.pendingParts), last), nil)
		ds.versions = append(ds.versions, version{bucket: bucketData, key: "pending", id: aws.ToString(done.VersionId),
			sha: digest(all), size: int64(len(all)), etag: aws.ToString(done.ETag)})
		ds.latest[bucketData+"/pending"] = digest(all)
		ds.pendingID = ""
	}
}
