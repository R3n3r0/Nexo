package integration_test

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/testutil"
)

// markedData returns n bytes made of a known marker: a plaintext leak
// shows in any file of the drives (the data shards of the erasure coding
// are plain slices of the stored content).
func markedData(marker string, n int) []byte {
	return bytes.Repeat([]byte(marker+";"), n/(len(marker)+1)+1)[:n]
}

// drivesContain reports whether a file of the drives of the deployment
// contains needle.
func drivesContain(t *testing.T, srv *testutil.Server, needle string) bool {
	t.Helper()
	found := false
	for _, dir := range srv.DriveDirs() {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || found {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					return nil // removed meanwhile (temporary files)
				}
				return err
			}
			found = bytes.Contains(data, []byte(needle))
			return nil
		})
		require.NoError(t, err)
	}
	return found
}

// tlsClient returns a root S3 client trusting the test CA of a TLS
// deployment (testutil.WithTLS).
func (e *env) tlsClient() *s3.Client {
	e.t.Helper()
	require.NotNil(e.t, e.srv.TLS, "the deployment is not served over TLS")
	cfg := e.sdkConfig(e.rootCreds())
	tr, _ := http.DefaultTransport.(*http.Transport)
	tr = tr.Clone()
	tr.TLSClientConfig = e.srv.TLS.Clone()
	cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second, Transport: tr}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(e.srv.S3URL)
		o.UsePathStyle = true
	})
}

// sseCKey is an SSE-C key with its base64 forms.
type sseCKey struct {
	key, md5 string
}

func newSSECKey(fill byte) sseCKey {
	raw := bytes.Repeat([]byte{fill}, 32)
	sum := md5.Sum(raw)
	return sseCKey{key: base64.StdEncoding.EncodeToString(raw), md5: base64.StdEncoding.EncodeToString(sum[:])}
}

func readAll(t *testing.T, out *s3.GetObjectOutput) []byte {
	t.Helper()
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	return data
}

