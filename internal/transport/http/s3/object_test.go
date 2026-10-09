package s3_test

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

// body returns an object reader over data.
func body(data string, info domain.ObjectInfo, offset, length int64) *object.Reader {
	return &object.Reader{ReadCloser: io.NopCloser(strings.NewReader(data)), Info: info, Offset: offset, Length: length}
}

func TestPutObject(t *testing.T) {
	f := newFixture(t)
	var got object.PutObjectInput
	var data []byte
	f.objects.EXPECT().PutObject(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in object.PutObjectInput) (domain.ObjectInfo, error) {
		got = in
		data, _ = io.ReadAll(in.Reader)
		return domain.ObjectInfo{ETag: "5eb63bbbe01eeed093cb22bb8f5acdc3", VersionID: "v1", Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: "crc"}}, nil
	}).Once()
	rec := f.do(http.MethodPut, "/photos/dir/a%20b.txt", "hello world",
		"Content-Type", "text/plain", "Content-MD5", "XrY7u+Ae7tCTyyK7j1rNww==", "Content-Encoding", "aws-chunked,gzip",
		"Content-Disposition", "attachment", "Content-Language", "it", "Cache-Control", "max-age=1", "Expires", "Thu, 01 Jan 2027 00:00:00 GMT",
		"x-amz-meta-Owner", "alice", "x-amz-tagging", "k=v&team=a%20b", "x-amz-checksum-crc32c", "crc",
		"x-amz-storage-class", "REDUCED_REDUNDANCY", "If-None-Match", "*", "x-amz-acl", "bucket-owner-full-control",
		"x-amz-content-sha256", strings.Repeat("ab", 32))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, `"5eb63bbbe01eeed093cb22bb8f5acdc3"`, rec.Header().Get("ETag"))
	assert.Equal(t, "v1", rec.Header().Get("x-amz-version-id"))
	assert.Equal(t, "crc", rec.Header().Get("x-amz-checksum-crc32c"))

	assert.Equal(t, "hello world", string(data))
	assert.Equal(t, object.PutObjectInput{
		Bucket: "photos", Key: "dir/a b.txt", Reader: got.Reader, Size: 11,
		MD5Hex: "5eb63bbbe01eeed093cb22bb8f5acdc3", SHA256Hex: strings.Repeat("ab", 32),
		Checksum:    domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: "crc"},
		ContentType: "text/plain", ContentEncoding: "gzip", ContentDisposition: "attachment", ContentLanguage: "it",
		CacheControl: "max-age=1", Expires: "Thu, 01 Jan 2027 00:00:00 GMT",
		UserMetadata: map[string]string{"owner": "alice"}, Tags: map[string]string{"k": "v", "team": "a b"},
		StorageClass: "REDUCED_REDUNDANCY", Conditions: domain.Conditions{IfNoneMatch: "*"},
	}, got)
}

func TestPutObjectLengths(t *testing.T) {
	f := newFixture(t)
	// Transfer-Encoding: chunked streams a body of unknown length.
	f.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool {
		data, _ := io.ReadAll(in.Reader)
		return in.Size == -1 && string(data) == "streamed"
	})).Return(domain.ObjectInfo{ETag: "e"}, nil).Once()
	req := httptest.NewRequest(http.MethodPut, "/b/k", strings.NewReader("streamed"))
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	assert.Equal(t, http.StatusOK, f.serve(req).Code)

	// An empty object needs Content-Length: 0.
	f.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool { return in.Size == 0 })).Return(domain.ObjectInfo{ETag: "d41d8"}, nil).Once()
	assert.Equal(t, http.StatusOK, f.do(http.MethodPut, "/b/empty", "").Code)

	// HTTP/2 bodies may come without a length.
	f.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool { return in.Size == -1 })).Return(domain.ObjectInfo{ETag: "e"}, nil).Once()
	req = httptest.NewRequest(http.MethodPut, "/b/h2", strings.NewReader("x"))
	req.ProtoMajor, req.ContentLength = 2, -1
	assert.Equal(t, http.StatusOK, f.serve(req).Code)

	// An aws-chunked body decoded by the authentication carries its decoded length.
	f.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool { return in.Size == 3 })).Return(domain.ObjectInfo{ETag: "e"}, nil).Once()
	req = request(http.MethodPut, "/b/chunked", "abc", "x-amz-decoded-content-length", "3", "Content-Encoding", "aws-chunked")
	assert.Equal(t, http.StatusOK, f.serve(req).Code)

	// No length at all.
	req = httptest.NewRequest(http.MethodPut, "/b/k", nil)
	requireError(t, f.serve(req), http.StatusLengthRequired, "MissingContentLength")
	req = httptest.NewRequest(http.MethodPut, "/b/k", strings.NewReader("x"))
	req.ContentLength = -1
	requireError(t, f.serve(req), http.StatusLengthRequired, "MissingContentLength")
	// An aws-chunked body nobody decoded would be stored with its framing.
	req = request(http.MethodPut, "/b/k", "5;chunk-signature=x\r\nhello\r\n0;chunk-signature=y\r\n\r\n", "x-amz-decoded-content-length", "5")
	requireError(t, f.serve(req), http.StatusBadRequest, "InvalidRequest")
	req = request(http.MethodPut, "/b/k", "x", "x-amz-decoded-content-length", "nope")
	requireError(t, f.serve(req), http.StatusBadRequest, "InvalidRequest")
}

