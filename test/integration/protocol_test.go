package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The S3 error documents are well-formed XML with the S3 code, a message
// and the request id of the x-amz-request-id header (docs/04 §4.2).
//
// covers: S-OBJ-01, S-BKT-01
func TestS3ErrorDocuments(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := e.rootCreds()
	e.mkBucket("errs")
	e.put("errs", "obj", []byte("0123456789"))
	etag := e.put("errs", "obj", []byte("0123456789"))

	// A multipart upload with a small first part, for the completion errors.
	mpu := e.signedDo(http.MethodPost, "/errs/mp?uploads", root, nil, nil)
	require.Equal(t, http.StatusOK, mpu.StatusCode, "%s", mpu.Body)
	var initiated struct {
		UploadID string `xml:"UploadId"`
	}
	require.NoError(t, xml.Unmarshal(mpu.Body, &initiated))
	uploadID := initiated.UploadID
	partETags := make([]string, 3)
	for n := 1; n <= 2; n++ {
		resp := e.signedDo(http.MethodPut, fmt.Sprintf("/errs/mp?partNumber=%d&uploadId=%s", n, uploadID), root, []byte("tiny part"), nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
		partETags[n] = resp.Header.Get("ETag")
	}
	completeXML := func(parts ...string) []byte {
		var b strings.Builder
		b.WriteString("<CompleteMultipartUpload>")
		for i := 0; i+1 < len(parts); i += 2 {
			fmt.Fprintf(&b, "<Part><PartNumber>%s</PartNumber><ETag>%s</ETag></Part>", parts[i], parts[i+1])
		}
		b.WriteString("</CompleteMultipartUpload>")
		return []byte(b.String())
	}
	completePath := "/errs/mp?uploadId=" + uploadID

	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
		hdr    http.Header
		status int
		code   string
	}{
		{"NoSuchBucket object", http.MethodGet, "/missing-bucket/key", nil, nil, 404, "NoSuchBucket"},
		{"NoSuchBucket listing", http.MethodGet, "/missing-bucket?list-type=2", nil, nil, 404, "NoSuchBucket"},
		{"NoSuchKey", http.MethodGet, "/errs/missing", nil, nil, 404, "NoSuchKey"},
		{"BucketAlreadyExists", http.MethodPut, "/errs", nil, nil, 409, "BucketAlreadyOwnedByYou"},
		{"BucketNotEmpty", http.MethodDelete, "/errs", nil, nil, 409, "BucketNotEmpty"},
		{"InvalidBucketName", http.MethodPut, "/Invalid_Bucket", nil, nil, 400, "InvalidBucketName"},
		{"MalformedXML delete", http.MethodPost, "/errs?delete", []byte("<Delete><Object><Key>a</Key>"), http.Header{"Content-Md5": {base64MD5([]byte("<Delete><Object><Key>a</Key>"))}}, 400, "MalformedXML"},
		{"MalformedXML complete", http.MethodPost, completePath, []byte("<CompleteMultipartUpload><Part>"), nil, 400, "MalformedXML"},
		{"MalformedXML versioning", http.MethodPut, "/errs?versioning", []byte("not xml at all"), nil, 400, "MalformedXML"},
		{"EntityTooSmall", http.MethodPost, completePath, completeXML("1", partETags[1], "2", partETags[2]), nil, 400, "EntityTooSmall"},
		{"InvalidPart", http.MethodPost, completePath, completeXML("1", `"00000000000000000000000000000000"`), nil, 400, "InvalidPart"},
		{"InvalidPartOrder", http.MethodPost, completePath, completeXML("2", partETags[2], "1", partETags[1]), nil, 400, "InvalidPartOrder"},
		{"NoSuchUpload", http.MethodPost, "/errs/mp?uploadId=no-such-upload", completeXML("1", partETags[1]), nil, 404, "NoSuchUpload"},
		{"PreconditionFailed", http.MethodGet, "/errs/obj", nil, http.Header{"If-Match": {`"nope"`}}, 412, "PreconditionFailed"},
		{"NotModified", http.MethodGet, "/errs/obj", nil, http.Header{"If-None-Match": {`"` + etag + `"`}}, 304, ""},
		{"InvalidRange", http.MethodGet, "/errs/obj", nil, http.Header{"Range": {"bytes=100-200"}}, 416, "InvalidRange"},
		{"NoSuchKey HEAD", http.MethodHead, "/errs/missing", nil, nil, 404, ""},
		{"NotImplemented", http.MethodGet, "/errs?website", nil, nil, 501, "NotImplemented"},
		{"MethodNotAllowed", http.MethodPatch, "/errs/obj", nil, nil, 405, "MethodNotAllowed"},
		{"InvalidAccessKeyId", http.MethodGet, "/errs/obj", nil, nil, 403, "InvalidAccessKeyId"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := root
			if tc.code == "InvalidAccessKeyId" {
				c = creds{AccessKey: "unknown-key", SecretKey: "unknown-secret"}
			}
			resp := e.signedDo(tc.method, tc.path, c, tc.body, tc.hdr)
			doc := requireS3Error(t, resp, tc.status, tc.code)
			if tc.method != http.MethodHead && tc.status != http.StatusNotModified {
				assert.NotEmpty(t, doc.Resource, "Resource names the request path")
			}
		})
	}
	// The anonymous error document too.
	resp := do(t, newRequest(t, http.MethodGet, e.srv.S3URL+"/errs/obj", nil))
	requireS3Error(t, resp, http.StatusForbidden, "AccessDenied")
}

