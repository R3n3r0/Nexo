package integration_test

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// The behaviors below were found by the Ceph s3-tests suite (test/compat,
// docs/06 §6.1); each is checked here in every deployment mode.

// TestStalledDownloadReleasesLock: a client that stops reading a GetObject
// body has its response aborted after the send timeout, which releases the
// read lock of the object: an overwrite does not wait for the lock timeout
// (s3-tests test_multipart_copy_versioned, docs/02 §2.4).
//
// covers: S-OBJ-01
func TestStalledDownloadReleasesLock(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithConfig(func(c *config.Static) { c.SendTimeout = 500 * time.Millisecond }))
	e.mkBucket("stall")
	// Far more than the socket buffers of a loopback connection hold.
	big := bytes.Repeat([]byte("s"), 24<<20)
	e.put("stall", "big", big)

	conn, err := net.Dial("tcp", e.hostPort())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	req := newRequest(t, http.MethodGet, e.srv.S3URL+"/stall/big", nil)
	e.signRequest(req, e.rootCreds(), nil)
	require.NoError(t, req.Write(conn))
	// The status line arrives, then the client reads nothing more.
	status, err := bufio.NewReaderSize(conn, 64).ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, status, "200")

	start := time.Now()
	_, err = e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("stall"), Key: aws.String("big"), Body: strings.NewReader("small")})
	require.NoError(t, err, "the overwrite gets the lock of the stalled download")
	assert.Less(t, time.Since(start), 15*time.Second, "well before the lock timeout (30s)")
	assert.Equal(t, []byte("small"), e.get("stall", "big"))
}

// TestStalledWebsiteDownloadReleasesLock: an anonymous client of a public
// website that stops reading an object of more than 8 parts (read under
// the lock of its key, docs/02 §2.4) has its response aborted after the
// send timeout, like an S3 download: an overwrite and a delete of the
// owner do not fail with XNexoLockTimeout (regression: the website
// downloads had no stall bound and held the lock for as long as the
// client stayed connected).
//
// covers: S-DST-01
func TestStalledWebsiteDownloadReleasesLock(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithConfig(func(c *config.Static) {
		c.SendTimeout = 500 * time.Millisecond
		c.WebAddress = "127.0.0.1:0"
	}))
	e.mkBucket("stallweb")
	part := bytes.Repeat([]byte("m"), 5<<20)
	upload := func() {
		up, err := e.root.CreateMultipartUpload(e.ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("stallweb"), Key: aws.String("mp.bin")})
		require.NoError(t, err)
		var done []types.CompletedPart
		for n := int32(1); n <= 9; n++ {
			out, err := e.root.UploadPart(e.ctx, &s3.UploadPartInput{Bucket: aws.String("stallweb"), Key: aws.String("mp.bin"), UploadId: up.UploadId,
				PartNumber: aws.Int32(n), Body: bytes.NewReader(part)})
			require.NoError(t, err)
			done = append(done, types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.ETag})
		}
		_, err = e.root.CompleteMultipartUpload(e.ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("stallweb"), Key: aws.String("mp.bin"),
			UploadId: up.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}})
		require.NoError(t, err)
	}
	upload()
	e.cacheSite("stall-site", "stallweb", "stall.site.test")
	// Without the cache: the download reads the object itself.
	e.setCache(cache.DistributionConfig{Distribution: "stall-site", Cache: cache.Settings{Enabled: aws.Bool(false)}})

	// stall starts an anonymous download of the object of 9 parts that
	// stops reading after the status line.
	stall := func() net.Conn {
		conn, err := net.Dial("tcp", e.srv.Nodes[0].App.WebAddr())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		_, err = io.WriteString(conn, "GET /mp.bin HTTP/1.1\r\nHost: stall.site.test\r\n\r\n")
		require.NoError(t, err)
		status, err := bufio.NewReaderSize(conn, 64).ReadString('\n')
		require.NoError(t, err)
		require.Contains(t, status, "200")
		return conn
	}

	conn := stall()
	start := time.Now()
	_, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("stallweb"), Key: aws.String("mp.bin"), Body: strings.NewReader("small")})
	require.NoError(t, err, "the overwrite gets the lock of the stalled website download")
	assert.Less(t, time.Since(start), 15*time.Second, "well before the lock timeout (30s)")
	assert.Equal(t, []byte("small"), e.get("stallweb", "mp.bin"))
	_ = conn.Close()

	upload()
	stall()
	start = time.Now()
	_, err = e.root.DeleteObject(e.ctx, &s3.DeleteObjectInput{Bucket: aws.String("stallweb"), Key: aws.String("mp.bin")})
	require.NoError(t, err, "the delete gets the lock of the stalled website download")
	assert.Less(t, time.Since(start), 15*time.Second, "well before the lock timeout (30s)")
}

