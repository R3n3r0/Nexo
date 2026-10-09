package integration_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enableVersioning sets the versioning state of a bucket.
func (e *env) setVersioning(bucket string, status types.BucketVersioningStatus) {
	e.t.Helper()
	_, err := e.root.PutBucketVersioning(e.ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: status}})
	require.NoError(e.t, err)
}

// putVersion stores an object as root and returns its version id.
func (e *env) putVersion(bucket, key, body string, mutate ...func(*s3.PutObjectInput)) string {
	e.t.Helper()
	in := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(body)}
	for _, m := range mutate {
		m(in)
	}
	out, err := e.root.PutObject(e.ctx, in)
	require.NoError(e.t, err, "put %s/%s", bucket, key)
	return aws.ToString(out.VersionId)
}

// getVersionBody reads one version of an object as root.
func (e *env) getVersionBody(bucket, key, versionID string) (string, error) {
	out, err := e.root.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID)})
	if err != nil {
		return "", err
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(out.Body)
	return string(b), err
}

// TestVersioningDeleteMarkers is TS-I-12 for the delete markers: the reads
// that resolve to a marker answer 404 (latest) or 405 (explicit version)
// with the marker headers, and a marker is never a copy source.
//
// covers: S-BKT-04, S-OBJ-10
func TestVersioningDeleteMarkers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "markers"
	e.mkBucket(bucket)
	ctx := e.ctx

	// Back to unversioned is not a state a bucket can return to.
	resp := e.signedDo(http.MethodPut, "/"+bucket+"?versioning", e.rootCreds(),
		[]byte(`<VersioningConfiguration><Status></Status></VersioningConfiguration>`), nil)
	requireS3Error(t, resp, http.StatusBadRequest, "IllegalVersioningConfigurationException")
	e.setVersioning(bucket, types.BucketVersioningStatusEnabled)

	v1 := e.putVersion(bucket, "doc", "one", func(in *s3.PutObjectInput) { in.Metadata = map[string]string{"rev": "1"} })
	v2 := e.putVersion(bucket, "doc", "two", func(in *s3.PutObjectInput) { in.Metadata = map[string]string{"rev": "2"} })
	require.NotEqual(t, v1, v2)

	// Metadata and tags belong to their version.
	head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc"), VersionId: aws.String(v1)})
	require.NoError(t, err)
	assert.Equal(t, "1", head.Metadata["rev"])
	assert.Equal(t, v1, aws.ToString(head.VersionId))
	_, err = e.root.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("doc"), VersionId: aws.String(v1),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("gen"), Value: aws.String("old")}}}})
	require.NoError(t, err)
	tags, err := e.root.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("doc"), VersionId: aws.String(v1)})
	require.NoError(t, err)
	assert.Equal(t, v1, aws.ToString(tags.VersionId))
	require.Len(t, tags.TagSet, 1)
	tags, err = e.root.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("doc")})
	require.NoError(t, err)
	assert.Empty(t, tags.TagSet, "the latest version keeps its own tags")

	del, err := e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc")})
	require.NoError(t, err)
	require.True(t, aws.ToBool(del.DeleteMarker))
	marker := aws.ToString(del.VersionId)

	// GET and HEAD of the key: 404 with the marker headers.
	resp = e.signedDo(http.MethodGet, "/"+bucket+"/doc", e.rootCreds(), nil, nil)
	requireS3Error(t, resp, http.StatusNotFound, "NoSuchKey")
	assert.Equal(t, "true", resp.Header.Get("x-amz-delete-marker"))
	assert.Equal(t, marker, resp.Header.Get("x-amz-version-id"))
	assert.NotEmpty(t, resp.Header.Get("Last-Modified"))
	resp = e.signedDo(http.MethodHead, "/"+bucket+"/doc", e.rootCreds(), nil, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "true", resp.Header.Get("x-amz-delete-marker"))
	assert.Equal(t, marker, resp.Header.Get("x-amz-version-id"))
	// The marker named by its version: 405.
	resp = e.signedDo(http.MethodGet, "/"+bucket+"/doc?versionId="+marker, e.rootCreds(), nil, nil)
	requireS3Error(t, resp, http.StatusMethodNotAllowed, "MethodNotAllowed")
	assert.Equal(t, "true", resp.Header.Get("x-amz-delete-marker"))
	assert.Equal(t, marker, resp.Header.Get("x-amz-version-id"))
	resp = e.signedDo(http.MethodHead, "/"+bucket+"/doc?versionId="+marker, e.rootCreds(), nil, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	assert.Equal(t, "true", resp.Header.Get("x-amz-delete-marker"))

	// Copies: from a version behind the marker, never from the marker.
	cp, err := e.root.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String("restored"),
		CopySource: aws.String(bucket + "/doc?versionId=" + v1)})
	require.NoError(t, err)
	assert.Equal(t, v1, aws.ToString(cp.CopySourceVersionId))
	assert.NotEmpty(t, aws.ToString(cp.VersionId))
	assert.Equal(t, []byte("one"), e.get(bucket, "restored"))
	_, err = e.root.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String("x"), CopySource: aws.String(bucket + "/doc?versionId=" + marker)})
	assert.Equal(t, "InvalidRequest", apiCode(err))
	_, err = e.root.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String("x"), CopySource: aws.String(bucket + "/doc")})
	assert.Equal(t, "NoSuchKey", apiCode(err))

	// Deleting the marker brings the previous version back.
	del, err = e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc"), VersionId: aws.String(marker)})
	require.NoError(t, err)
	assert.True(t, aws.ToBool(del.DeleteMarker))
	assert.Equal(t, marker, aws.ToString(del.VersionId))
	assert.Equal(t, []byte("two"), e.get(bucket, "doc"))
	// Deleting a version for good: it is gone, the others stay.
	_, err = e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc"), VersionId: aws.String(v2)})
	require.NoError(t, err)
	assert.Equal(t, []byte("one"), e.get(bucket, "doc"), "v1 is the latest again")
	_, err = e.getVersionBody(bucket, "doc", v2)
	assert.Equal(t, "NoSuchVersion", apiCode(err))
}