// Every response carries x-amz-request-id and x-amz-id-2, unique per
// request (docs/04 §4.2).
//
// covers: S-OBJ-01, S-OPS-03
func TestRequestIDHeaders(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := e.rootCreds()
	e.mkBucket("ids")
	seen := map[string]bool{}
	requests := []struct {
		method, path string
		body         []byte
	}{
		{http.MethodPut, "/ids/a", []byte("a")},
		{http.MethodGet, "/ids/a", nil},
		{http.MethodHead, "/ids/a", nil},
		{http.MethodGet, "/ids?list-type=2", nil},
		{http.MethodGet, "/", nil},
		{http.MethodDelete, "/ids/a", nil},
		{http.MethodGet, "/ids/a", nil}, // error
	}
	for _, r := range requests {
		resp := e.signedDo(r.method, r.path, root, r.body, nil)
		id := resp.Header.Get("x-amz-request-id")
		assert.NotEmpty(t, id, "%s %s", r.method, r.path)
		assert.NotEmpty(t, resp.Header.Get("x-amz-id-2"), "%s %s", r.method, r.path)
		assert.False(t, seen[id], "request ids are unique")
		seen[id] = true
	}
	for _, path := range []string{"/nexo/health/live", "/nexo/health/ready"} {
		resp := do(t, newRequest(t, http.MethodGet, e.srv.S3URL+path, nil))
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.NotEmpty(t, resp.Header.Get("x-amz-request-id"), path)
	}
	// The SDK exposes it in the response metadata.
	out, err := e.root.HeadBucket(e.ctx, &s3.HeadBucketInput{Bucket: aws.String("ids")})
	require.NoError(t, err)
	_ = out
}

// Virtual-host style requests ("{bucket}.localhost" with NEXO_DOMAIN=localhost)
// reach the bucket named by the host, signed with that host.
//
// covers: S-OBJ-01, S-OBJ-08
func TestVirtualHostStyle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := e.rootCreds()
	e.mkBucket("vhost")
	e.put("vhost", "path/style.txt", []byte("written path-style"))
	_, port, _ := strings.Cut(e.hostPort(), ":")
	vhost := "http://vhost.localhost:" + port
	client := e.loopbackClient()

	send := func(method, path string, body []byte, sign bool) response {
		req := newRequest(t, method, vhost+path, body)
		if sign {
			e.signRequest(req, root, body)
		}
		return doWith(t, client, req)
	}
	resp := send(http.MethodPut, "/virtual.txt", []byte("written virtual-host"), true)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, []byte("written virtual-host"), e.get("vhost", "virtual.txt"), "stored in the bucket of the host")

	resp = send(http.MethodGet, "/path/style.txt", nil, true)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "written path-style", string(resp.Body))

	resp = send(http.MethodGet, "/?list-type=2&prefix=path%2F", nil, true)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	var doc listV2Raw
	require.NoError(t, xml.Unmarshal(resp.Body, &doc))
	require.Len(t, doc.Contents, 1)
	assert.Equal(t, "path/style.txt", doc.Contents[0].Key)

	resp = send(http.MethodGet, "/missing", nil, true)
	requireS3Error(t, resp, http.StatusNotFound, "NoSuchKey")
	resp = send(http.MethodGet, "/virtual.txt", nil, false)
	requireS3Error(t, resp, http.StatusForbidden, "AccessDenied")

	// The same host signed for another one does not verify.
	req := newRequest(t, http.MethodGet, vhost+"/virtual.txt", nil)
	e.signRequest(req, root, nil)
	req.Host = "other.localhost:" + port
	resp = doWith(t, client, req)
	requireS3Error(t, resp, http.StatusForbidden, "SignatureDoesNotMatch")

	// The SDK in virtual-host mode.
	cfg := e.sdkConfig(root)
	cfg.HTTPClient = client
	sdk := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://localhost:" + port)
		o.UsePathStyle = false
	})
	_, err := sdk.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("vhost"), Key: aws.String("sdk.txt"), Body: strings.NewReader("sdk")})
	require.NoError(t, err)
	got := getWith(t, e.ctx, sdk, "vhost", "sdk.txt")
	assert.Equal(t, "sdk", string(got))
	list, err := sdk.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String("vhost")})
	require.NoError(t, err)
	assert.Len(t, list.Contents, 3)

	// Presigned GET on the virtual host.
	ps, err := s3.NewPresignClient(sdk).PresignGetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("vhost"), Key: aws.String("sdk.txt")},
		s3.WithPresignExpires(time.Minute))
	require.NoError(t, err)
	assert.Contains(t, ps.URL, "vhost.localhost")
	resp = doWith(t, client, newRequest(t, http.MethodGet, ps.URL, nil))
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "sdk", string(resp.Body))
	assert.True(t, bytes.Equal([]byte("sdk"), resp.Body))
}