// TestStalledDownloadIsASnapshot: a GET releases the lock of its key once
// the data of the version is open (MVCC on the data directory): an
// overwrite does not wait for a client that stopped reading (without
// relying on NEXO_SEND_TIMEOUT), and the download, resumed afterwards,
// delivers the whole version it started with (ceph/s3-tests
// test_atomic_read_*), single-part and multipart.
//
// covers: S-OBJ-02
func TestStalledDownloadIsASnapshot(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("snapshot")
	big := bytes.Repeat([]byte("s"), 24<<20)
	parts := [][]byte{bytes.Repeat([]byte("p"), 5<<20), bytes.Repeat([]byte("q"), 5<<20), bytes.Repeat([]byte("r"), 3<<20)}
	e.put("snapshot", "single", big)
	up, err := e.root.CreateMultipartUpload(e.ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("snapshot"), Key: aws.String("multi")})
	require.NoError(t, err)
	var done []types.CompletedPart
	for i, p := range parts {
		out, err := e.root.UploadPart(e.ctx, &s3.UploadPartInput{Bucket: aws.String("snapshot"), Key: aws.String("multi"), UploadId: up.UploadId,
			PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(p)})
		require.NoError(t, err)
		done = append(done, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.ETag})
	}
	_, err = e.root.CompleteMultipartUpload(e.ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("snapshot"), Key: aws.String("multi"),
		UploadId: up.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}})
	require.NoError(t, err)

	for key, want := range map[string][]byte{"single": big, "multi": bytes.Join(parts, nil)} {
		t.Run(key, func(t *testing.T) {
			conn, err := net.Dial("tcp", e.hostPort())
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			req := newRequest(t, http.MethodGet, e.srv.S3URL+"/snapshot/"+key, nil)
			e.signRequest(req, e.rootCreds(), nil)
			require.NoError(t, req.Write(conn))
			// The headers arrive, then the client reads nothing more.
			resp, err := http.ReadResponse(bufio.NewReaderSize(conn, 64), req)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)

			start := time.Now()
			_, err = e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("snapshot"), Key: aws.String(key), Body: strings.NewReader("new")})
			require.NoError(t, err)
			assert.Less(t, time.Since(start), 5*time.Second, "the overwrite does not wait for the stalled download")
			assert.Equal(t, []byte("new"), e.get("snapshot", key))

			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, len(want), len(got))
			assert.True(t, bytes.Equal(want, got), "the download delivers the version it started with")
		})
	}
}

// TestMultipartCompletionRetry: a CompleteMultipartUpload repeated after
// its success (the response lost) answers with the same object.
//
// covers: S-OBJ-06
func TestMultipartCompletionRetry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("retry")
	up, err := e.root.CreateMultipartUpload(e.ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("retry"), Key: aws.String("k"),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
	require.NoError(t, err)
	part := bytes.Repeat([]byte("p"), 5<<20)
	var parts []types.CompletedPart
	for n := int32(1); n <= 2; n++ {
		out, err := e.root.UploadPart(e.ctx, &s3.UploadPartInput{Bucket: aws.String("retry"), Key: aws.String("k"), UploadId: up.UploadId,
			PartNumber: aws.Int32(n), Body: bytes.NewReader(part), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
		require.NoError(t, err)
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.ETag, ChecksumSHA256: out.ChecksumSHA256})
	}
	in := &s3.CompleteMultipartUploadInput{Bucket: aws.String("retry"), Key: aws.String("k"), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}}
	first, err := e.root.CompleteMultipartUpload(e.ctx, in)
	require.NoError(t, err)
	again, err := e.root.CompleteMultipartUpload(e.ctx, in)
	require.NoError(t, err, "a retried completion succeeds")
	assert.Equal(t, aws.ToString(first.ETag), aws.ToString(again.ETag))
	assert.Equal(t, aws.ToString(first.ChecksumSHA256), aws.ToString(again.ChecksumSHA256))

	// Once the key is overwritten, the upload is gone for good.
	e.put("retry", "k", []byte("other"))
	_, err = e.root.CompleteMultipartUpload(e.ctx, in)
	assert.Equal(t, "NoSuchUpload", apiCode(err))
}

