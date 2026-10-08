package integration_test

import (
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Keys with characters that need escaping round-trip through every request
// path: SDK put/get/head/list/copy/delete, presigned URLs and raw signed
// requests (SigV4 canonical URIs are not double-encoded for S3).
//
// covers: S-OBJ-01
func TestSpecialCharacterKeys(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "keys"
	e.mkBucket(bucket)
	keys := []string{
		"dir with space/file name.txt",
		"unicode/caffè-ü-日本.txt",
		"plus+sign/a+b.txt",
		"percent/100%.txt",
		"query?x=y&z#frag.txt",
		"folder/",
		"tilde~/x.txt",
		"semi;colon,comma=equals@at$dollar.txt",
		"quote'and\"double.txt",
		"brackets[1]{2}(3)!*.txt",
		"<tag>&amp;.xml",
		"  leading-spaces.txt",
	}
	ctx := e.ctx
	ps := e.presigner(e.rootCreds())
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			body := []byte("content of " + key)
			_, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(string(body)),
				Metadata: map[string]string{"source": "keys"}})
			require.NoError(t, err)
			assert.Equal(t, body, e.get(bucket, key))
			head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
			require.NoError(t, err)
			assert.Equal(t, int64(len(body)), aws.ToInt64(head.ContentLength))

			list, err := e.root.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(key)})
			require.NoError(t, err)
			require.NotEmpty(t, list.Contents)
			assert.Equal(t, key, aws.ToString(list.Contents[0].Key))

			// CopySource is URL-encoded by the caller.
			dst := "copies/" + key
			_, err = e.root.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String(dst),
				CopySource: aws.String(url.PathEscape(bucket + "/" + key))})
			require.NoError(t, err)
			assert.Equal(t, body, e.get(bucket, dst))

			get, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}, s3.WithPresignExpires(time.Minute))
			require.NoError(t, err)
			resp := presignedDo(t, http.MethodGet, get.URL, nil, nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
			assert.Equal(t, body, resp.Body)

			raw := e.signedDo(http.MethodGet, "/"+bucket+"/"+escapePath(key), e.rootCreds(), nil, nil)
			require.Equal(t, http.StatusOK, raw.StatusCode, "%s", raw.Body)
			assert.Equal(t, body, raw.Body)

			tags, err := e.root.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String(key)})
			require.NoError(t, err)
			assert.Empty(t, tags.TagSet)
		})
	}

	// Listing returns every key once, in byte order.
	var listed []string
	p := s3.NewListObjectsV2Paginator(e.root, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), MaxKeys: aws.Int32(4)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		require.NoError(t, err)
		for _, o := range page.Contents {
			if !strings.HasPrefix(aws.ToString(o.Key), "copies/") {
				listed = append(listed, aws.ToString(o.Key))
			}
		}
	}
	want := append([]string(nil), keys...)
	sort.Strings(want)
	assert.Equal(t, want, listed)

	// Keys that cannot map to a file path are refused (like MinIO).
	for _, key := range []string{"double//slash.txt", "/leading-slash.txt"} {
		_, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("x")})
		assert.Equal(t, "XNexoInvalidObjectName", apiCode(err), key)
		assert.Equal(t, http.StatusBadRequest, httpStatus(err), key)
	}

	// Batch delete with special keys.
	var ids []types.ObjectIdentifier
	for _, k := range keys {
		ids = append(ids, types.ObjectIdentifier{Key: aws.String(k)})
	}
	del, err := e.root.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: &types.Delete{Objects: ids}})
	require.NoError(t, err)
	assert.Empty(t, del.Errors)
	assert.Len(t, del.Deleted, len(keys))
	for _, k := range keys {
		_, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(k)})
		assert.Equal(t, http.StatusNotFound, httpStatus(err), k)
	}
}

// escapePath escapes a key for a URL path, keeping the slashes.
func escapePath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// Keys that would escape the bucket directory are refused or stored
// inside it: no file is ever written outside the bucket.
//
// covers: S-OBJ-01
func TestPathTraversalKeys(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "jail"
	e.mkBucket(bucket)
	drive := filepath.Join(e.srv.Dir, "data")
	before := listFiles(t, drive, bucket)
	for _, key := range []string{"../escape.txt", "a/../../escape.txt", "a/./b.txt", "..", ".", "a/..", "../.nexo.sys/config/x", "\\..\\win.txt", "nul\x00byte"} {
		t.Run(strings.ReplaceAll(key, "/", "_"), func(t *testing.T) {
			// The dot segments are sent as they are (no client-side cleaning).
			req := newRequest(t, http.MethodPut, e.srv.S3URL+"/"+bucket+"/"+escapePath(key), []byte("payload"))
			e.signRequest(req, e.rootCreds(), []byte("payload"))
			resp := do(t, req)
			if resp.StatusCode == http.StatusOK {
				// Stored: it must be readable under the same key, inside the bucket.
				got := e.signedDo(http.MethodGet, "/"+bucket+"/"+escapePath(key), e.rootCreds(), nil, nil)
				assert.Equal(t, http.StatusOK, got.StatusCode)
				return
			}
			assert.Contains(t, []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed}, resp.StatusCode, "%s", resp.Body)
		})
	}
	after := listFiles(t, drive, bucket)
	assert.Equal(t, before, after, "no file outside the bucket directory changed")
}

// listFiles lists the files of the drive outside the bucket directory and
// the temporary area.
func listFiles(t *testing.T, root, bucket string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == bucket || strings.HasPrefix(rel, bucket+string(filepath.Separator)) ||
			strings.Contains(rel, filepath.Join(".nexo.sys", "buckets", bucket)) ||
			strings.HasPrefix(rel, filepath.Join(".nexo.sys", "tmp")) {
			return nil
		}
		if !d.IsDir() {
			out = append(out, rel)
		}
		return nil
	})
	require.NoError(t, err)
	return out
}
