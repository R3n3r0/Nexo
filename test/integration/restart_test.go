package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/iam"
)

// tmpDir is the directory of the in-progress writes of the test drive.
func tmpDir(e *env) string {
	return filepath.Join(e.srv.Dir, "data", ".nexo.sys", "tmp")
}

// assertNoTmp checks that no in-progress write is left in the drive.
func assertNoTmp(t *testing.T, e *env) {
	t.Helper()
	entries, err := os.ReadDir(tmpDir(e))
	require.NoError(t, err)
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	assert.Empty(t, names, "temporary files left in %s", tmpDir(e))
}

// TS-I-17: after a restart on the same directory every object, version,
// bucket setting, IAM user, service account, named and bucket policy and
// in-progress multipart upload is still there.
//
// covers: S-STO-01, S-IAM-03, S-IAM-04, S-OPS-01
func TestRestartPreservesState(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	const bucket = "durable"
	e.mkBucket(bucket)
	_, err := e.root.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	require.NoError(t, err)
	_, err = e.root.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String(bucket),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("env"), Value: aws.String("test")}}}})
	require.NoError(t, err)
	small, large := []byte("inline object"), pseudoRandom(5, 3<<20)
	v1 := e.put(bucket, "small.txt", []byte("first version"))
	_ = v1
	e.put(bucket, "small.txt", small)
	e.put(bucket, "dir/large.bin", large)
	_, err = e.root.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("dir/large.bin"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("kind"), Value: aws.String("blob")}}}})
	require.NoError(t, err)
	e.putBucketPolicy(bucket, policyJSON(`{"Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::durable/dir/*"]}`))

	// IAM: a user with a custom policy, a group, a service account.
	e.setPolicy("durable-reader", policyJSON(`{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::durable","arn:aws:s3:::durable/*"]}`))
	user := e.createUser("durable-user", "durable-user-secret", "durable-reader")
	_, err = e.srv.IAM.CreateGroup(ctx, iam.CreateGroupInput{Name: "writers", Members: []string{user.AccessKey}, PolicyNames: []string{"writeonly"}})
	require.NoError(t, err)
	key, err := e.srv.IAM.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: e.identity(user.AccessKey), Name: "backup"})
	require.NoError(t, err)

	// A multipart upload in progress with one part.
	mpu, err := e.root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("pending.bin")})
	require.NoError(t, err)
	part1 := pseudoRandom(6, partSize)
	up1, err := e.root.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("pending.bin"), UploadId: mpu.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader(part1)})
	require.NoError(t, err)

	// Dynamic configuration, through the Admin API.
	csrf := adminLogin(t, e)
	resp := adminDo(t, e, csrf, http.MethodPut, "/api/v1/config/scanner", `{"settings":{"speed":"slow","cycle":"5m"}}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)

	s3URL := e.srv.S3URL
	e.srv.Restart(t)
	require.Equal(t, s3URL, e.srv.S3URL, "same address after the restart")
	assertNoTmp(t, e)

	// Data and versions.
	assert.Equal(t, small, e.get(bucket, "small.txt"))
	assert.True(t, bytes.Equal(large, e.get(bucket, "dir/large.bin")))
	versions, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String("small.txt")})
	require.NoError(t, err)
	assert.Len(t, versions.Versions, 2)
	tags, err := e.root.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("dir/large.bin")})
	require.NoError(t, err)
	require.Len(t, tags.TagSet, 1)
	assert.Equal(t, "blob", aws.ToString(tags.TagSet[0].Value))
	// Bucket settings.
	vs, err := e.root.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.Equal(t, types.BucketVersioningStatusEnabled, vs.Status)
	btags, err := e.root.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, btags.TagSet, 1)
	assert.Equal(t, http.StatusOK, e.anonStatus(http.MethodGet, "/"+bucket+"/dir/large.bin", nil), "the bucket policy survives")
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/"+bucket+"/small.txt", nil))

	// IAM.
	got, err := e.srv.IAM.GetUser(ctx, user.AccessKey)
	require.NoError(t, err)
	assert.Equal(t, []string{"durable-reader"}, got.PolicyNames)
	assert.Equal(t, []string{"writers"}, got.Groups)
	named, err := e.srv.Policies.GetPolicy(ctx, "durable-reader")
	require.NoError(t, err)
	assert.Len(t, named.Statement, 1)
	ents, err := e.srv.Policies.Entities(ctx, "durable-reader")
	require.NoError(t, err)
	assert.Equal(t, []string{user.AccessKey}, ents.Users)
	assert.Empty(t, ents.Groups)
	uc := e.clientFor(user)
	assert.Equal(t, small, getWith(t, ctx, uc, bucket, "small.txt"), "custom policy")
	_, err = uc.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("by-user.txt"), Body: strings.NewReader("x")})
	assert.NoError(t, err, "group policy")
	_, err = uc.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("by-user.txt")})
	assert.Equal(t, "AccessDenied", apiCode(err))
	sa := e.client(key.AccessKey, key.SecretKey, "")
	assert.Equal(t, small, getWith(t, ctx, sa, bucket, "small.txt"), "service account")
	_, err = e.client(e.srv.RootUser, "wrong-password", "").ListBuckets(ctx, &s3.ListBucketsInput{})
	assert.Equal(t, "SignatureDoesNotMatch", apiCode(err))

	// Configuration.
	csrf = adminLogin(t, e)
	resp = adminDo(t, e, csrf, http.MethodGet, "/api/v1/config/scanner", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	var subsystem struct {
		Name     string            `json:"name"`
		Settings map[string]string `json:"settings"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &subsystem))
	assert.Equal(t, map[string]string{"speed": "slow", "cycle": "5m"}, subsystem.Settings)

	// The multipart upload continues.
	parts, err := e.root.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(bucket), Key: aws.String("pending.bin"), UploadId: mpu.UploadId})
	require.NoError(t, err)
	require.Len(t, parts.Parts, 1)
	assert.Equal(t, aws.ToString(up1.ETag), aws.ToString(parts.Parts[0].ETag))
	up2, err := e.root.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("pending.bin"), UploadId: mpu.UploadId,
		PartNumber: aws.Int32(2), Body: strings.NewReader("tail")})
	require.NoError(t, err)
	_, err = e.root.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("pending.bin"), UploadId: mpu.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: up1.ETag}, {PartNumber: aws.Int32(2), ETag: up2.ETag},
		}}})
	require.NoError(t, err)
	assert.True(t, bytes.Equal(append(part1, []byte("tail")...), e.get(bucket, "pending.bin")))
}

