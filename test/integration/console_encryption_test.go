package integration_test

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/testutil"
)

// TestConsoleUploadEncryption: the uploads of the console ask for SSE-S3 or
// SSE-KMS with the headers of the S3 API, in one request or multipart,
// authorized with the condition values of the S3 PutObject
// (s3:x-amz-server-side-encryption), and the details of an object report
// its encryption (D-OBJ-02, D-OBJ-06).
func TestConsoleUploadEncryption(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithKMS())
	ctx := e.ctx
	const bucket = "consolesse"
	e.mkBucket(bucket)
	root := e.consoleLogin(e.rootCreds())
	sse := func(algorithm, keyID string) http.Header {
		h := http.Header{"Content-Type": {"text/plain"}}
		if algorithm != "" {
			h.Set("X-Amz-Server-Side-Encryption", algorithm)
		}
		if keyID != "" {
			h.Set("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id", keyID)
		}
		return h
	}
	type details struct {
		Encryption *struct {
			Type     string `json:"type"`
			KMSKeyID string `json:"kmsKeyId"`
		} `json:"encryption"`
	}
	stat := func(key string) details {
		t.Helper()
		resp := root.do(http.MethodGet, "/buckets/"+bucket+"/objects/stat?key="+key, "")
		requireStatus(t, resp, http.StatusOK)
		return decodeJSON[details](t, resp)
	}

	// One request, SSE-S3.
	requireStatus(t, root.doRaw(http.MethodPost, "/buckets/"+bucket+"/objects/upload?key=s3.txt",
		[]byte("sealed with SSE-S3"), sse("AES256", "")), http.StatusCreated)
	head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("s3.txt")})
	require.NoError(t, err)
	assert.Equal(t, types.ServerSideEncryptionAes256, head.ServerSideEncryption)
	got := stat("s3.txt")
	require.NotNil(t, got.Encryption)
	assert.Equal(t, "SSE-S3", got.Encryption.Type)

	// Multipart, SSE-KMS with a named key.
	resp := root.doRaw(http.MethodPost, "/buckets/"+bucket+"/uploads?key=big.bin", []byte(`{}`),
		http.Header{"Content-Type": {"application/json"}, "X-Amz-Server-Side-Encryption": {"aws:kms"},
			"X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id": {testutil.KMSKeyName}})
	requireStatus(t, resp, http.StatusCreated)
	up := decodeJSON[struct {
		UploadID string `json:"uploadId"`
	}](t, resp)
	resp = root.doRaw(http.MethodPut, "/buckets/"+bucket+"/uploads/"+up.UploadID+"/parts/1?key=big.bin",
		[]byte("one part sealed with SSE-KMS"), http.Header{"Content-Type": {"application/octet-stream"}})
	requireStatus(t, resp, http.StatusOK)
	part := decodeJSON[struct {
		ETag string `json:"etag"`
	}](t, resp)
	requireStatus(t, root.do(http.MethodPost, "/buckets/"+bucket+"/uploads/"+up.UploadID+"/complete?key=big.bin",
		fmt.Sprintf(`{"parts":[{"partNumber":1,"etag":%q}]}`, part.ETag)), http.StatusOK)
	head, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("big.bin")})
	require.NoError(t, err)
	assert.Equal(t, types.ServerSideEncryptionAwsKms, head.ServerSideEncryption)
	assert.Contains(t, aws.ToString(head.SSEKMSKeyId), testutil.KMSKeyName)
	got = stat("big.bin")
	require.NotNil(t, got.Encryption)
	assert.Equal(t, "SSE-KMS", got.Encryption.Type)
	assert.Contains(t, got.Encryption.KMSKeyID, testutil.KMSKeyName)
	out, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("big.bin")})
	require.NoError(t, err)
	assert.Equal(t, "one part sealed with SSE-KMS", string(readAll(t, out)))

	// Without encryption the object reports none; an unknown algorithm or
	// a key without aws:kms is refused before anything is stored.
	requireStatus(t, root.doRaw(http.MethodPost, "/buckets/"+bucket+"/objects/upload?key=plain.txt",
		[]byte("plain"), sse("", "")), http.StatusCreated)
	assert.Nil(t, stat("plain.txt").Encryption)
	requireStatus(t, root.doRaw(http.MethodPost, "/buckets/"+bucket+"/objects/upload?key=bad.txt",
		[]byte("x"), sse("AES128", "")), http.StatusBadRequest)
	requireStatus(t, root.doRaw(http.MethodPost, "/buckets/"+bucket+"/objects/upload?key=bad.txt",
		[]byte("x"), sse("AES256", testutil.KMSKeyName)), http.StatusBadRequest)
	requireStatus(t, root.doRaw(http.MethodPost, "/buckets/"+bucket+"/objects/upload?key=bad.txt",
		[]byte("x"), sse("aws:kms", "no-such-key")), http.StatusBadRequest)

	// A bucket policy requiring encryption decides alike through the
	// console: the conditions are those of the S3 PutObject.
	user := e.createUser("sseuser", "sseuser-secret", "readwrite")
	e.putBucketPolicy(bucket, map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect": "Deny", "Principal": map[string]any{"AWS": []string{"*"}},
			"Action": []string{"s3:PutObject"}, "Resource": []string{"arn:aws:s3:::" + bucket + "/*"},
			"Condition": map[string]any{"Null": map[string]any{"s3:x-amz-server-side-encryption": "true"}},
		}},
	})
	console := e.consoleLogin(user)
	requireStatus(t, console.doRaw(http.MethodPost, "/buckets/"+bucket+"/objects/upload?key=user-plain.txt",
		[]byte("plain"), sse("", "")), http.StatusForbidden)
	requireStatus(t, console.doRaw(http.MethodPost, "/buckets/"+bucket+"/objects/upload?key=user-sealed.txt",
		[]byte("sealed"), sse("AES256", "")), http.StatusCreated)
	_, err = e.clientFor(user).PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("s3-plain.txt"),
		Body: strings.NewReader("plain")})
	assert.Equal(t, http.StatusForbidden, httpStatus(err), "the S3 API decides alike")
}