// TestVersioningListAndBatchDelete is TS-I-12 for ListObjectVersions
// (order, IsLatest, markers, prefix and delimiter, pagination) and the
// version-aware DeleteObjects.
//
// covers: S-OBJ-10, S-OBJ-05
func TestVersioningListAndBatchDelete(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "listing"
	e.mkBucket(bucket)
	ctx := e.ctx
	e.put(bucket, "a/null.txt", []byte("before versioning"))
	e.setVersioning(bucket, types.BucketVersioningStatusEnabled)
	ids := map[string][]string{}
	for _, key := range []string{"a/x.txt", "a/null.txt", "b.txt", "a/x.txt", "c/d/e.txt", "a/x.txt"} {
		ids[key] = append([]string{e.putVersion(bucket, key, key)}, ids[key]...) // newest first
	}
	del, err := e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("b.txt")})
	require.NoError(t, err)
	ids["b.txt"] = append([]string{aws.ToString(del.VersionId)}, ids["b.txt"]...)
	ids["a/null.txt"] = append(ids["a/null.txt"], "null")

	type entry struct {
		key, id string
		latest  bool
		marker  bool
	}
	var want []entry
	for _, key := range []string{"a/null.txt", "a/x.txt", "b.txt", "c/d/e.txt"} {
		for i, id := range ids[key] {
			want = append(want, entry{key: key, id: id, latest: i == 0, marker: key == "b.txt" && i == 0})
		}
	}
	collect := func(out *s3.ListObjectVersionsOutput) []entry {
		var got []entry
		for _, v := range out.Versions {
			got = append(got, entry{key: aws.ToString(v.Key), id: aws.ToString(v.VersionId), latest: aws.ToBool(v.IsLatest)})
		}
		for _, m := range out.DeleteMarkers {
			got = append(got, entry{key: aws.ToString(m.Key), id: aws.ToString(m.VersionId), latest: aws.ToBool(m.IsLatest), marker: true})
		}
		return got
	}
	all, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.ElementsMatch(t, want, collect(all))
	assert.False(t, aws.ToBool(all.IsTruncated))

	// Pages of two entries, in S3 order (key ascending, newest first).
	var paged []entry
	keyMarker, idMarker := "", ""
	for page := 0; page < 20; page++ {
		out, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), MaxKeys: aws.Int32(2),
			KeyMarker: nilIfEmpty(keyMarker), VersionIdMarker: nilIfEmpty(idMarker)})
		require.NoError(t, err)
		got := collect(out)
		assert.LessOrEqual(t, len(got), 2)
		paged = append(paged, got...)
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		keyMarker, idMarker = aws.ToString(out.NextKeyMarker), aws.ToString(out.NextVersionIdMarker)
		require.NotEmpty(t, keyMarker)
		require.NotEmpty(t, idMarker)
	}
	assert.ElementsMatch(t, want, paged, "every entry exactly once")
	var order []string
	for _, p := range paged {
		order = append(order, p.key)
	}
	assert.IsNonDecreasing(t, order, "keys in ascending order")

	// Prefix and delimiter.
	byPrefix, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String("a/")})
	require.NoError(t, err)
	assert.Len(t, byPrefix.Versions, 5)
	tree, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), Delimiter: aws.String("/")})
	require.NoError(t, err)
	var prefixes []string
	for _, p := range tree.CommonPrefixes {
		prefixes = append(prefixes, aws.ToString(p.Prefix))
	}
	assert.Equal(t, []string{"a/", "c/"}, prefixes)
	require.Len(t, tree.DeleteMarkers, 1)
	assert.Equal(t, "b.txt", aws.ToString(tree.DeleteMarkers[0].Key))
	// Within a key: the version id marker resumes after that version.
	resumed, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String("a/x.txt"),
		KeyMarker: aws.String("a/x.txt"), VersionIdMarker: aws.String(ids["a/x.txt"][0])})
	require.NoError(t, err)
	require.Len(t, resumed.Versions, 2)
	assert.Equal(t, ids["a/x.txt"][1], aws.ToString(resumed.Versions[0].VersionId))
	assert.False(t, aws.ToBool(resumed.Versions[0].IsLatest))

	// DeleteObjects with a version id per key: a version, a marker, the
	// null version, a key without version (new marker).
	out, err := e.root.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: &types.Delete{Objects: []types.ObjectIdentifier{
		{Key: aws.String("a/x.txt"), VersionId: aws.String(ids["a/x.txt"][2])},
		{Key: aws.String("b.txt"), VersionId: aws.String(ids["b.txt"][0])},
		{Key: aws.String("a/null.txt"), VersionId: aws.String("null")},
		{Key: aws.String("c/d/e.txt")},
	}}})
	require.NoError(t, err)
	require.Empty(t, out.Errors)
	require.Len(t, out.Deleted, 4)
	results := map[string]types.DeletedObject{}
	for _, d := range out.Deleted {
		results[aws.ToString(d.Key)] = d
	}
	assert.Equal(t, ids["a/x.txt"][2], aws.ToString(results["a/x.txt"].VersionId))
	assert.True(t, aws.ToBool(results["b.txt"].DeleteMarker), "the removed version is a marker")
	assert.Equal(t, ids["b.txt"][0], aws.ToString(results["b.txt"].DeleteMarkerVersionId))
	assert.True(t, aws.ToBool(results["c/d/e.txt"].DeleteMarker), "a delete without version adds a marker")
	assert.NotEmpty(t, aws.ToString(results["c/d/e.txt"].DeleteMarkerVersionId))
	assert.Equal(t, []byte("b.txt"), e.get(bucket, "b.txt"), "the marker removal uncovers the version")
	_, err = e.getVersionBody(bucket, "a/null.txt", "null")
	assert.Equal(t, "NoSuchVersion", apiCode(err))
}

