package cli_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// sdkEnv is a Nexo server started in-process and the aws-sdk-go-v2 client
// of nexoctl for its root user.
type sdkEnv struct {
	srv     *testutil.Server
	factory cli.S3Factory
	alias   cli.Alias
	client  cli.S3Client
}

func newSDKEnv(t *testing.T) *sdkEnv {
	t.Helper()
	srv := testutil.StartServer(t)
	e := &sdkEnv{
		srv:     srv,
		factory: cli.NewS3Factory(cli.S3FactoryDeps{}),
		alias:   cli.Alias{Name: "test", URL: srv.S3URL, AccessKey: srv.RootUser, SecretKey: srv.RootPassword, Path: "on"},
	}
	e.client = e.clientWith(t, cli.ClientOptions{})
	return e
}

func (e *sdkEnv) clientWith(t *testing.T, opts cli.ClientOptions) cli.S3Client {
	t.Helper()
	c, err := e.factory.Client(context.Background(), e.alias, opts)
	require.NoError(t, err)
	return c
}

func random(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func readAll(t *testing.T, c cli.S3Client, bucket, key string, opts cli.GetOptions) []byte {
	t.Helper()
	body, _, err := c.Get(context.Background(), bucket, key, opts)
	require.NoError(t, err)
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	return data
}

func uploads(t *testing.T, c cli.S3Client, bucket string) []cli.UploadInfo {
	t.Helper()
	var out []cli.UploadInfo
	for u, err := range c.ListUploads(context.Background(), bucket, "") {
		require.NoError(t, err)
		out = append(out, u)
	}
	return out
}

// onlyReader hides the io.ReaderAt of a reader.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

// covers: C-OBJ-01, C-OBJ-03, C-OBJ-05, TCL-U-06
func TestSDKClientObjects(t *testing.T) {
	ctx := context.Background()
	e := newSDKEnv(t)
	c := e.client
	require.NoError(t, c.MakeBucket(ctx, "objects", cli.MakeBucketOptions{}))
	assert.ErrorIs(t, c.MakeBucket(ctx, "objects", cli.MakeBucketOptions{}), domain.ErrBucketAlreadyExists)
	assert.ErrorIs(t, c.MakeBucket(ctx, "x", cli.MakeBucketOptions{}), domain.ErrInvalidBucketName)
	ok, err := c.BucketExists(ctx, "objects")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = c.BucketExists(ctx, "missing")
	require.NoError(t, err)
	assert.False(t, ok)
	buckets, err := c.ListBuckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, "objects", buckets[0].Name)

	// Single PUT with metadata, content type and tags.
	res, err := c.Put(ctx, "objects", "dir/small.txt", strings.NewReader("hello"), cli.PutOptions{
		Size: 5, ContentType: "text/plain", Metadata: map[string]string{"owner": "ops"}, Tags: map[string]string{"t": "1"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.Parts)
	info, err := c.Stat(ctx, "objects", "dir/small.txt", cli.StatOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(5), info.Size)
	assert.Equal(t, "text/plain", info.ContentType)
	assert.Equal(t, map[string]string{"owner": "ops"}, info.Metadata)
	assert.Equal(t, res.ETag, info.ETag)
	tags, err := c.GetTags(ctx, cli.ObjectRef{Bucket: "objects", Key: "dir/small.txt"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"t": "1"}, tags)

	// Multipart from a random-access body, from a sequential body of known
	// size and from a stream of unknown size.
	payload := random(t, 12<<20+123)
	var progressed atomic.Int64
	res, err = c.Put(ctx, "objects", "big/section", bytes.NewReader(payload), cli.PutOptions{
		Size: int64(len(payload)), PartSize: cli.MinPartSize, Parallel: 3, Progress: func(n int64) { progressed.Add(n) },
	})
	require.NoError(t, err)
	assert.Equal(t, 3, res.Parts)
	assert.Equal(t, int64(len(payload)), progressed.Load(), "Progress is called from the workers")
	assert.True(t, strings.HasSuffix(res.ETag, "-3"), res.ETag)
	res, err = c.Put(ctx, "objects", "big/sequential", onlyReader{bytes.NewReader(payload)}, cli.PutOptions{Size: int64(len(payload)), PartSize: cli.MinPartSize})
	require.NoError(t, err)
	assert.Equal(t, 3, res.Parts)
	res, err = c.Put(ctx, "objects", "big/stream", onlyReader{bytes.NewReader(payload)}, cli.PutOptions{Size: -1, PartSize: cli.MinPartSize, Parallel: 2})
	require.NoError(t, err)
	assert.Equal(t, 3, res.Parts)
	assert.Equal(t, int64(len(payload)), res.Size)
	res, err = c.Put(ctx, "objects", "small-stream", onlyReader{strings.NewReader("tiny")}, cli.PutOptions{Size: -1})
	require.NoError(t, err)
	assert.Zero(t, res.Parts, "a short stream is a single PUT")
	for _, k := range []string{"big/section", "big/sequential", "big/stream"} {
		assert.Equal(t, payload, readAll(t, c, "objects", k, cli.GetOptions{}), k)
	}
	assert.Equal(t, payload[100:200], readAll(t, c, "objects", "big/stream", cli.GetOptions{Range: &cli.ByteRange{Offset: 100, Length: 100}}))
	assert.Equal(t, payload[len(payload)-10:], readAll(t, c, "objects", "big/stream", cli.GetOptions{Range: &cli.ByteRange{Offset: int64(len(payload) - 10)}}))
	_, err = c.Put(ctx, "objects", "short", onlyReader{strings.NewReader("abc")}, cli.PutOptions{Size: 6 << 20, PartSize: cli.MinPartSize})
	assert.Error(t, err, "a body shorter than its size fails")
	assert.Empty(t, uploads(t, c, "objects"), "and its upload is aborted")
	_, err = c.Put(ctx, "objects", "bad", strings.NewReader("x"), cli.PutOptions{Size: 1, PartSize: 1})
	assert.ErrorIs(t, err, cli.ErrUsage)

	// Server-side copies.
	_, err = c.Copy(ctx, cli.ObjectRef{Bucket: "objects", Key: "dir/small.txt"}, cli.ObjectRef{Bucket: "objects", Key: "copy/keep.txt"}, cli.CopyOptions{})
	require.NoError(t, err)
	info, err = c.Stat(ctx, "objects", "copy/keep.txt", cli.StatOptions{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"owner": "ops"}, info.Metadata)
	_, err = c.Copy(ctx, cli.ObjectRef{Bucket: "objects", Key: "dir/small.txt"}, cli.ObjectRef{Bucket: "objects", Key: "copy/replace a?b.txt"},
		cli.CopyOptions{Metadata: map[string]string{"new": "1"}, Tags: map[string]string{"x": "y"}})
	require.NoError(t, err)
	info, err = c.Stat(ctx, "objects", "copy/replace a?b.txt", cli.StatOptions{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"new": "1"}, info.Metadata)
	assert.Equal(t, "text/plain", info.ContentType, "the content type survives a metadata replacement")
	_, err = c.Copy(ctx, cli.ObjectRef{Bucket: "objects", Key: "missing"}, cli.ObjectRef{Bucket: "objects", Key: "x"}, cli.CopyOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)

	// Listings.
	var keys []string
	for o, err := range c.List(ctx, "objects", cli.ListOptions{}) {
		require.NoError(t, err)
		if o.Prefix {
			keys = append(keys, o.Key+" (prefix)")
		} else {
			keys = append(keys, o.Key)
		}
	}
	assert.Equal(t, []string{"big/ (prefix)", "copy/ (prefix)", "dir/ (prefix)", "small-stream"}, keys)
	keys = nil
	for o, err := range c.List(ctx, "objects", cli.ListOptions{Prefix: "big/", Recursive: true}) {
		require.NoError(t, err)
		keys = append(keys, o.Key)
	}
	assert.Equal(t, []string{"big/section", "big/sequential", "big/stream"}, keys)
	for _, err := range c.List(ctx, "missing", cli.ListOptions{}) {
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	}

	// Batch removal.
	results, err := c.Remove(ctx, "objects", []cli.ObjectVersion{{Key: "big/section"}, {Key: "big/sequential"}, {Key: "never-existed"}}, cli.DeleteOptions{})
	require.NoError(t, err)
	require.Len(t, results, 3)
	for _, r := range results {
		assert.NoError(t, r.Err)
	}
	_, err = c.Stat(ctx, "objects", "big/section", cli.StatOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	assert.ErrorIs(t, c.RemoveBucket(ctx, "objects"), domain.ErrBucketNotEmpty)
}

// covers: C-OBJ-09, S-OBJ-10
func TestSDKClientVersionsTagsPolicies(t *testing.T) {
	ctx := context.Background()
	e := newSDKEnv(t)
	c := e.client
	require.NoError(t, c.MakeBucket(ctx, "vers", cli.MakeBucketOptions{}))
	v, err := c.GetVersioning(ctx, "vers")
	require.NoError(t, err)
	assert.Equal(t, cli.VersioningOff, v)
	require.NoError(t, c.SetVersioning(ctx, "vers", cli.VersioningEnabled))
	v, err = c.GetVersioning(ctx, "vers")
	require.NoError(t, err)
	assert.Equal(t, cli.VersioningEnabled, v)
	assert.ErrorIs(t, c.SetVersioning(ctx, "vers", "Bogus"), cli.ErrUsage)

	r1, err := c.Put(ctx, "vers", "doc", strings.NewReader("one"), cli.PutOptions{Size: 3})
	require.NoError(t, err)
	r2, err := c.Put(ctx, "vers", "doc", strings.NewReader("two"), cli.PutOptions{Size: 3})
	require.NoError(t, err)
	require.NotEqual(t, r1.VersionID, r2.VersionID)
	removed, err := c.Remove(ctx, "vers", []cli.ObjectVersion{{Key: "doc"}}, cli.DeleteOptions{})
	require.NoError(t, err)
	require.Len(t, removed, 1)
	assert.True(t, removed[0].DeleteMarker)
	assert.NotEmpty(t, removed[0].DeleteMarkerVersionID)

	var versions []cli.ObjectInfo
	for o, err := range c.List(ctx, "vers", cli.ListOptions{Versions: true, Recursive: true}) {
		require.NoError(t, err)
		versions = append(versions, o)
	}
	require.Len(t, versions, 3)
	assert.True(t, versions[0].DeleteMarker && versions[0].IsLatest, "latest first: the delete marker")
	assert.Equal(t, r2.VersionID, versions[1].VersionID)
	assert.Equal(t, r1.VersionID, versions[2].VersionID)
	assert.Equal(t, []byte("one"), readAll(t, c, "vers", "doc", cli.GetOptions{VersionID: r1.VersionID}))
	_, err = c.Stat(ctx, "vers", "doc", cli.StatOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)

	// Tags of a version and of the bucket.
	ref := cli.ObjectRef{Bucket: "vers", Key: "doc", VersionID: r1.VersionID}
	require.NoError(t, c.PutTags(ctx, ref, map[string]string{"a": "1"}))
	tags, err := c.GetTags(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1"}, tags)
	require.NoError(t, c.DeleteTags(ctx, ref))
	tags, err = c.GetTags(ctx, ref)
	require.NoError(t, err)
	assert.Empty(t, tags)
	bucketRef := cli.ObjectRef{Bucket: "vers"}
	tags, err = c.GetTags(ctx, bucketRef)
	require.NoError(t, err)
	assert.Empty(t, tags, "no tag set is an empty map")
	require.NoError(t, c.PutTags(ctx, bucketRef, map[string]string{"team": "ops"}))
	tags, err = c.GetTags(ctx, bucketRef)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"team": "ops"}, tags)
	require.NoError(t, c.DeleteTags(ctx, bucketRef))

	// Bucket policy.
	_, err = c.GetBucketPolicy(ctx, "vers")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	p, err := cli.ApplyAnonymousPreset(nil, "vers", "pub/", cli.AnonymousDownload)
	require.NoError(t, err)
	doc := mustJSON(t, p)
	require.NoError(t, c.PutBucketPolicy(ctx, "vers", doc))
	got, err := c.GetBucketPolicy(ctx, "vers")
	require.NoError(t, err)
	parsed, err := cli.ParseBucketPolicy(got)
	require.NoError(t, err)
	assert.Equal(t, cli.AnonymousDownload, cli.DetectAnonymousPreset(parsed, "vers", "pub/"))
	assert.ErrorIs(t, c.PutBucketPolicy(ctx, "vers", "{bad"), domain.ErrMalformedPolicy)
	require.NoError(t, c.DeleteBucketPolicy(ctx, "vers"))
	_, err = c.GetBucketPolicy(ctx, "vers")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
}

// covers: TCL-P-01, TCL-P-02, TCL-P-06, S-OBJ-08, S-OBJ-08b
func TestSDKClientPresignAndPostPolicy(t *testing.T) {
	ctx := context.Background()
	e := newSDKEnv(t)
	c := e.client
	require.NoError(t, c.MakeBucket(ctx, "share", cli.MakeBucketOptions{}))
	_, err := c.Put(ctx, "share", "file.txt", strings.NewReader("shared"), cli.PutOptions{Size: 6})
	require.NoError(t, err)
	httpc := &http.Client{Timeout: 10 * time.Second}
	do := func(method, url string, headers map[string]string, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		require.NoError(t, err)
		if body == "" {
			req.Body = http.NoBody
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := httpc.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(data)
	}

	get, err := c.Presign(ctx, cli.PresignInput{Method: "GET", Bucket: "share", Key: "file.txt", Expiry: time.Hour, ResponseContentDisposition: "attachment"})
	require.NoError(t, err)
	assert.Equal(t, "GET", get.Method)
	assert.Empty(t, get.Headers)
	assert.WithinDuration(t, time.Now().Add(time.Hour), get.ExpiresAt, 5*time.Second)
	code, body := do("GET", get.URL, nil, "")
	assert.Equal(t, 200, code)
	assert.Equal(t, "shared", body)

	head, err := c.Presign(ctx, cli.PresignInput{Method: "head", Bucket: "share", Key: "file.txt", Expiry: time.Minute})
	require.NoError(t, err)
	code, _ = do("HEAD", head.URL, nil, "")
	assert.Equal(t, 200, code)

	put, err := c.Presign(ctx, cli.PresignInput{Method: "PUT", Bucket: "share", Key: "up.txt", Expiry: time.Minute,
		Headers: map[string]string{"Content-Type": "text/plain", "x-amz-meta-origin": "link"}})
	require.NoError(t, err)
	assert.Equal(t, "text/plain", put.Headers["Content-Type"], "the content type is signed")
	assert.Equal(t, "link", put.Headers["X-Amz-Meta-Origin"])
	code, _ = do("PUT", put.URL, map[string]string{"Content-Type": "image/png", "x-amz-meta-origin": "link"}, "data")
	assert.Equal(t, 403, code, "another content type breaks the signature")
	code, _ = do("PUT", put.URL, put.Headers, "uploaded")
	assert.Equal(t, 200, code)
	info, err := c.Stat(ctx, "share", "up.txt", cli.StatOptions{})
	require.NoError(t, err)
	assert.Equal(t, "text/plain", info.ContentType)
	assert.Equal(t, "link", info.Metadata["origin"])

	del, err := c.Presign(ctx, cli.PresignInput{Method: "DELETE", Bucket: "share", Key: "up.txt", Expiry: time.Minute})
	require.NoError(t, err)
	code, _ = do("DELETE", del.URL, nil, "")
	assert.Equal(t, 204, code)
	_, err = c.Stat(ctx, "share", "up.txt", cli.StatOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)

	for _, bad := range []cli.PresignInput{
		{Method: "GET", Bucket: "share", Key: "k", Expiry: 0},
		{Method: "GET", Bucket: "share", Key: "k", Expiry: 8 * 24 * time.Hour},
		{Method: "POST", Bucket: "share", Key: "k", Expiry: time.Minute},
		{Method: "GET", Bucket: "share", Expiry: time.Minute},
		{Method: "GET", Bucket: "share", Key: "k", Expiry: time.Minute, Headers: map[string]string{"a": "b"}},
		{Method: "PUT", Bucket: "share", Key: "k", Expiry: time.Minute, Headers: map[string]string{"x-amz-acl": "public-read"}},
	} {
		_, err := c.Presign(ctx, bad)
		assert.ErrorIs(t, err, cli.ErrUsage, "%+v", bad)
	}

	// POST policy: an upload under the prefix within the size range.
	form, err := c.PostPolicy(ctx, cli.PostPolicyInput{Bucket: "share", KeyPrefix: "inbox/", Expiry: time.Hour, MaxSize: 1 << 10, ContentType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, e.srv.S3URL+"/share", form.URL)
	assert.Equal(t, "inbox/${filename}", form.Fields["key"])
	post := func(fields map[string]string, content string) int {
		t.Helper()
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for _, k := range cli.SortedKeys(fields) {
			require.NoError(t, w.WriteField(k, fields[k]))
		}
		fw, err := w.CreateFormFile("file", "note.txt")
		require.NoError(t, err)
		_, err = fw.Write([]byte(content))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		resp, err := httpc.Post(form.URL, w.FormDataContentType(), &buf)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, 204, post(form.Fields, "posted"))
	assert.Equal(t, []byte("posted"), readAll(t, c, "share", "inbox/note.txt", cli.GetOptions{}))
	assert.NotEqual(t, 204, post(form.Fields, strings.Repeat("x", 2<<10)), "too large")
	outside := map[string]string{}
	for k, v := range form.Fields {
		outside[k] = v
	}
	outside["key"] = "elsewhere/note.txt"
	assert.Equal(t, 403, post(outside, "x"), "key outside the prefix")
	_, err = c.PostPolicy(ctx, cli.PostPolicyInput{Bucket: "share", Expiry: time.Hour, MinSize: 10, MaxSize: 5})
	assert.ErrorIs(t, err, cli.ErrUsage)
}

// blockingReader returns size bytes then blocks until ctx is done.
type blockingReader struct {
	ctx  context.Context
	data []byte
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		<-r.ctx.Done()
		return 0, r.ctx.Err()
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// failingReader returns size bytes then an error.
type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// covers: TCL-E-04
func TestSDKClientAbortsInterruptedUploads(t *testing.T) {
	e := newSDKEnv(t)
	c := e.client
	require.NoError(t, c.MakeBucket(context.Background(), "abort", cli.MakeBucketOptions{}))

	// Ctrl-C: the context is canceled while the upload waits for data.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Put(ctx, "abort", "stream.bin", &blockingReader{ctx: ctx, data: random(t, 6<<20)}, cli.PutOptions{Size: -1, PartSize: cli.MinPartSize})
		done <- err
	}()
	require.Eventually(t, func() bool { return len(uploads(t, c, "abort")) == 1 }, 10*time.Second, 20*time.Millisecond)
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the upload did not stop")
	}
	var aborted *cli.AbortedUploadError
	require.ErrorAs(t, err, &aborted)
	assert.NoError(t, aborted.AbortErr)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, cli.ExitInterrupted, cli.ExitCode(err))
	assert.Empty(t, uploads(t, c, "abort"), "no upload left on the server")

	// A failing body aborts the upload too.
	boom := errors.New("disk read error")
	_, err = c.Put(context.Background(), "abort", "broken.bin", &failingReader{data: random(t, 7<<20), err: boom}, cli.PutOptions{Size: -1, PartSize: cli.MinPartSize})
	require.ErrorAs(t, err, &aborted)
	assert.ErrorIs(t, err, boom)
	assert.Empty(t, uploads(t, c, "abort"))
	_, err = c.Stat(context.Background(), "abort", "broken.bin", cli.StatOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)

	// Explicit abort of a dangling upload.
	assert.ErrorIs(t, c.AbortUpload(context.Background(), "abort", "k", "nope"), domain.ErrNoSuchUpload)
}

// covers: TCL-E-01, TCL-U-04, C-UX-01
func TestSDKClientCredentialsDebugAndLimits(t *testing.T) {
	ctx := context.Background()
	e := newSDKEnv(t)
	bad := e.alias
	bad.SecretKey = "wrong-secret"
	c, err := e.factory.Client(ctx, bad, cli.ClientOptions{})
	require.NoError(t, err)
	_, err = c.ListBuckets(ctx)
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)
	assert.Equal(t, cli.ExitAccess, cli.ExitCode(err))
	unknown := e.alias
	unknown.AccessKey = "nobody"
	c, err = e.factory.Client(ctx, unknown, cli.ClientOptions{})
	require.NoError(t, err)
	_, err = c.ListBuckets(ctx)
	assert.Equal(t, cli.ExitAccess, cli.ExitCode(err), "%v", err)
	_, err = e.factory.Client(ctx, cli.Alias{Name: "x", URL: "ftp://x", AccessKey: "a", SecretKey: "b"}, cli.ClientOptions{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	var dump bytes.Buffer
	debug := e.clientWith(t, cli.ClientOptions{Debug: &dump})
	require.NoError(t, debug.MakeBucket(ctx, "debug", cli.MakeBucketOptions{}))
	_, err = debug.Stat(ctx, "debug", "missing", cli.StatOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	out := dump.String()
	assert.Contains(t, out, "> PUT /debug HTTP/1.1")
	assert.Contains(t, out, "Signature=REDACTED")
	assert.Contains(t, out, "< HTTP/1.1 404 Not Found")
	assert.NotContains(t, out, e.srv.RootPassword)

	limited := e.clientWith(t, cli.ClientOptions{LimitUpload: 64 << 10})
	start := time.Now()
	_, err = limited.Put(ctx, "debug", "slow", bytes.NewReader(random(t, 96<<10)), cli.PutOptions{Size: 96 << 10})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 900*time.Millisecond, "96 KiB at 64 KiB/s")

	assert.True(t, cli.UsePathStyle(cli.Alias{URL: "http://localhost:9000", Path: "auto"}))
	assert.False(t, cli.UsePathStyle(cli.Alias{URL: "https://s3.eu-west-1.amazonaws.com", Path: "auto"}))
	assert.False(t, cli.UsePathStyle(cli.Alias{URL: "http://localhost", Path: "off"}))
	assert.True(t, cli.UsePathStyle(cli.Alias{URL: "https://s3.amazonaws.com", Path: "on"}))
	assert.Equal(t, "b/a%20b/c%3F.txt?versionId=v%2B1", cli.CopySource(cli.ObjectRef{Bucket: "b", Key: "a b/c?.txt", VersionID: "v+1"}))
	assert.Equal(t, "https://h/x?X-Amz-Signature=REDACTED&X-Amz-Credential=REDACTED&a=1", cli.RedactURL("https://h/x?X-Amz-Signature=abc&X-Amz-Credential=AK%2F1&a=1"))
	assert.Equal(t, "https://h/x", cli.RedactURL("https://h/x"))
}