// TestMultipartCompletionValidation: as S3, the completion of an upload
// created with a checksum algorithm (COMPOSITE) must list the checksum of
// every part, and an x-amz-mp-object-size that is not the size of the
// object is refused; both with 400 InvalidRequest, nothing committed.
//
// covers: S-OBJ-06, S-OBJ-09
func TestMultipartCompletionValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "complete-checks"
	e.mkBucket(bucket)
	up, err := e.root.CreateMultipartUpload(e.ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("k"),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
	require.NoError(t, err)
	part := bytes.Repeat([]byte("p"), 5<<20)
	var withSums, withoutSums []types.CompletedPart
	for n := int32(1); n <= 2; n++ {
		out, err := e.root.UploadPart(e.ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("k"), UploadId: up.UploadId,
			PartNumber: aws.Int32(n), Body: bytes.NewReader(part), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
		require.NoError(t, err)
		withSums = append(withSums, types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.ETag, ChecksumSHA256: out.ChecksumSHA256})
		withoutSums = append(withoutSums, types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.ETag})
	}
	complete := func(parts []types.CompletedPart, size *int64) error {
		_, err := e.root.CompleteMultipartUpload(e.ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("k"),
			UploadId: up.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}, MpuObjectSize: size})
		return err
	}
	err = complete(withoutSums, nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, httpStatus(err))
	assert.Equal(t, "InvalidRequest", apiCode(err))
	assert.Contains(t, err.Error(), "It was missing for part 1 in the request", "the S3 message")
	// A declared object checksum that does not match is reported first
	// (ceph/s3-tests test_multipart_checksum_sha256).
	_, err = e.root.CompleteMultipartUpload(e.ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("k"),
		UploadId: up.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: withoutSums},
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(make([]byte, sha256.Size)))})
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, httpStatus(err))
	assert.Equal(t, "BadDigest", apiCode(err))
	for _, wrong := range []int64{5, 1024, 10<<20 + 1} {
		err = complete(withSums, aws.Int64(wrong))
		require.Error(t, err, "x-amz-mp-object-size %d", wrong)
		assert.Equal(t, http.StatusBadRequest, httpStatus(err))
		assert.Equal(t, "InvalidRequest", apiCode(err))
	}
	_, err = e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("k")})
	assert.Equal(t, http.StatusNotFound, httpStatus(err), "nothing is committed")
	require.NoError(t, complete(withSums, aws.Int64(10<<20)))
	head, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("k")})
	require.NoError(t, err)
	assert.Equal(t, int64(10<<20), aws.ToInt64(head.ContentLength))
}