func TestPutObjectRejectsInvalidHeaders(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name    string
		headers []string
		status  int
		code    string
	}{
		{"invalid Content-MD5", []string{"Content-MD5", "abc"}, 400, "InvalidDigest"},
		{"short Content-MD5", []string{"Content-MD5", "YWJj"}, 400, "InvalidDigest"},
		{"metadata too large", []string{"x-amz-meta-big", strings.Repeat("x", 2048)}, 400, "MetadataTooLarge"},
		{"invalid storage class", []string{"x-amz-storage-class", "GLACIER"}, 400, "InvalidStorageClass"},
		{"duplicate tag", []string{"x-amz-tagging", "a=1&a=2"}, 400, "InvalidTag"},
		{"bad tagging", []string{"x-amz-tagging", "a=%zz"}, 400, "InvalidTag"},
		{"two checksums", []string{"x-amz-checksum-crc32", "a", "x-amz-checksum-sha256", "b"}, 400, "InvalidRequest"},
		{"public ACL", []string{"x-amz-acl", "public-read"}, 501, "NotImplemented"},
		{"SSE", []string{"x-amz-server-side-encryption", "ROT13"}, 400, "InvalidArgument"},
		{"SSE-C over plain HTTP", []string{"x-amz-server-side-encryption-customer-algorithm", "AES256"}, 400, "InvalidRequest"},
		{"object lock mode without date", []string{"x-amz-object-lock-mode", "GOVERNANCE"}, 400, "InvalidArgument"},
		{"object lock bad mode", []string{"x-amz-object-lock-mode", "LOOSE", "x-amz-object-lock-retain-until-date", "2030-01-01T00:00:00Z"}, 400, "InvalidArgument"},
		{"object lock bad date", []string{"x-amz-object-lock-mode", "GOVERNANCE", "x-amz-object-lock-retain-until-date", "tomorrow"}, 400, "InvalidArgument"},
		{"legal hold", []string{"x-amz-object-lock-legal-hold", "MAYBE"}, 400, "InvalidArgument"},
		{"grant", []string{"x-amz-grant-full-control", "id=x"}, 501, "NotImplemented"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requireError(t, f.do(http.MethodPut, "/b/k", "data", tc.headers...), tc.status, tc.code)
		})
	}
	f.objects.EXPECT().PutObject(mock.Anything, mock.Anything).Return(domain.ObjectInfo{}, domain.NewS3Error(domain.ErrBadDigest, "BadDigest", "")).Once()
	requireError(t, f.do(http.MethodPut, "/b/k", "data"), http.StatusBadRequest, "BadDigest")
	f.objects.EXPECT().PutObject(mock.Anything, mock.Anything).Return(domain.ObjectInfo{}, domain.ErrPreconditionFailed).Once()
	requireError(t, f.do(http.MethodPut, "/b/k", "data", "If-None-Match", "*"), http.StatusPreconditionFailed, "PreconditionFailed")
}