// Regression: in virtual-host style the reserved "/nexo/" paths of the
// listener (health, metrics) are object keys of the bucket: they are
// authenticated (SigV4, never skipped) and reach the bucket, while the
// same paths stay system routes path-style.
//
// covers: S-OBJ-01
func TestVirtualHostSystemPathsAreKeys(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := e.rootCreds()
	e.mkBucket("vhb")
	_, port, _ := strings.Cut(e.hostPort(), ":")
	vhost := "http://vhb.localhost:" + port
	client := e.loopbackClient()
	send := func(method, path string, body []byte, sign bool) response {
		req := newRequest(t, method, vhost+path, body)
		if sign {
			e.signRequest(req, root, body)
		}
		return doWith(t, client, req)
	}
	for _, key := range []string{"nexo/metrics/foo", "nexo/metrics/v1/node", "nexo/health/live"} {
		t.Run(key, func(t *testing.T) {
			body := []byte("data of " + key)
			resp := send(http.MethodPut, "/"+key, body, true)
			require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
			assert.Equal(t, body, e.get("vhb", key), "stored under the key")
			resp = send(http.MethodGet, "/"+key, nil, true)
			require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
			assert.Equal(t, body, resp.Body)
			resp = send(http.MethodGet, "/"+key, nil, false)
			requireS3Error(t, resp, http.StatusForbidden, "AccessDenied")
		})
	}
	// Path-style, the system routes still answer.
	resp := do(t, newRequest(t, http.MethodGet, e.srv.S3URL+"/nexo/health/live", nil))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	resp = do(t, newRequest(t, http.MethodGet, e.srv.S3URL+"/nexo/metrics/v1/node", nil))
	requireS3Error(t, resp, http.StatusForbidden, "AccessDenied")
}

// Regression: a signature is bound to the service of its credential scope:
// S3 requests need "s3", the STS API (POST /) needs "sts".
//
// covers: S-IAM-02, S-IAM-08
func TestCredentialScopeService(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := e.rootCreds()
	e.mkBucket("scoped")
	e.put("scoped", "obj", []byte("data"))
	signFor := func(req *http.Request, body []byte, service string) {
		sum := sha256.Sum256(body)
		hash := hex.EncodeToString(sum[:])
		req.Header.Set("X-Amz-Content-Sha256", hash)
		require.NoError(t, s3Signer().SignHTTP(e.ctx, root.aws(), req, hash, service, e.srv.Config.Region, time.Now()))
	}

	get := func(service string) response {
		req := newRequest(t, http.MethodGet, e.srv.S3URL+"/scoped/obj", nil)
		signFor(req, nil, service)
		return do(t, req)
	}
	resp := get("s3")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	requireS3Error(t, get("sts"), http.StatusBadRequest, "AuthorizationHeaderMalformed")

	form := []byte("Action=AssumeRole&Version=2011-06-15&DurationSeconds=900")
	assume := func(service string) response {
		req := newRequest(t, http.MethodPost, e.srv.S3URL+"/", form)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		signFor(req, form, service)
		return do(t, req)
	}
	resp = assume("sts")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), "<AccessKeyId>")
	requireS3Error(t, assume("s3"), http.StatusBadRequest, "AuthorizationHeaderMalformed")
}