// TestServerSideEncryptionKMS (docs/05 T-7.5): SSE-S3 and SSE-KMS with the
// builtin KMS: round trip, ranges across the 64 KiB packages, multipart,
// the plaintext sizes in HEAD and listings, and no plaintext on the drives.
func TestServerSideEncryptionKMS(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithKMS())
	const bucket = "sse-kms"
	e.mkBucket(bucket)
	ctx := e.ctx
	const marker = "NEXO-SSE-KMS-SECRET"
	data := markedData(marker, 300<<10)

	put, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("s3.bin"), Body: bytes.NewReader(data),
		ServerSideEncryption: types.ServerSideEncryptionAes256})
	require.NoError(t, err)
	assert.Equal(t, types.ServerSideEncryptionAes256, put.ServerSideEncryption)
	sum := md5.Sum(data)
	assert.Equal(t, fmt.Sprintf(`"%x"`, sum), aws.ToString(put.ETag), "SSE-S3 keeps the MD5 ETag")

	got, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("s3.bin")})
	require.NoError(t, err)
	assert.Equal(t, types.ServerSideEncryptionAes256, got.ServerSideEncryption)
	assert.Equal(t, data, readAll(t, got))
	for _, rg := range [][2]int{{0, 0}, {65530, 65545}, {65536, 131072}, {len(data) - 10, len(data) - 1}} {
		got, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("s3.bin"), Range: aws.String(fmt.Sprintf("bytes=%d-%d", rg[0], rg[1]))})
		require.NoError(t, err)
		assert.Equal(t, data[rg[0]:rg[1]+1], readAll(t, got), "range %v", rg)
		assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", rg[0], rg[1], len(data)), aws.ToString(got.ContentRange))
	}
	head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("s3.bin")})
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), aws.ToInt64(head.ContentLength), "HEAD reports the plaintext size")
	list, err := e.root.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, list.Contents, 1)
	assert.Equal(t, int64(len(data)), aws.ToInt64(list.Contents[0].Size), "listings report the plaintext size")

	// SSE-KMS with the default key, by name and context.
	kmsCtx := base64.StdEncoding.EncodeToString([]byte(`{"department":"finance"}`))
	put, err = e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("kms.bin"), Body: bytes.NewReader(data),
		ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: aws.String(testutil.KMSKeyName), SSEKMSEncryptionContext: aws.String(kmsCtx)})
	require.NoError(t, err)
	assert.Equal(t, types.ServerSideEncryptionAwsKms, put.ServerSideEncryption)
	assert.Equal(t, testutil.KMSKeyName, aws.ToString(put.SSEKMSKeyId))
	assert.NotEqual(t, fmt.Sprintf(`"%x"`, sum), aws.ToString(put.ETag), "SSE-KMS does not reveal the MD5")
	head, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("kms.bin")})
	require.NoError(t, err)
	assert.Equal(t, types.ServerSideEncryptionAwsKms, head.ServerSideEncryption)
	assert.Equal(t, testutil.KMSKeyName, aws.ToString(head.SSEKMSKeyId))
	got, err = e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("kms.bin")})
	require.NoError(t, err)
	assert.Equal(t, data, readAll(t, got))

	_, err = e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("bad.bin"), Body: bytes.NewReader(data),
		ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: aws.String("no-such-key")})
	assert.Equal(t, "KMS.NotFoundException", apiCode(err))
	assert.Equal(t, http.StatusBadRequest, httpStatus(err))

	// Multipart: every part encrypted, ranges across the parts.
	up, err := e.root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("mpu.bin"),
		ServerSideEncryption: types.ServerSideEncryptionAes256})
	require.NoError(t, err)
	assert.Equal(t, types.ServerSideEncryptionAes256, up.ServerSideEncryption)
	part1, part2 := markedData(marker, partSize), markedData(marker, 70<<10)
	var parts []types.CompletedPart
	for i, p := range [][]byte{part1, part2} {
		out, err := e.root.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("mpu.bin"), UploadId: up.UploadId,
			PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(p)})
		require.NoError(t, err)
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.ETag})
	}
	_, err = e.root.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("mpu.bin"), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	require.NoError(t, err)
	whole := append(append([]byte(nil), part1...), part2...)
	got, err = e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("mpu.bin")})
	require.NoError(t, err)
	require.True(t, bytes.Equal(whole, readAll(t, got)))
	got, err = e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("mpu.bin"), Range: aws.String(fmt.Sprintf("bytes=%d-%d", partSize-5, partSize+5))})
	require.NoError(t, err)
	assert.Equal(t, whole[partSize-5:partSize+6], readAll(t, got))
	head, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("mpu.bin")})
	require.NoError(t, err)
	assert.Equal(t, int64(len(whole)), aws.ToInt64(head.ContentLength))

	assert.False(t, drivesContain(t, e.srv, marker), "the drives hold no plaintext of the encrypted objects")
	// The scan finds the plaintext of an unencrypted object.
	e.put(bucket, "plain.bin", markedData("NEXO-PLAINTEXT-CONTROL", 300<<10))
	assert.True(t, drivesContain(t, e.srv, "NEXO-PLAINTEXT-CONTROL"))
}