func TestPutObjectSignedPayloadIsNotRehashed(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool { return in.SHA256Hex == "" })).
		Return(domain.ObjectInfo{ETag: "e"}, nil).Twice()
	sha := strings.Repeat("ab", 32)
	rec := f.do(http.MethodPut, "/b/k", "x", "Authorization", "AWS4-HMAC-SHA256 Credential=a/20261008/us-east-1/s3/aws4_request", "x-amz-content-sha256", sha)
	assert.Equal(t, http.StatusOK, rec.Code)
	rec = f.do(http.MethodPut, "/b/k", "x", "x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestGetObject(t *testing.T) {
	f := newFixture(t)
	info := domain.ObjectInfo{
		Bucket: "photos", Key: "a.txt", Size: 11, ETag: "abc", ModTime: now, ContentType: "text/plain", VersionID: "v1",
		ContentEncoding: "gzip", CacheControl: "no-cache", ContentDisposition: "inline", ContentLanguage: "it", Expires: now,
		UserMetadata: map[string]string{"owner": "alice"}, Tags: map[string]string{"a": "1", "b": "2"},
		StorageClass: "REDUCED_REDUNDANCY", Checksum: domain.Checksum{Algorithm: domain.ChecksumSHA256, Value: "sum"},
	}
	f.objects.EXPECT().GetObject(mock.Anything, "photos", "a.txt", object.GetObjectOptions{}).Return(body("hello world", info, 0, 11), nil).Once()
	rec := f.do(http.MethodGet, "/photos/a.txt", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "hello world", rec.Body.String())
	h := rec.Header()
	assert.Equal(t, "11", h.Get("Content-Length"))
	assert.Equal(t, `"abc"`, h.Get("ETag"))
	assert.Equal(t, "Thu, 08 Oct 2026 12:00:00 GMT", h.Get("Last-Modified"))
	assert.Equal(t, "Thu, 08 Oct 2026 12:00:00 GMT", h.Get("Expires"))
	assert.Equal(t, "text/plain", h.Get("Content-Type"))
	assert.Equal(t, "gzip", h.Get("Content-Encoding"))
	assert.Equal(t, "no-cache", h.Get("Cache-Control"))
	assert.Equal(t, "inline", h.Get("Content-Disposition"))
	assert.Equal(t, "it", h.Get("Content-Language"))
	assert.Equal(t, "bytes", h.Get("Accept-Ranges"))
	assert.Equal(t, "alice", h.Get("x-amz-meta-owner"))
	assert.Equal(t, "2", h.Get("x-amz-tagging-count"))
	assert.Equal(t, "v1", h.Get("x-amz-version-id"))
	assert.Equal(t, "REDUCED_REDUNDANCY", h.Get("x-amz-storage-class"))
	assert.Empty(t, h.Get("x-amz-checksum-sha256"), "checksums only with x-amz-checksum-mode")

	// Checksum mode and response overrides on an authenticated request.
	f.objects.EXPECT().GetObject(mock.Anything, "photos", "a.txt", object.GetObjectOptions{VersionID: "v1"}).Return(body("hello world", info, 0, 11), nil).Once()
	rec = f.do(http.MethodGet, "/photos/a.txt?versionId=v1&response-content-type=application/json&response-content-disposition=attachment%3B%20filename%3Dx&response-cache-control=private&response-expires=0&response-content-language=en&response-content-encoding=br", "",
		"x-amz-checksum-mode", "ENABLED")
	require.Equal(t, http.StatusOK, rec.Code)
	h = rec.Header()
	assert.Equal(t, "sum", h.Get("x-amz-checksum-sha256"))
	assert.Equal(t, "application/json", h.Get("Content-Type"))
	assert.Equal(t, "attachment; filename=x", h.Get("Content-Disposition"))
	assert.Equal(t, "private", h.Get("Cache-Control"))
	assert.Equal(t, "0", h.Get("Expires"))
	assert.Equal(t, "en", h.Get("Content-Language"))
	assert.Equal(t, "br", h.Get("Content-Encoding"))
}

func TestGetObjectRanges(t *testing.T) {
	f := newFixture(t)
	info := domain.ObjectInfo{Size: 100, ETag: "e", ModTime: now, Parts: 3, Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: "c"}}
	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", object.GetObjectOptions{Range: &domain.Range{Start: 10, End: 19}}).Return(body("0123456789", info, 10, 10), nil).Once()
	rec := f.do(http.MethodGet, "/b/k", "", "Range", "bytes=10-19", "x-amz-checksum-mode", "ENABLED")
	require.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, "bytes 10-19/100", rec.Header().Get("Content-Range"))
	assert.Equal(t, "10", rec.Header().Get("Content-Length"))
	assert.Empty(t, rec.Header().Get("x-amz-checksum-crc32"), "no whole-object checksum on a range")
	assert.Equal(t, "0123456789", rec.Body.String())

	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", object.GetObjectOptions{PartNumber: 2}).Return(body("part2", info, 50, 5), nil).Once()
	rec = f.do(http.MethodGet, "/b/k?partNumber=2", "")
	require.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, "bytes 50-54/100", rec.Header().Get("Content-Range"))
	assert.Equal(t, "3", rec.Header().Get("x-amz-mp-parts-count"))

	// A Range the server does not understand is ignored.
	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", object.GetObjectOptions{}).Return(body("all", domain.ObjectInfo{Size: 3}, 0, 3), nil).Once()
	rec = f.do(http.MethodGet, "/b/k", "", "Range", "lines=1-2")
	assert.Equal(t, http.StatusOK, rec.Code)

	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", mock.Anything).Return(nil, domain.ErrInvalidRange).Once()
	requireError(t, f.do(http.MethodGet, "/b/k", "", "Range", "bytes=500-"), http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", mock.Anything).Return(nil, domain.NewS3Error(domain.ErrInvalidRange, "InvalidPartNumber", "")).Once()
	requireError(t, f.do(http.MethodGet, "/b/k?partNumber=9", ""), http.StatusRequestedRangeNotSatisfiable, "InvalidPartNumber")
	for _, n := range []string{"0", "10001", "x"} {
		requireError(t, f.do(http.MethodGet, "/b/k?partNumber="+n, ""), http.StatusBadRequest, "InvalidArgument")
	}

	// Regression: an empty part is not a partial content ("bytes */N" is
	// the Content-Range of a 416 only).
	empty := domain.ObjectInfo{Size: 0, ETag: "e", ModTime: now, Parts: 1}
	f.objects.EXPECT().GetObject(mock.Anything, "b", "empty", object.GetObjectOptions{PartNumber: 1}).Return(body("", empty, 0, 0), nil).Once()
	rec = f.do(http.MethodGet, "/b/empty?partNumber=1", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Content-Range"))
	assert.Equal(t, "0", rec.Header().Get("Content-Length"))
	assert.Equal(t, "1", rec.Header().Get("x-amz-mp-parts-count"))
	f.objects.EXPECT().GetObject(mock.Anything, "b", "empty", object.GetObjectOptions{PartNumber: 1}).Return(body("", empty, 0, 0), nil).Once()
	rec = f.do(http.MethodHead, "/b/empty?partNumber=1", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Content-Range"))
	assert.Equal(t, "1", rec.Header().Get("x-amz-mp-parts-count"))
	// Every range of an empty object is unsatisfiable, suffixes included.
	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "empty", mock.Anything).Return(empty, nil).Once()
	rec = f.do(http.MethodHead, "/b/empty", "", "Range", "bytes=-1")
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, rec.Code)
}