// adminLogin opens a console session as root and returns its CSRF token
// (the session cookie is in the server client's jar).
func adminLogin(t *testing.T, e *env) string {
	t.Helper()
	body := fmt.Sprintf(`{"accessKey":%q,"secretKey":%q}`, e.srv.RootUser, e.srv.RootPassword)
	resp, err := e.srv.Client.Post(e.srv.ConsoleURL+"/api/v1/auth/login", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", data)
	var login struct {
		CSRFToken string `json:"csrfToken"`
	}
	require.NoError(t, json.Unmarshal(data, &login))
	require.NotEmpty(t, login.CSRFToken)
	return login.CSRFToken
}

// adminDo sends an Admin API request with the console session.
func adminDo(t *testing.T, e *env, csrf, method, path, body string) response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.ConsoleURL+path, rd)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	return doWith(t, e.srv.Client, req)
}

// blockingReader serves size bytes but blocks after half of them until
// its context ends, as a client that dies mid-upload.
type blockingReader struct {
	ctx      context.Context
	size     int64
	sent     int64
	halfway  chan struct{}
	signaled sync.Once
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if b.sent >= b.size/2 {
		b.signaled.Do(func() { close(b.halfway) })
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	n := int64(len(p))
	if rest := b.size/2 - b.sent; n > rest {
		n = rest
	}
	for i := range p[:n] {
		p[i] = byte(b.sent + int64(i))
	}
	b.sent += n
	return int(n), nil
}

// TS-I-18: a client dying in the middle of a large PUT leaves no partial
// object (nor a new version of an existing one), and a restart removes any
// temporary file a crash could have left.
//
// covers: S-STO-01, S-OBJ-06
func TestInterruptedUpload(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "crash"
	e.mkBucket(bucket)
	previous := []byte("the previous version")
	e.put(bucket, "existing.bin", previous)

	for _, key := range []string{"new.bin", "existing.bin"} {
		ctx, cancel := context.WithCancel(context.Background())
		body := &blockingReader{ctx: ctx, size: 64 << 20, halfway: make(chan struct{})}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, e.srv.S3URL+"/"+bucket+"/"+key, body)
		require.NoError(t, err)
		req.ContentLength = body.size
		req.Header.Set("X-Amz-Content-Sha256", unsignedPayload)
		e.signRequestUnsigned(req, e.rootCreds())
		done := make(chan error, 1)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				err = errors.New("the upload completed: " + resp.Status)
			}
			done <- err
		}()
		select {
		case <-body.halfway:
		case <-time.After(30 * time.Second):
			t.Fatal("the upload did not start")
		}
		// The writer is mid-way: the object is not visible (or still the
		// previous version), and reading it is not blocked by the upload.
		start := time.Now()
		head, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		assert.Less(t, time.Since(start), 2*time.Second, "an in-flight upload does not block the readers")
		if key == "new.bin" {
			assert.Equal(t, http.StatusNotFound, httpStatus(err))
		} else {
			require.NoError(t, err)
			assert.Equal(t, int64(len(previous)), aws.ToInt64(head.ContentLength))
		}
		cancel()
		require.Error(t, <-done)

		// The server notices the broken body and cleans up.
		require.Eventually(t, func() bool {
			entries, err := os.ReadDir(tmpDir(e))
			return err == nil && len(entries) == 0
		}, 10*time.Second, 20*time.Millisecond, "the temporary data of the aborted upload is removed")
		_, err = e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if key == "new.bin" {
			assert.Equal(t, http.StatusNotFound, httpStatus(err), "no partial object")
		} else {
			assert.NoError(t, err)
			assert.Equal(t, previous, e.get(bucket, key), "the previous version is intact")
		}
	}
	list, err := e.root.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, list.Contents, 1)

	// The same for a part of a multipart upload.
	mpu, err := e.root.CreateMultipartUpload(e.ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("parts.bin")})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	body := &blockingReader{ctx: ctx, size: 32 << 20, halfway: make(chan struct{})}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		e.srv.S3URL+"/"+bucket+"/parts.bin?partNumber=1&uploadId="+aws.ToString(mpu.UploadId), body)
	require.NoError(t, err)
	req.ContentLength = body.size
	e.signRequestUnsigned(req, e.rootCreds())
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			err = errors.New("the part upload completed: " + resp.Status)
		}
		done <- err
	}()
	<-body.halfway
	cancel()
	require.Error(t, <-done)
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(tmpDir(e))
		return err == nil && len(entries) == 0
	}, 10*time.Second, 20*time.Millisecond, "the temporary data of the aborted part is removed")
	parts, err := e.root.ListParts(e.ctx, &s3.ListPartsInput{Bucket: aws.String(bucket), Key: aws.String("parts.bin"), UploadId: mpu.UploadId})
	require.NoError(t, err)
	assert.Empty(t, parts.Parts, "no partial part")
	_, err = e.root.AbortMultipartUpload(e.ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("parts.bin"), UploadId: mpu.UploadId})
	require.NoError(t, err)

	// A hard crash leaves temporary data behind: simulate it while the
	// server is down, the restart removes it.
	e.srv.Stop(t)
	leftover := filepath.Join(tmpDir(e), "0192f2c4-dead-7000-8000-000000000000")
	require.NoError(t, os.MkdirAll(filepath.Join(leftover, "data-dir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(leftover, "data-dir", "part.1"), pseudoRandom(1, 1<<20), 0o644))
	e.srv.Restart(t)
	assertNoTmp(t, e)
	assert.Equal(t, previous, e.get(bucket, "existing.bin"))
	_, err = e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("new.bin")})
	assert.Equal(t, http.StatusNotFound, httpStatus(err))
}