// TestServerSideEncryptionCustomerKeys (docs/05 T-7.5): SSE-C over TLS:
// the key is needed for every read (none: 400, another one: 403), a copy
// re-keys an object (key rotation, SSE-C to SSE-S3), multipart uploads
// need the key with every part, and the drives hold no plaintext.
func TestServerSideEncryptionCustomerKeys(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithKMS(), testutil.WithTLS())
	c := e.tlsClient()
	ctx := e.ctx
	const bucket = "sse-c"
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	const marker = "NEXO-SSE-C-SECRET"
	data := markedData(marker, 200<<10)
	k1, k2 := newSSECKey(1), newSSECKey(2)

	put, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("c.bin"), Body: bytes.NewReader(data),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(k1.key), SSECustomerKeyMD5: aws.String(k1.md5)})
	require.NoError(t, err)
	assert.Equal(t, k1.md5, aws.ToString(put.SSECustomerKeyMD5))
	sum := md5.Sum(data)
	assert.NotEqual(t, fmt.Sprintf(`"%x"`, sum), aws.ToString(put.ETag), "SSE-C does not reveal the MD5")

	get := func(key string, k *sseCKey, rng string) (*s3.GetObjectOutput, error) {
		in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
		if k != nil {
			in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5 = aws.String("AES256"), aws.String(k.key), aws.String(k.md5)
		}
		if rng != "" {
			in.Range = aws.String(rng)
		}
		return c.GetObject(ctx, in)
	}
	got, err := get("c.bin", &k1, "")
	require.NoError(t, err)
	assert.Equal(t, k1.md5, aws.ToString(got.SSECustomerKeyMD5))
	assert.Equal(t, data, readAll(t, got))
	got, err = get("c.bin", &k1, "bytes=65530-70000")
	require.NoError(t, err)
	assert.Equal(t, data[65530:70001], readAll(t, got))

	_, err = get("c.bin", nil, "")
	assert.Equal(t, http.StatusBadRequest, httpStatus(err), "no key")
	assert.Equal(t, "InvalidRequest", apiCode(err))
	_, err = get("c.bin", &k2, "")
	assert.Equal(t, http.StatusForbidden, httpStatus(err), "another key")
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("c.bin")})
	assert.Equal(t, http.StatusBadRequest, httpStatus(err), "HEAD needs the key too")
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("c.bin"),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(k1.key), SSECustomerKeyMD5: aws.String(k1.md5)})
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), aws.ToInt64(head.ContentLength))

	// Key rotation: a copy onto itself with a new key.
	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String("c.bin"), CopySource: aws.String(bucket + "/c.bin"),
		CopySourceSSECustomerAlgorithm: aws.String("AES256"), CopySourceSSECustomerKey: aws.String(k1.key), CopySourceSSECustomerKeyMD5: aws.String(k1.md5),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(k2.key), SSECustomerKeyMD5: aws.String(k2.md5)})
	require.NoError(t, err)
	got, err = get("c.bin", &k2, "")
	require.NoError(t, err)
	assert.Equal(t, data, readAll(t, got))
	_, err = get("c.bin", &k1, "")
	assert.Equal(t, http.StatusForbidden, httpStatus(err), "the old key no longer opens it")

	// SSE-C to SSE-S3.
	cp, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String("s3.bin"), CopySource: aws.String(bucket + "/c.bin"),
		CopySourceSSECustomerAlgorithm: aws.String("AES256"), CopySourceSSECustomerKey: aws.String(k2.key), CopySourceSSECustomerKeyMD5: aws.String(k2.md5),
		ServerSideEncryption: types.ServerSideEncryptionAes256})
	require.NoError(t, err)
	assert.Equal(t, types.ServerSideEncryptionAes256, cp.ServerSideEncryption)
	got, err = get("s3.bin", nil, "")
	require.NoError(t, err)
	assert.Equal(t, data, readAll(t, got))

	// Multipart with SSE-C: every part carries the key.
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("mpu.bin"),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(k1.key), SSECustomerKeyMD5: aws.String(k1.md5)})
	require.NoError(t, err)
	_, err = c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("mpu.bin"), UploadId: up.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader([]byte("no key"))})
	assert.Equal(t, http.StatusBadRequest, httpStatus(err), "a part without the key")
	part1, part2 := markedData(marker, partSize), markedData(marker, 1000)
	var parts []types.CompletedPart
	for i, p := range [][]byte{part1, part2} {
		out, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("mpu.bin"), UploadId: up.UploadId,
			PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(p),
			SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(k1.key), SSECustomerKeyMD5: aws.String(k1.md5)})
		require.NoError(t, err)
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.ETag})
	}
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("mpu.bin"), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	require.NoError(t, err)
	got, err = get("mpu.bin", &k1, "")
	require.NoError(t, err)
	require.True(t, bytes.Equal(append(append([]byte(nil), part1...), part2...), readAll(t, got)))

	assert.False(t, drivesContain(t, e.srv, marker), "the drives hold no plaintext of the encrypted objects")
	assert.False(t, drivesContain(t, e.srv, k1.key), "nor the customer keys")
}