func TestGetObjectConditionsAndMarkers(t *testing.T) {
	f := newFixture(t)
	// HTTP dates parse in a GMT location: compare instants.
	conditional := mock.MatchedBy(func(o object.GetObjectOptions) bool {
		c := o.Conditions
		return c.IfNoneMatch == `"e"` && c.IfModifiedSince.Equal(now) && c.IfUnmodifiedSince.IsZero() && c.IfMatch == ""
	})
	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", conditional).Return(nil, domain.ErrNotModified).Once()
	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "k", conditional).
		Return(domain.ObjectInfo{ETag: "e", ModTime: now}, domain.ErrNotModified).Once()
	rec := f.do(http.MethodGet, "/b/k", "", "If-None-Match", `"e"`, "If-Modified-Since", "Thu, 08 Oct 2026 12:00:00 GMT", "If-Unmodified-Since", "garbage")
	assert.Equal(t, http.StatusNotModified, rec.Code)
	assert.Equal(t, `"e"`, rec.Header().Get("ETag"))
	assert.Equal(t, "Thu, 08 Oct 2026 12:00:00 GMT", rec.Header().Get("Last-Modified"))
	assert.Empty(t, rec.Body.String())

	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", mock.Anything).Return(nil, domain.ErrPreconditionFailed).Once()
	requireError(t, f.do(http.MethodGet, "/b/k", "", "If-Match", `"x"`), http.StatusPreconditionFailed, "PreconditionFailed")

	// The latest version is a delete marker: its version id and time.
	f.objects.EXPECT().GetObject(mock.Anything, "b", "gone", mock.Anything).
		Return(nil, &domain.DeleteMarkerError{VersionID: "m1", ModTime: now,
			Err: domain.NewS3Error(domain.ErrObjectNotFound, "NoSuchKey", "The specified key does not exist.")}).Once()
	rec = f.do(http.MethodGet, "/b/gone", "")
	requireError(t, rec, http.StatusNotFound, "NoSuchKey")
	assert.Equal(t, "true", rec.Header().Get("x-amz-delete-marker"))
	assert.Equal(t, "m1", rec.Header().Get("x-amz-version-id"))
	assert.Equal(t, "Thu, 08 Oct 2026 12:00:00 GMT", rec.Header().Get("Last-Modified"))
	// An explicit version that is a delete marker.
	f.objects.EXPECT().GetObject(mock.Anything, "b", "gone", mock.Anything).
		Return(nil, &domain.DeleteMarkerError{VersionID: "m1", ModTime: now, Err: domain.ErrMethodNotAllowed}).Once()
	rec = f.do(http.MethodGet, "/b/gone?versionId=m1", "")
	requireError(t, rec, http.StatusMethodNotAllowed, "MethodNotAllowed")
	assert.Equal(t, "true", rec.Header().Get("x-amz-delete-marker"))
	assert.Equal(t, "m1", rec.Header().Get("x-amz-version-id"))
	// HEAD answers the same headers without a body.
	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "gone", mock.Anything).
		Return(domain.ObjectInfo{}, &domain.DeleteMarkerError{VersionID: "m2", ModTime: now, Err: domain.ErrObjectNotFound}).Once()
	rec = f.do(http.MethodHead, "/b/gone", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "true", rec.Header().Get("x-amz-delete-marker"))
	assert.Equal(t, "m2", rec.Header().Get("x-amz-version-id"))
	// A plain missing key.
	f.objects.EXPECT().GetObject(mock.Anything, "b", "none", mock.Anything).Return(nil, domain.ErrObjectNotFound).Once()
	rec = f.do(http.MethodGet, "/b/none", "")
	requireError(t, rec, http.StatusNotFound, "NoSuchKey")
	assert.Empty(t, rec.Header().Get("x-amz-delete-marker"))
	f.objects.EXPECT().GetObject(mock.Anything, "b", "v", mock.Anything).Return(nil, domain.ErrNoSuchVersion).Once()
	requireError(t, f.do(http.MethodGet, "/b/v?versionId=zz", ""), http.StatusNotFound, "NoSuchVersion")
}