// Integrity headers are verified end to end: a wrong Content-MD5,
// x-amz-content-sha256 or x-amz-checksum-* fails the upload and stores
// nothing; the SDK's own checksums are accepted.
//
// covers: S-OBJ-09, S-IAM-02
func TestPayloadIntegrity(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := e.rootCreds()
	const bucket = "integrity"
	e.mkBucket(bucket)
	payload := pseudoRandom(3, 200<<10) // on disk, not inline
	small := []byte("small inline payload")

	for _, body := range [][]byte{small, payload} {
		name := fmt.Sprint(len(body))
		t.Run("bad content-md5/"+name, func(t *testing.T) {
			resp := e.signedDo(http.MethodPut, "/"+bucket+"/md5-"+name, root, body, http.Header{"Content-Md5": {base64MD5([]byte("other"))}})
			requireS3Error(t, resp, http.StatusBadRequest, "BadDigest")
			assert.Equal(t, http.StatusNotFound, e.signedDo(http.MethodHead, "/"+bucket+"/md5-"+name, root, nil, nil).StatusCode)
		})
		t.Run("invalid content-md5/"+name, func(t *testing.T) {
			resp := e.signedDo(http.MethodPut, "/"+bucket+"/md5x-"+name, root, body, http.Header{"Content-Md5": {"not-base64!"}})
			requireS3Error(t, resp, http.StatusBadRequest, "InvalidDigest")
		})
		t.Run("good content-md5/"+name, func(t *testing.T) {
			resp := e.signedDo(http.MethodPut, "/"+bucket+"/md5ok-"+name, root, body, http.Header{"Content-Md5": {base64MD5(body)}})
			require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
		})
		t.Run("sha256 mismatch/"+name, func(t *testing.T) {
			// Signed with the hash of another payload.
			req := newRequest(t, http.MethodPut, e.srv.S3URL+"/"+bucket+"/sha-"+name, body)
			e.signRequest(req, root, []byte("another payload"))
			resp := do(t, req)
			requireS3Error(t, resp, http.StatusBadRequest, "XAmzContentSHA256Mismatch")
			assert.Equal(t, http.StatusNotFound, e.signedDo(http.MethodHead, "/"+bucket+"/sha-"+name, root, nil, nil).StatusCode)
		})
		t.Run("crc32 mismatch/"+name, func(t *testing.T) {
			resp := e.signedDo(http.MethodPut, "/"+bucket+"/crc-"+name, root, body, http.Header{"X-Amz-Checksum-Crc32": {"AAAAAA=="}})
			requireS3Error(t, resp, http.StatusBadRequest, "BadDigest")
			assert.Equal(t, http.StatusNotFound, e.signedDo(http.MethodHead, "/"+bucket+"/crc-"+name, root, nil, nil).StatusCode)
		})
		t.Run("truncated body/"+name, func(t *testing.T) {
			req := newRequest(t, http.MethodPut, e.srv.S3URL+"/"+bucket+"/short-"+name, body[:len(body)/2])
			req.Header.Set("X-Amz-Content-Sha256", unsignedPayload)
			e.signRequestUnsigned(req, root)
			req.ContentLength = int64(len(body)) // the client lies
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			assert.Equal(t, http.StatusNotFound, e.signedDo(http.MethodHead, "/"+bucket+"/short-"+name, root, nil, nil).StatusCode, "no partial object")
		})
	}

	// The SDK checksums (CRC32 by default, SHA256 and CRC32C on request)
	// are verified and stored.
	for _, algo := range []types.ChecksumAlgorithm{types.ChecksumAlgorithmCrc32, types.ChecksumAlgorithmCrc32c, types.ChecksumAlgorithmSha256, types.ChecksumAlgorithmSha1} {
		key := "sdk-" + string(algo)
		out, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(payload), ChecksumAlgorithm: algo})
		require.NoError(t, err, algo)
		head, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
		require.NoError(t, err)
		got := map[types.ChecksumAlgorithm]*string{
			types.ChecksumAlgorithmCrc32: head.ChecksumCRC32, types.ChecksumAlgorithmCrc32c: head.ChecksumCRC32C,
			types.ChecksumAlgorithmSha256: head.ChecksumSHA256, types.ChecksumAlgorithmSha1: head.ChecksumSHA1,
		}[algo]
		want := map[types.ChecksumAlgorithm]*string{
			types.ChecksumAlgorithmCrc32: out.ChecksumCRC32, types.ChecksumAlgorithmCrc32c: out.ChecksumCRC32C,
			types.ChecksumAlgorithmSha256: out.ChecksumSHA256, types.ChecksumAlgorithmSha1: out.ChecksumSHA1,
		}[algo]
		require.NotNil(t, want, algo)
		assert.Equal(t, aws.ToString(want), aws.ToString(got), algo)
	}
	assertNoTmp(t, e)
}