// TestBucketDefaultEncryption (S-BKT-11): the default encryption of a
// bucket applies to the writes without encryption headers, the S3 ones
// and those proxied by the console; without KMS it is refused, and so is
// SSE-S3.
func TestBucketDefaultEncryption(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithKMS())
	ctx := e.ctx
	const bucket = "default-enc"
	e.mkBucket(bucket)
	_, err := e.root.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{Bucket: aws.String(bucket),
		ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
			ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256},
		}}}})
	require.NoError(t, err)
	enc, err := e.root.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, enc.ServerSideEncryptionConfiguration.Rules, 1)
	assert.Equal(t, types.ServerSideEncryptionAes256, enc.ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault.SSEAlgorithm)

	const marker = "NEXO-DEFAULT-ENC-SECRET"
	data := markedData(marker, 200<<10)
	e.put(bucket, "s3.bin", data)
	head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("s3.bin")})
	require.NoError(t, err)
	assert.Equal(t, types.ServerSideEncryptionAes256, head.ServerSideEncryption, "encrypted by default")

	// The uploads and downloads of the console go through the same layer.
	con := e.consoleLogin(e.rootCreds())
	resp := con.doRaw(http.MethodPost, "/buckets/"+bucket+"/objects/upload?key=console.bin", data, http.Header{"Content-Type": {"application/octet-stream"}})
	require.Less(t, resp.StatusCode, 300, "%s", resp.Body)
	resp = con.do(http.MethodGet, "/buckets/"+bucket+"/objects/download?key=console.bin", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.True(t, bytes.Equal(data, resp.Body), "the console download is decrypted")
	head, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("console.bin")})
	require.NoError(t, err)
	assert.Equal(t, types.ServerSideEncryptionAes256, head.ServerSideEncryption)
	assert.Equal(t, data, e.get(bucket, "console.bin"))
	assert.False(t, drivesContain(t, e.srv, marker))

	_, err = e.root.DeleteBucketEncryption(ctx, &s3.DeleteBucketEncryptionInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	e.put(bucket, "plain.bin", []byte("plain"))
	head, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("plain.bin")})
	require.NoError(t, err)
	assert.Empty(t, head.ServerSideEncryption)
}

// TestEncryptionWithoutKMS: without KMS, SSE-S3, SSE-KMS and the default
// encryption are refused with a clear error and nothing is written.
func TestEncryptionWithoutKMS(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	const bucket = "no-kms"
	e.mkBucket(bucket)
	_, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("k"), Body: strings.NewReader("data"),
		ServerSideEncryption: types.ServerSideEncryptionAes256})
	assert.Equal(t, http.StatusNotImplemented, httpStatus(err))
	assert.Equal(t, "NotImplemented", apiCode(err))
	_, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("k")})
	assert.Equal(t, http.StatusNotFound, httpStatus(err), "nothing written")
	_, err = e.root.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{Bucket: aws.String(bucket),
		ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
			ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256},
		}}}})
	assert.Equal(t, http.StatusNotImplemented, httpStatus(err))
}

// TestKMSAdminAPI: the Admin API reports the KMS in /info and /kms/status
// and creates keys (administrators only); a created key serves SSE-KMS
// and survives a restart.
func TestKMSAdminAPI(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithKMS())
	ctx := e.ctx
	root := e.consoleLogin(e.rootCreds())

	resp := root.do(http.MethodGet, "/kms/status", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	var status struct {
		Configured   bool   `json:"configured"`
		Backend      string `json:"backend"`
		DefaultKeyID string `json:"defaultKeyId"`
		Online       bool   `json:"online"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &status))
	assert.True(t, status.Configured)
	assert.Equal(t, "builtin", status.Backend)
	assert.Equal(t, testutil.KMSKeyName, status.DefaultKeyID)
	assert.True(t, status.Online)

	resp = root.do(http.MethodPost, "/kms/keys", `{"id":"tenant-a"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
	resp = root.do(http.MethodPost, "/kms/keys", `{"id":"tenant-a"}`)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "%s", resp.Body)
	resp = root.do(http.MethodPost, "/kms/keys", `{"id":"bad/name"}`)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s", resp.Body)

	// Only the administrators create keys.
	user := e.createUser("kmsuser", "kmsuser-secret", "readwrite")
	resp = e.consoleLogin(user).do(http.MethodPost, "/kms/keys", `{"id":"tenant-b"}`)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s", resp.Body)

	resp = root.do(http.MethodGet, "/kms/keys", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	var keys struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &keys))
	var ids []string
	for _, k := range keys.Items {
		ids = append(ids, k.ID)
	}
	assert.ElementsMatch(t, []string{"tenant-a", testutil.KMSKeyName}, ids)

	resp = root.do(http.MethodGet, "/info", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	var info struct {
		KMS struct {
			Configured bool     `json:"configured"`
			Keys       []string `json:"keys"`
		} `json:"kms"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &info))
	assert.True(t, info.KMS.Configured)
	assert.Contains(t, info.KMS.Keys, "tenant-a")

	const bucket = "kms-keys"
	e.mkBucket(bucket)
	put := func(key string) error {
		_, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("tenant data"),
			ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: aws.String("tenant-a")})
		return err
	}
	require.NoError(t, put("before"))
	e.srv.Restart(t)
	require.NoError(t, put("after"), "the created key survives a restart")
	assert.Equal(t, []byte("tenant data"), e.get(bucket, "before"))
	assert.Equal(t, []byte("tenant data"), e.get(bucket, "after"))
}