func TestGetObjectAnonymousOverrides(t *testing.T) {
	f := newFixture(t, withoutIdentity())
	requireError(t, f.do(http.MethodGet, "/b/k?response-content-type=text/html", ""), http.StatusBadRequest, "InvalidRequest")
	rec := f.do(http.MethodHead, "/b/k?response-cache-control=x", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingReader fails after the first read.
type failingReader struct{ done bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("disk gone")
	}
	r.done = true
	return copy(p, "par"), nil
}

func TestGetObjectInterruptedBody(t *testing.T) {
	f := newFixture(t)
	rd := &object.Reader{ReadCloser: io.NopCloser(&failingReader{}), Info: domain.ObjectInfo{Size: 10}, Length: 10}
	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", mock.Anything).Return(rd, nil).Once()
	rec := f.do(http.MethodGet, "/b/k", "")
	assert.Equal(t, http.StatusOK, rec.Code, "the status is sent before the body")
	assert.Equal(t, "par", rec.Body.String())
	assert.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"), "no type is sniffed")
}

func TestHeadObject(t *testing.T) {
	f := newFixture(t)
	info := domain.ObjectInfo{Size: 100, ETag: "e", ModTime: now, ContentType: "image/png", UserMetadata: map[string]string{"a": "b"}, Parts: 4}
	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "k", object.GetObjectOptions{}).Return(info, nil).Once()
	rec := f.do(http.MethodHead, "/b/k", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "100", rec.Header().Get("Content-Length"))
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Equal(t, "b", rec.Header().Get("x-amz-meta-a"))
	assert.Empty(t, rec.Body.String())

	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "k", object.GetObjectOptions{Range: &domain.Range{IsSuffix: true, SuffixLength: 10, End: -1}}).Return(info, nil).Once()
	rec = f.do(http.MethodHead, "/b/k", "", "Range", "bytes=-10")
	require.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, "bytes 90-99/100", rec.Header().Get("Content-Range"))
	assert.Equal(t, "10", rec.Header().Get("Content-Length"))

	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "k", mock.Anything).Return(info, nil).Once()
	rec = f.do(http.MethodHead, "/b/k", "", "Range", "bytes=200-")
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, rec.Code)
	assert.Empty(t, rec.Body.String())

	// partNumber: the size of the part comes from the reader.
	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", object.GetObjectOptions{PartNumber: 2}).Return(body("", info, 25, 25), nil).Once()
	rec = f.do(http.MethodHead, "/b/k?partNumber=2", "")
	require.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, "bytes 25-49/100", rec.Header().Get("Content-Range"))
	assert.Equal(t, "25", rec.Header().Get("Content-Length"))
	assert.Equal(t, "4", rec.Header().Get("x-amz-mp-parts-count"))
	assert.Empty(t, rec.Body.String())
	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", object.GetObjectOptions{PartNumber: 9}).Return(nil, domain.NewS3Error(domain.ErrInvalidRange, "InvalidPartNumber", "")).Once()
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, f.do(http.MethodHead, "/b/k?partNumber=9", "").Code)
	assert.Equal(t, http.StatusBadRequest, f.do(http.MethodHead, "/b/k?partNumber=0", "").Code)

	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "missing", mock.Anything).Return(domain.ObjectInfo{}, domain.ErrObjectNotFound).Once()
	rec = f.do(http.MethodHead, "/b/missing", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, rec.Body.String())
	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "k", mock.Anything).Return(domain.ObjectInfo{ETag: "e", ModTime: now}, domain.ErrNotModified).Twice()
	rec = f.do(http.MethodHead, "/b/k", "", "If-None-Match", `"e"`)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	assert.Equal(t, `"e"`, rec.Header().Get("ETag"))
}