// TestConsoleKeepsEncryption (T-7.5): the console operations that copy an
// object (rename, restore of a version) keep its encryption: an SSE-S3 or
// SSE-KMS object in a bucket without default encryption is never
// rewritten in clear nor under another KMS key, and an SSE-C object, which
// the console cannot read, is refused with a clear error.
//
// covers: S-OBJ-13, D-OBJ-06
func TestConsoleKeepsEncryption(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithKMS(), testutil.WithTLS())
	c := e.tlsClient()
	ctx := e.ctx
	const bucket = "keep-sse"
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	_, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	require.NoError(t, err)
	const marker = "NEXO-CONSOLE-SSE-SECRET"
	data := markedData(marker, 5000)
	k := newSSECKey(9)
	puts := []struct {
		key string
		in  s3.PutObjectInput
	}{
		{"s3.bin", s3.PutObjectInput{ServerSideEncryption: types.ServerSideEncryptionAes256}},
		{"kms.bin", s3.PutObjectInput{ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: aws.String(testutil.KMSKeyName)}},
		{"c.bin", s3.PutObjectInput{SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(k.key), SSECustomerKeyMD5: aws.String(k.md5)}},
	}
	versions := map[string]string{}
	for _, p := range puts {
		in := p.in
		in.Bucket, in.Key, in.Body = aws.String(bucket), aws.String(p.key), bytes.NewReader(data)
		out, err := c.PutObject(ctx, &in)
		require.NoError(t, err, p.key)
		versions[p.key] = aws.ToString(out.VersionId)
	}
	console := e.consoleLogin(e.rootCreds())

	// Rename: the destination keeps the encryption of the source.
	for _, p := range []struct{ src, dst string }{{"s3.bin", "s3-renamed.bin"}, {"kms.bin", "kms-renamed.bin"}} {
		resp := console.do(http.MethodPost, "/buckets/"+bucket+"/objects/rename", fmt.Sprintf(`{"source":%q,"destination":%q}`, p.src, p.dst))
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	}
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("s3-renamed.bin")})
	require.NoError(t, err)
	assert.Equal(t, types.ServerSideEncryptionAes256, head.ServerSideEncryption)
	head, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("kms-renamed.bin")})
	require.NoError(t, err)
	assert.Equal(t, types.ServerSideEncryptionAwsKms, head.ServerSideEncryption)
	assert.Equal(t, testutil.KMSKeyName, aws.ToString(head.SSEKMSKeyId), "the KMS key of the source")
	resp := console.do(http.MethodPost, "/buckets/"+bucket+"/objects/rename", `{"source":"c.bin","destination":"c-renamed.bin"}`)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), "customer-provided key")

	// Restore of a version: the restored copy keeps its encryption.
	for _, key := range []string{"s3.bin", "kms.bin"} {
		resp := console.do(http.MethodPost, "/buckets/"+bucket+"/objects/restore?key="+key+"&versionId="+versions[key], "")
		require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
		head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		require.NoError(t, err)
		assert.NotEmpty(t, head.ServerSideEncryption, "%s: the restored version is encrypted", key)
		got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		require.NoError(t, err)
		assert.Equal(t, data, readAll(t, got))
	}
	resp = console.do(http.MethodPost, "/buckets/"+bucket+"/objects/restore?key=c.bin&versionId="+versions["c.bin"], "")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s", resp.Body)

	assert.False(t, drivesContain(t, e.srv, marker), "no console operation stored the plaintext")
}