// TestS3ResponseDetails: response details the SDKs rely on.
//
// covers: S-OBJ-03, S-OBJ-10, S-BKT-07, S-OBJ-02
func TestS3ResponseDetails(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("details")

	// No x-amz-version-id for the null version of a suspended bucket.
	e.setVersioning("details", types.BucketVersioningStatusSuspended)
	put, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("details"), Key: aws.String("null"), Body: strings.NewReader("x")})
	require.NoError(t, err)
	assert.Nil(t, put.VersionId, "the null version is not announced")
	e.setVersioning("details", types.BucketVersioningStatusEnabled)
	put, err = e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("details"), Key: aws.String("null"), Body: strings.NewReader("y")})
	require.NoError(t, err)
	assert.NotEmpty(t, aws.ToString(put.VersionId))

	// If-Match on a missing key: 404 NoSuchKey.
	_, err = e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("details"), Key: aws.String("absent"), Body: strings.NewReader("x"),
		IfMatch: aws.String("*")})
	assert.Equal(t, http.StatusNotFound, httpStatus(err))
	assert.Equal(t, "NoSuchKey", apiCode(err))

	// Listings: spaces encoded as %20; the V1 Prefix sent as is.
	e.put("details", "a b/c d", []byte("x"))
	var v1 struct {
		Prefix   string `xml:"Prefix"`
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	resp := e.signedDo(http.MethodGet, "/details?encoding-type=url&prefix=a%20b%2F", e.rootCreds(), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	require.NoError(t, xml.Unmarshal(resp.Body, &v1))
	assert.Equal(t, "a b/", v1.Prefix)
	require.Len(t, v1.Contents, 1)
	assert.Equal(t, "a%20b/c%20d", v1.Contents[0].Key)

	// SSE-S3/SSE-KMS headers are refused on a read.
	resp = e.signedDo(http.MethodGet, "/details/null", e.rootCreds(), nil, http.Header{"X-Amz-Server-Side-Encryption": {"aws:kms"}})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(resp.Body), "<Code>InvalidArgument</Code>")

	// A lifecycle rule written with the legacy top-level Prefix is
	// returned in that form.
	doc := `<LifecycleConfiguration><Rule><ID>old</ID><Prefix>logs/</Prefix><Status>Enabled</Status>` +
		`<Expiration><Days>3</Days></Expiration></Rule></LifecycleConfiguration>`
	resp = e.signedDo(http.MethodPut, "/details?lifecycle", e.rootCreds(), []byte(doc), http.Header{"Content-Md5": {base64MD5([]byte(doc))}})
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	lc, err := e.root.GetBucketLifecycleConfiguration(e.ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("details")})
	require.NoError(t, err)
	require.Len(t, lc.Rules, 1)
	assert.Equal(t, "logs/", aws.ToString(lc.Rules[0].Prefix)) //nolint:staticcheck // the deprecated element is the point
	assert.Nil(t, lc.Rules[0].Filter)

	// The read of one part reports its checksum with the type of the
	// object.
	up, err := e.root.CreateMultipartUpload(e.ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("details"), Key: aws.String("mp"),
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32, ChecksumType: types.ChecksumTypeFullObject})
	require.NoError(t, err)
	part, err := e.root.UploadPart(e.ctx, &s3.UploadPartInput{Bucket: aws.String("details"), Key: aws.String("mp"), UploadId: up.UploadId,
		PartNumber: aws.Int32(1), Body: strings.NewReader("part one"), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32})
	require.NoError(t, err)
	_, err = e.root.CompleteMultipartUpload(e.ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("details"), Key: aws.String("mp"),
		UploadId: up.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: part.ETag, ChecksumCRC32: part.ChecksumCRC32}}}})
	require.NoError(t, err)
	got, err := e.root.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("details"), Key: aws.String("mp"), PartNumber: aws.Int32(1),
		ChecksumMode: types.ChecksumModeEnabled})
	require.NoError(t, err)
	_ = got.Body.Close()
	assert.Equal(t, aws.ToString(part.ChecksumCRC32), aws.ToString(got.ChecksumCRC32))
	assert.Equal(t, types.ChecksumTypeFullObject, got.ChecksumType)
}