func TestDeleteObject(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().DeleteObject(mock.Anything, "b", "k", object.DeleteObjectOptions{}).
		Return(object.DeletedObject{Key: "k", DeleteMarker: true, DeleteMarkerVersionID: "m1"}, nil).Once()
	rec := f.do(http.MethodDelete, "/b/k", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "true", rec.Header().Get("x-amz-delete-marker"))
	assert.Equal(t, "m1", rec.Header().Get("x-amz-version-id"))

	f.objects.EXPECT().DeleteObject(mock.Anything, "b", "k", object.DeleteObjectOptions{VersionID: "v1", BypassGovernance: true}).
		Return(object.DeletedObject{Key: "k", VersionID: "v1"}, nil).Once()
	rec = f.do(http.MethodDelete, "/b/k?versionId=v1", "", "x-amz-bypass-governance-retention", "true")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "v1", rec.Header().Get("x-amz-version-id"))
	assert.Empty(t, rec.Header().Get("x-amz-delete-marker"))

	f.objects.EXPECT().DeleteObject(mock.Anything, "b", "k", mock.Anything).Return(object.DeletedObject{}, domain.ErrObjectLocked).Once()
	requireError(t, f.do(http.MethodDelete, "/b/k", ""), http.StatusForbidden, "XNexoObjectLocked")
}

func TestDeleteObjects(t *testing.T) {
	f := newFixture(t)
	doc := `<Delete><Quiet>false</Quiet><Object><Key>a</Key></Object><Object><Key>b</Key><VersionId>v2</VersionId></Object><Object><Key>c</Key></Object></Delete>`
	f.objects.EXPECT().DeleteObjects(mock.Anything, object.DeleteObjectsInput{Bucket: "bk", Objects: []object.ObjectToDelete{{Key: "a"}, {Key: "b", VersionID: "v2"}, {Key: "c"}}}).
		Return(object.DeleteObjectsResult{
			Deleted: []object.DeletedObject{{Key: "a", DeleteMarker: true, DeleteMarkerVersionID: "m"}, {Key: "b", VersionID: "v2"}},
			Errors:  []object.DeleteError{{Key: "c", Err: domain.ErrObjectLocked}},
		}, nil).Once()
	rec := f.do(http.MethodPost, "/bk?delete", doc, "Content-MD5", contentMD5(doc))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var res s3.DeleteResult
	decodeXML(t, rec, &res)
	assert.Equal(t, []s3.DeletedEntry{{Key: "a", DeleteMarker: true, DeleteMarkerVersionID: "m"}, {Key: "b", VersionID: "v2"}}, res.Deleted)
	assert.Equal(t, []s3.DeleteErrorEntry{{Key: "c", Code: "XNexoObjectLocked", Message: "Object is WORM protected and cannot be overwritten or deleted"}}, res.Errors)

	// Quiet mode reports the errors only.
	f.objects.EXPECT().DeleteObjects(mock.Anything, mock.MatchedBy(func(in object.DeleteObjectsInput) bool { return in.Quiet })).
		Return(object.DeleteObjectsResult{Deleted: []object.DeletedObject{{Key: "a"}}}, nil).Once()
	rec = f.do(http.MethodPost, "/bk?delete", `<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Quiet>true</Quiet><Object><Key>a</Key></Object></Delete>`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "<Deleted>")

	many := "<Delete>" + strings.Repeat("<Object><Key>k</Key></Object>", 1001) + "</Delete>"
	tests := []struct {
		name, body string
		headers    []string
		code       string
	}{
		{"empty", `<Delete></Delete>`, nil, "MalformedXML"},
		{"too many", many, nil, "MalformedXML"},
		{"malformed", `<Delete><Object>`, nil, "MalformedXML"},
		{"no body", "", nil, "MalformedXML"},
		{"bad digest", doc, []string{"Content-MD5", contentMD5("x")}, "BadDigest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requireError(t, f.do(http.MethodPost, "/bk?delete", tc.body, tc.headers...), http.StatusBadRequest, tc.code)
		})
	}
	f.objects.EXPECT().DeleteObjects(mock.Anything, mock.Anything).Return(object.DeleteObjectsResult{}, domain.ErrBucketNotFound).Once()
	requireError(t, f.do(http.MethodPost, "/bk?delete", doc), http.StatusNotFound, "NoSuchBucket")
}