// nilIfEmpty returns nil for "" (an absent optional SDK parameter).
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

// TestVersioningSuspendedAndMultipart is TS-I-12 for the suspended state
// (the null version is replaced, a delete writes a null marker, the other
// versions stay) and for the multipart uploads of a versioned bucket.
//
// covers: S-BKT-04, S-OBJ-06
func TestVersioningSuspendedAndMultipart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "suspend"
	e.mkBucket(bucket)
	ctx := e.ctx
	e.setVersioning(bucket, types.BucketVersioningStatusEnabled)
	kept := e.putVersion(bucket, "k", "versioned")

	// A multipart upload creates a version of its own.
	up, err := e.root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("k")})
	require.NoError(t, err)
	part, err := e.root.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("k"), UploadId: up.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader([]byte("multipart"))})
	require.NoError(t, err)
	done, err := e.root.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("k"), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}}}})
	require.NoError(t, err)
	mpVersion := aws.ToString(done.VersionId)
	require.NotEmpty(t, mpVersion)
	assert.NotEqual(t, kept, mpVersion)
	assert.Equal(t, []byte("multipart"), e.get(bucket, "k"))
	body, err := e.getVersionBody(bucket, "k", kept)
	require.NoError(t, err)
	assert.Equal(t, "versioned", body)

	e.setVersioning(bucket, types.BucketVersioningStatusSuspended)
	for i := 0; i < 2; i++ {
		v := e.putVersion(bucket, "k", fmt.Sprintf("null %d", i))
		assert.Contains(t, []string{"", "null"}, v)
	}
	body, err = e.getVersionBody(bucket, "k", "null")
	require.NoError(t, err)
	assert.Equal(t, "null 1", body, "the null version is overwritten")
	del, err := e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("k")})
	require.NoError(t, err)
	assert.True(t, aws.ToBool(del.DeleteMarker))
	assert.Equal(t, "null", aws.ToString(del.VersionId), "the marker is the null version")

	versions, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	var listed []string
	for _, v := range versions.Versions {
		listed = append(listed, aws.ToString(v.VersionId))
	}
	assert.ElementsMatch(t, []string{mpVersion, kept}, listed, "the null object gave way to the null marker, the others stay")
	require.Len(t, versions.DeleteMarkers, 1)
	assert.Equal(t, "null", aws.ToString(versions.DeleteMarkers[0].VersionId))
	assert.True(t, aws.ToBool(versions.DeleteMarkers[0].IsLatest))

	// Enabled again: new versions get fresh ids above the null marker.
	e.setVersioning(bucket, types.BucketVersioningStatusEnabled)
	again := e.putVersion(bucket, "k", "again")
	assert.NotContains(t, []string{"", "null"}, again)
	assert.Equal(t, []byte("again"), e.get(bucket, "k"))
}