// TestObjectLockValidation: the S3 answers to malformed Object Lock
// configurations and the governance rules of retention changes.
//
// covers: S-BKT-08, S-OBJ-12
func TestObjectLockValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "lockval"
	e.mkLockedBucket(bucket)
	rule := func(retention string) []byte {
		return []byte(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention>` +
			retention + `</DefaultRetention></Rule></ObjectLockConfiguration>`)
	}
	for _, tt := range []struct {
		name, retention, code string
	}{
		{"zero days", `<Mode>GOVERNANCE</Mode><Days>0</Days>`, "InvalidRetentionPeriod"},
		{"negative years", `<Mode>COMPLIANCE</Mode><Years>-1</Years>`, "InvalidRetentionPeriod"},
		{"lowercase mode", `<Mode>governance</Mode><Days>1</Days>`, "MalformedXML"},
		{"days and years", `<Mode>GOVERNANCE</Mode><Days>1</Days><Years>1</Years>`, "MalformedXML"},
	} {
		body := rule(tt.retention)
		resp := e.signedDo(http.MethodPut, "/"+bucket+"?object-lock", e.rootCreds(), body, http.Header{"Content-Md5": {base64MD5(body)}})
		requireS3Error(t, resp, http.StatusBadRequest, tt.code)
	}

	// A governance retention becomes compliance only with the bypass.
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	_, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("gov"), Body: strings.NewReader("g"),
		ObjectLockMode: types.ObjectLockModeGovernance, ObjectLockRetainUntilDate: aws.Time(until), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32})
	require.NoError(t, err)
	toCompliance := &s3.PutObjectRetentionInput{Bucket: aws.String(bucket), Key: aws.String("gov"),
		Retention:         &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeCompliance, RetainUntilDate: aws.Time(until)},
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32}
	_, err = e.root.PutObjectRetention(e.ctx, toCompliance)
	assert.Equal(t, "AccessDenied", apiCode(err), "a mode change needs the bypass")
	toCompliance.BypassGovernanceRetention = aws.Bool(true)
	_, err = e.root.PutObjectRetention(e.ctx, toCompliance)
	require.NoError(t, err)
	ret, err := e.root.GetObjectRetention(e.ctx, &s3.GetObjectRetentionInput{Bucket: aws.String(bucket), Key: aws.String("gov")})
	require.NoError(t, err)
	assert.Equal(t, types.ObjectLockRetentionModeCompliance, ret.Retention.Mode)
}

// TestSSECustomerKeyRequirements: SSE-C needs the MD5 of the key; the
// description of an SSE-C object (attributes) needs its key, its
// subresources (tags) do not.
//
// covers: S-OBJ-13, S-OBJ-15, S-OBJ-11
func TestSSECustomerKeyRequirements(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithKMS(), testutil.WithTLS())
	c := e.tlsClient()
	const bucket = "ssec-req"
	_, err := c.CreateBucket(e.ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	k := newSSECKey(7)
	_, err = c.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("o"), Body: strings.NewReader("secret"),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(k.key), SSECustomerKeyMD5: aws.String(k.md5),
		Tagging: aws.String("team=a")})
	require.NoError(t, err)

	attrs := &s3.GetObjectAttributesInput{Bucket: aws.String(bucket), Key: aws.String("o"),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesEtag, types.ObjectAttributesObjectSize}}
	_, err = c.GetObjectAttributes(e.ctx, attrs)
	assert.Equal(t, http.StatusBadRequest, httpStatus(err), "the attributes of an SSE-C object need its key")
	attrs.SSECustomerAlgorithm, attrs.SSECustomerKey, attrs.SSECustomerKeyMD5 = aws.String("AES256"), aws.String(k.key), aws.String(k.md5)
	out, err := c.GetObjectAttributes(e.ctx, attrs)
	require.NoError(t, err)
	assert.Equal(t, int64(6), aws.ToInt64(out.ObjectSize))

	tags, err := c.GetObjectTagging(e.ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("o")})
	require.NoError(t, err, "tags are served without the key, as in S3")
	require.Len(t, tags.TagSet, 1)
	assert.Equal(t, "a", aws.ToString(tags.TagSet[0].Value))

	// Without the MD5 of the key: 400 InvalidArgument.
	tlsHTTP := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: e.srv.TLS.Clone()}}
	req := newRequest(t, http.MethodPut, e.srv.S3URL+"/"+bucket+"/nomd5", []byte("x"))
	req.Header.Set("X-Amz-Server-Side-Encryption-Customer-Algorithm", "AES256")
	req.Header.Set("X-Amz-Server-Side-Encryption-Customer-Key", k.key)
	e.signRequest(req, e.rootCreds(), []byte("x"))
	requireS3Error(t, doWith(t, tlsHTTP, req), http.StatusBadRequest, "InvalidArgument")
}

// TestSSECustomerKeyOverPlainHTTP: over plain HTTP an SSE-C key would
// travel in clear: the writes and the reads that carry one are refused
// with 400 InvalidRequest before anything is stored, as in S3.
//
// covers: S-OBJ-13
func TestSSECustomerKeyOverPlainHTTP(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithKMS())
	require.True(t, strings.HasPrefix(e.srv.S3URL, "http://"), "a plain HTTP deployment")
	const bucket = "ssec-plain"
	e.mkBucket(bucket)
	k := newSSECKey(9)
	ssec := func(req *http.Request) {
		req.Header.Set("X-Amz-Server-Side-Encryption-Customer-Algorithm", "AES256")
		req.Header.Set("X-Amz-Server-Side-Encryption-Customer-Key", k.key)
		req.Header.Set("X-Amz-Server-Side-Encryption-Customer-Key-Md5", k.md5)
	}

	req := newRequest(t, http.MethodPut, e.srv.S3URL+"/"+bucket+"/o", []byte("secret"))
	ssec(req)
	e.signRequest(req, e.rootCreds(), []byte("secret"))
	requireS3Error(t, do(t, req), http.StatusBadRequest, "InvalidRequest")
	_, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("o")})
	assert.Equal(t, http.StatusNotFound, httpStatus(err), "nothing stored")

	e.put(bucket, "plain", []byte("plain"))
	req = newRequest(t, http.MethodGet, e.srv.S3URL+"/"+bucket+"/plain", nil)
	ssec(req)
	e.signRequest(req, e.rootCreds(), nil)
	requireS3Error(t, do(t, req), http.StatusBadRequest, "InvalidRequest")
}

// TestRequestHeaderValidation: the request headers of ceph/s3-tests
// test_headers.py. An empty Content-MD5 is invalid (400 InvalidDigest); a
// canned ACL that S3 does not define is a malformed request (400
// InvalidArgument), a public one a feature Nexo does not provide (501,
// S-BKT-13); a request signed with the Date header in the RFC 1123
// numeric-zone format of Python clients (botocore, no X-Amz-Date) is
// accepted.
//
// covers: S-OBJ-01, S-IAM-02
func TestRequestHeaderValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "header-checks"
	e.mkBucket(bucket)
	root := e.rootCreds()

	tests := []struct {
		name   string
		path   string
		header http.Header
		status int
		code   string
	}{
		{"empty Content-MD5", "/" + bucket + "/md5", http.Header{"Content-Md5": {""}}, http.StatusBadRequest, "InvalidDigest"},
		{"malformed Content-MD5", "/" + bucket + "/md5", http.Header{"Content-Md5": {"YWJyYWNhZGFicmE="}}, http.StatusBadRequest, "InvalidDigest"},
		{"unknown canned ACL", "/" + bucket + "/acl", http.Header{"X-Amz-Acl": {"public-ready"}}, http.StatusBadRequest, "InvalidArgument"},
		{"public canned ACL", "/" + bucket + "/acl", http.Header{"X-Amz-Acl": {"public-read"}}, http.StatusNotImplemented, "NotImplemented"},
		{"private canned ACL", "/" + bucket + "/acl", http.Header{"X-Amz-Acl": {"private"}}, http.StatusOK, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := e.signedDo(http.MethodPut, tt.path, root, []byte("bar"), tt.header)
			assert.Equal(t, tt.status, r.StatusCode, "%s", r.Body)
			if tt.code != "" {
				assert.Contains(t, string(r.Body), "<Code>"+tt.code+"</Code>")
			}
		})
	}

	t.Run("signed with the Date header", func(t *testing.T) {
		for _, layout := range []string{"Mon, 02 Jan 2006 15:04:05 -0000", http.TimeFormat} {
			body := []byte("dated")
			req := newRequest(t, http.MethodPut, e.srv.S3URL+"/"+bucket+"/dated", body)
			signWithDateHeader(t, req, root, e.srv.Config.Region, time.Now().UTC(), layout, body)
			r := do(t, req)
			assert.Equal(t, http.StatusOK, r.StatusCode, "Date %q: %s", req.Header.Get("Date"), r.Body)
		}
	})
}

// signWithDateHeader signs req with SigV4 dated by the Date header (in
// layout) instead of X-Amz-Date, as botocore does when the request
// carries a Date header.
func signWithDateHeader(t *testing.T, req *http.Request, c creds, region string, at time.Time, layout string, body []byte) {
	t.Helper()
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	req.Header.Set("Date", at.Format(layout))
	req.Header.Set("X-Amz-Content-Sha256", payload)
	amzDate := at.Format("20060102T150405Z")
	day := at.Format("20060102")
	canonical := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		"date:" + req.Header.Get("Date") + "\nhost:" + req.URL.Host + "\nx-amz-content-sha256:" + payload + "\n",
		"date;host;x-amz-content-sha256",
		payload,
	}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	crSum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(crSum[:])
	mac := func(key []byte, data string) []byte {
		h := hmac.New(sha256.New, key)
		_, _ = h.Write([]byte(data))
		return h.Sum(nil)
	}
	key := mac(mac(mac(mac([]byte("AWS4"+c.SecretKey), day), region), "s3"), "aws4_request")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKey+"/"+scope+
		", SignedHeaders=date;host;x-amz-content-sha256, Signature="+hex.EncodeToString(mac(key, toSign)))
}