func TestCopyObject(t *testing.T) {
	f := newFixture(t)
	var got object.CopyObjectInput
	f.objects.EXPECT().CopyObject(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in object.CopyObjectInput) (domain.ObjectInfo, error) {
		got = in
		return domain.ObjectInfo{ETag: "ce", ModTime: now, VersionID: "dv"}, nil
	}).Once()
	rec := f.do(http.MethodPut, "/dst/copy.txt", "",
		"x-amz-copy-source", "/src/dir/a%20b%2Bc.txt?versionId=v1", "x-amz-metadata-directive", "REPLACE", "x-amz-tagging-directive", "replace",
		"x-amz-copy-source-if-match", `"e"`, "x-amz-copy-source-if-none-match", `"x"`,
		"x-amz-copy-source-if-modified-since", "Thu, 08 Oct 2026 12:00:00 GMT", "x-amz-copy-source-if-unmodified-since", "Thu, 08 Oct 2026 12:00:00 GMT",
		"Content-Type", "text/csv", "Cache-Control", "no-store", "x-amz-meta-k", "v", "x-amz-tagging", "t=1", "x-amz-storage-class", "STANDARD")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "dv", rec.Header().Get("x-amz-version-id"))
	assert.Equal(t, "v1", rec.Header().Get("x-amz-copy-source-version-id"))
	var res s3.CopyObjectResult
	decodeXML(t, rec, &res)
	assert.Equal(t, s3.CopyObjectResult{XMLName: res.XMLName, ETag: `"ce"`, LastModified: "2026-10-08T12:00:00.000Z"}, res)
	assert.True(t, got.SrcConditions.IfModifiedSince.Equal(now))
	assert.True(t, got.SrcConditions.IfUnmodifiedSince.Equal(now))
	got.SrcConditions.IfModifiedSince, got.SrcConditions.IfUnmodifiedSince = now, now
	assert.Equal(t, object.CopyObjectInput{
		SrcBucket: "src", SrcKey: "dir/a b+c.txt", SrcVersionID: "v1",
		SrcConditions: domain.Conditions{IfMatch: `"e"`, IfNoneMatch: `"x"`, IfModifiedSince: now, IfUnmodifiedSince: now},
		DstBucket:     "dst", DstKey: "copy.txt", ReplaceMetadata: true, ContentType: "text/csv", CacheControl: "no-store",
		UserMetadata: map[string]string{"k": "v"}, ReplaceTags: true, Tags: map[string]string{"t": "1"}, StorageClass: "STANDARD",
	}, got)

	// COPY (the default) ignores the metadata and tags of the request.
	f.objects.EXPECT().CopyObject(mock.Anything, object.CopyObjectInput{SrcBucket: "src", SrcKey: "k", DstBucket: "dst", DstKey: "k2"}).
		Return(domain.ObjectInfo{ETag: "ce", ModTime: now}, nil).Once()
	rec = f.do(http.MethodPut, "/dst/k2", "", "x-amz-copy-source", "src/k", "Content-Type", "ignored", "x-amz-meta-x", "ignored", "x-amz-tagging", "ignored=1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Header().Get("x-amz-copy-source-version-id"))

	tests := []struct {
		name    string
		headers []string
		status  int
		code    string
	}{
		{"no key", []string{"x-amz-copy-source", "/src"}, 400, "InvalidArgument"},
		{"bad escape", []string{"x-amz-copy-source", "/src/%zz"}, 400, "InvalidArgument"},
		{"bad query", []string{"x-amz-copy-source", "/src/k?versionId=%zz"}, 400, "InvalidArgument"},
		{"long key", []string{"x-amz-copy-source", "/src/" + strings.Repeat("k", 1025)}, 400, "KeyTooLongError"},
		{"bad directive", []string{"x-amz-copy-source", "/src/k", "x-amz-metadata-directive", "MERGE"}, 400, "InvalidArgument"},
		{"bad tagging directive", []string{"x-amz-copy-source", "/src/k", "x-amz-tagging-directive", "MERGE"}, 400, "InvalidArgument"},
		{"bad storage class", []string{"x-amz-copy-source", "/src/k", "x-amz-storage-class", "X"}, 400, "InvalidStorageClass"},
		{"SSE", []string{"x-amz-copy-source", "/src/k", "x-amz-server-side-encryption", "ROT13"}, 400, "InvalidArgument"},
		{"metadata too large", []string{"x-amz-copy-source", "/src/k", "x-amz-metadata-directive", "REPLACE", "x-amz-meta-x", strings.Repeat("y", 3000)}, 400, "MetadataTooLarge"},
		{"bad tags", []string{"x-amz-copy-source", "/src/k", "x-amz-tagging-directive", "REPLACE", "x-amz-tagging", "a=1&a=2"}, 400, "InvalidTag"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requireError(t, f.do(http.MethodPut, "/dst/k", "", tc.headers...), tc.status, tc.code)
		})
	}
	f.objects.EXPECT().CopyObject(mock.Anything, mock.Anything).Return(domain.ObjectInfo{}, domain.ErrPreconditionFailed).Once()
	requireError(t, f.do(http.MethodPut, "/dst/k", "", "x-amz-copy-source", "/src/k", "x-amz-copy-source-if-match", "x"), http.StatusPreconditionFailed, "PreconditionFailed")
}

func TestObjectTagging(t *testing.T) {
	f := newFixture(t)
	doc := `<Tagging><TagSet><Tag><Key>k</Key><Value>v</Value></Tag></TagSet></Tagging>`
	f.objects.EXPECT().PutObjectTags(mock.Anything, "b", "o", map[string]string{"k": "v"}, object.TagOptions{VersionID: "v1"}).
		Return(domain.ObjectInfo{VersionID: "v1"}, nil).Once()
	rec := f.do(http.MethodPut, "/b/o?tagging&versionId=v1", doc)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "v1", rec.Header().Get("x-amz-version-id"))
	requireError(t, f.do(http.MethodPut, "/b/o?tagging", `<Tagging><TagSet><Tag><Key>k</Key></Tag><Tag><Key>k</Key></Tag></TagSet></Tagging>`), http.StatusBadRequest, "InvalidTag")
	requireError(t, f.do(http.MethodPut, "/b/o?tagging", "nope"), http.StatusBadRequest, "MalformedXML")
	f.objects.EXPECT().PutObjectTags(mock.Anything, "b", "o", mock.Anything, mock.Anything).Return(domain.ObjectInfo{}, domain.ErrObjectNotFound).Once()
	requireError(t, f.do(http.MethodPut, "/b/o?tagging", doc), http.StatusNotFound, "NoSuchKey")

	f.objects.EXPECT().GetObjectTags(mock.Anything, "b", "o", object.TagOptions{VersionID: "v1"}).Return(map[string]string{"z": "1", "a": "2"}, nil).Once()
	rec = f.do(http.MethodGet, "/b/o?tagging&versionId=v1", "")
	var tagging s3.Tagging
	decodeXML(t, rec, &tagging)
	assert.Equal(t, []s3.Tag{{Key: "a", Value: "2"}, {Key: "z", Value: "1"}}, tagging.TagSet.Tags)
	assert.Equal(t, "v1", rec.Header().Get("x-amz-version-id"))
	f.objects.EXPECT().GetObjectTags(mock.Anything, "b", "o", object.TagOptions{}).Return(nil, nil).Once()
	rec = f.do(http.MethodGet, "/b/o?tagging", "")
	assert.Contains(t, rec.Body.String(), "<TagSet></TagSet>")
	f.objects.EXPECT().GetObjectTags(mock.Anything, "b", "x", object.TagOptions{}).Return(nil, domain.ErrObjectNotFound).Once()
	requireError(t, f.do(http.MethodGet, "/b/x?tagging", ""), http.StatusNotFound, "NoSuchKey")

	f.objects.EXPECT().DeleteObjectTags(mock.Anything, "b", "o", object.TagOptions{}).Return(domain.ObjectInfo{VersionID: "v2"}, nil).Once()
	rec = f.do(http.MethodDelete, "/b/o?tagging", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "v2", rec.Header().Get("x-amz-version-id"))
	f.objects.EXPECT().DeleteObjectTags(mock.Anything, "b", "x", object.TagOptions{}).Return(domain.ObjectInfo{}, domain.ErrObjectNotFound).Once()
	requireError(t, f.do(http.MethodDelete, "/b/x?tagging", ""), http.StatusNotFound, "NoSuchKey")
}

func TestObjectACL(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "o", object.GetObjectOptions{VersionID: "v"}).Return(domain.ObjectInfo{}, nil).Twice()
	rec := f.do(http.MethodGet, "/b/o?acl&versionId=v", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<Permission>FULL_CONTROL</Permission>")
	assert.Equal(t, http.StatusOK, f.do(http.MethodPut, "/b/o?acl&versionId=v", "", "x-amz-acl", "private").Code)
	requireError(t, f.do(http.MethodPut, "/b/o?acl", "", "x-amz-acl", "public-read-write"), http.StatusNotImplemented, "NotImplemented")
	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "x", mock.Anything).Return(domain.ObjectInfo{}, domain.ErrObjectNotFound).Twice()
	requireError(t, f.do(http.MethodGet, "/b/x?acl", ""), http.StatusNotFound, "NoSuchKey")
	requireError(t, f.do(http.MethodPut, "/b/x?acl", ""), http.StatusNotFound, "NoSuchKey")
}

func TestCopyResultXML(t *testing.T) {
	out, err := xml.Marshal(s3.CopyObjectResult{ETag: `"e"`, LastModified: "t"})
	require.NoError(t, err)
	assert.Equal(t, `<CopyObjectResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ETag>"e"</ETag><LastModified>t</LastModified></CopyObjectResult>`,
		strings.ReplaceAll(string(out), "&#34;", `"`))
}
