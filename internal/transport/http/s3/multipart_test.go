package s3_test

import (
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

func TestCreateMultipartUpload(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().NewMultipartUpload(mock.Anything, object.NewMultipartInput{
		Bucket: "b", Key: "big.bin", ContentType: "application/zip", ContentDisposition: "attachment",
		UserMetadata: map[string]string{"m": "1"}, Tags: map[string]string{"t": "2"}, StorageClass: "STANDARD", Checksum: domain.ChecksumSHA256,
	}).Return(domain.MultipartUpload{Bucket: "b", Key: "big.bin", UploadID: "up-1", Checksum: domain.ChecksumSHA256}, nil).Once()
	rec := f.do(http.MethodPost, "/b/big.bin?uploads", "", "Content-Type", "application/zip", "Content-Disposition", "attachment",
		"x-amz-meta-m", "1", "x-amz-tagging", "t=2", "x-amz-storage-class", "STANDARD", "x-amz-checksum-algorithm", "sha256")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "SHA256", rec.Header().Get("x-amz-checksum-algorithm"))
	var res s3.InitiateMultipartUploadResult
	decodeXML(t, rec, &res)
	assert.Equal(t, "b", res.Bucket)
	assert.Equal(t, "big.bin", res.Key)
	assert.Equal(t, "up-1", res.UploadID)

	tests := []struct {
		name    string
		headers []string
		status  int
		code    string
	}{
		{"bad tags", []string{"x-amz-tagging", "a=1&a=2"}, 400, "InvalidTag"},
		{"bad storage class", []string{"x-amz-storage-class", "DEEP_ARCHIVE"}, 400, "InvalidStorageClass"},
		{"metadata too large", []string{"x-amz-meta-x", strings.Repeat("y", 2100)}, 400, "MetadataTooLarge"},
		{"SSE", []string{"x-amz-server-side-encryption", "ROT13"}, 400, "InvalidArgument"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requireError(t, f.do(http.MethodPost, "/b/k?uploads", "", tc.headers...), tc.status, tc.code)
		})
	}
	f.objects.EXPECT().NewMultipartUpload(mock.Anything, mock.Anything).Return(domain.MultipartUpload{}, domain.ErrBucketNotFound).Once()
	requireError(t, f.do(http.MethodPost, "/nope/k?uploads", ""), http.StatusNotFound, "NoSuchBucket")
}

func TestUploadPart(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().PutObjectPart(mock.Anything, mock.MatchedBy(func(in object.PutObjectPartInput) bool {
		data, _ := io.ReadAll(in.Reader)
		return in.Bucket == "b" && in.Key == "k" && in.UploadID == "u1" && in.PartNumber == 3 && in.Size == 4 && string(data) == "part" &&
			in.MD5Hex == hexMD5("part") && in.Checksum == domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: "crc"}
	})).Return(domain.Part{Number: 3, ETag: "pe", Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: "crc"}}, nil).Once()
	rec := f.do(http.MethodPut, "/b/k?partNumber=3&uploadId=u1", "part", "Content-MD5", contentMD5("part"), "x-amz-checksum-crc32", "crc")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, `"pe"`, rec.Header().Get("ETag"))
	assert.Equal(t, "crc", rec.Header().Get("x-amz-checksum-crc32"))

	for _, target := range []string{"/b/k?partNumber=0&uploadId=u1", "/b/k?partNumber=10001&uploadId=u1", "/b/k?uploadId=u1", "/b/k?partNumber=x&uploadId=u1"} {
		requireError(t, f.do(http.MethodPut, target, "part"), http.StatusBadRequest, "InvalidArgument")
	}
	requireError(t, f.serve(httptest.NewRequest(http.MethodPut, "/b/k?partNumber=1&uploadId=u1", nil)), http.StatusLengthRequired, "MissingContentLength")
	requireError(t, f.do(http.MethodPut, "/b/k?partNumber=1&uploadId=u1", "part", "Content-MD5", "bad"), http.StatusBadRequest, "InvalidDigest")
	requireError(t, f.do(http.MethodPut, "/b/k?partNumber=1&uploadId=u1", "part", "x-amz-checksum-crc32", "a", "x-amz-checksum-sha1", "b"), http.StatusBadRequest, "InvalidRequest")
	f.objects.EXPECT().PutObjectPart(mock.Anything, mock.Anything).Return(domain.Part{}, domain.ErrNoSuchUpload).Once()
	requireError(t, f.do(http.MethodPut, "/b/k?partNumber=1&uploadId=gone", "part"), http.StatusNotFound, "NoSuchUpload")
}

func TestUploadPartCopy(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().CopyObjectPart(mock.Anything, object.CopyObjectPartInput{
		SrcBucket: "src", SrcKey: "a b", SrcVersionID: "sv", SrcRange: &domain.Range{Start: 0, End: 99},
		SrcConditions: domain.Conditions{IfMatch: `"e"`},
		DstBucket:     "b", DstKey: "k", UploadID: "u1", PartNumber: 2,
	}).Return(domain.Part{Number: 2, ETag: "cp", ModTime: now}, nil).Once()
	rec := f.do(http.MethodPut, "/b/k?partNumber=2&uploadId=u1", "",
		"x-amz-copy-source", "src/a%20b?versionId=sv", "x-amz-copy-source-range", "bytes=0-99", "x-amz-copy-source-if-match", `"e"`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "sv", rec.Header().Get("x-amz-copy-source-version-id"))
	var res s3.CopyPartResult
	decodeXML(t, rec, &res)
	assert.Equal(t, `"cp"`, res.ETag)
	assert.Equal(t, "2026-10-08T12:00:00.000Z", res.LastModified)

	for _, rg := range []string{"bytes=-5", "bytes=5-", "items=0-1", "bytes=9-1"} {
		requireError(t, f.do(http.MethodPut, "/b/k?partNumber=2&uploadId=u1", "", "x-amz-copy-source", "src/a", "x-amz-copy-source-range", rg),
			http.StatusBadRequest, "InvalidArgument")
	}
	requireError(t, f.do(http.MethodPut, "/b/k?partNumber=0&uploadId=u1", "", "x-amz-copy-source", "src/a"), http.StatusBadRequest, "InvalidArgument")
	requireError(t, f.do(http.MethodPut, "/b/k?partNumber=1&uploadId=u1", "", "x-amz-copy-source", "src"), http.StatusBadRequest, "InvalidArgument")
	f.objects.EXPECT().CopyObjectPart(mock.Anything, mock.Anything).Return(domain.Part{}, domain.ErrInvalidRange).Once()
	requireError(t, f.do(http.MethodPut, "/b/k?partNumber=1&uploadId=u1", "", "x-amz-copy-source", "src/a", "x-amz-copy-source-range", "bytes=0-999999"),
		http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
}

func TestCompleteMultipartUpload(t *testing.T) {
	f := newFixture(t)
	doc := `<CompleteMultipartUpload xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
		<Part><PartNumber>1</PartNumber><ETag>"e1"</ETag><ChecksumCRC32>c1</ChecksumCRC32></Part>
		<Part><PartNumber>2</PartNumber><ETag>e2</ETag><ChecksumCRC32C>c2</ChecksumCRC32C></Part>
		<Part><PartNumber>3</PartNumber><ETag>"e3"</ETag><ChecksumSHA1>c3</ChecksumSHA1></Part>
		<Part><PartNumber>4</PartNumber><ETag>"e4"</ETag><ChecksumSHA256>c4</ChecksumSHA256></Part>
	</CompleteMultipartUpload>`
	f.objects.EXPECT().CompleteMultipartUpload(mock.Anything, object.CompleteMultipartInput{
		Bucket: "b", Key: "dir/a b", UploadID: "u1", Conditions: domain.Conditions{IfNoneMatch: "*"},
		Parts: []domain.CompletePart{
			{Number: 1, ETag: "e1", Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: "c1"}},
			{Number: 2, ETag: "e2", Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: "c2"}},
			{Number: 3, ETag: "e3", Checksum: domain.Checksum{Algorithm: domain.ChecksumSHA1, Value: "c3"}},
			{Number: 4, ETag: "e4", Checksum: domain.Checksum{Algorithm: domain.ChecksumSHA256, Value: "c4"}},
		},
	}).Return(domain.ObjectInfo{ETag: "md5-4", VersionID: "v9", Checksum: domain.Checksum{Algorithm: domain.ChecksumSHA256, Value: "full-4"}}, nil).Once()
	req := request(http.MethodPost, "/b/dir/a%20b?uploadId=u1", doc, "If-None-Match", "*")
	req.Host = "s3.local:9000"
	rec := f.serve(req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "v9", rec.Header().Get("x-amz-version-id"))
	var res s3.CompleteMultipartUploadResult
	decodeXML(t, rec, &res)
	assert.Equal(t, "http://s3.local:9000/b/dir/a%20b", res.Location)
	assert.Equal(t, `"md5-4"`, res.ETag)
	assert.Equal(t, "full-4", res.ChecksumSHA256)
	assert.Equal(t, "dir/a b", res.Key)

	tests := []struct {
		name, body string
		code       string
	}{
		{"no parts", `<CompleteMultipartUpload></CompleteMultipartUpload>`, "MalformedXML"},
		{"malformed", `<CompleteMultipartUpload><Part>`, "MalformedXML"},
		{"empty", "", "MalformedXML"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requireError(t, f.do(http.MethodPost, "/b/k?uploadId=u1", tc.body), http.StatusBadRequest, tc.code)
		})
	}
	f.objects.EXPECT().CompleteMultipartUpload(mock.Anything, mock.Anything).
		Return(domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidPart, "InvalidPart", "part 2 is missing")).Once()
	rec = f.do(http.MethodPost, "/b/k?uploadId=u1", `<CompleteMultipartUpload><Part><PartNumber>2</PartNumber><ETag>x</ETag></Part></CompleteMultipartUpload>`)
	requireError(t, rec, http.StatusBadRequest, "InvalidPart")
	assert.Equal(t, "part 2 is missing", s3Error(t, rec).Message)
	f.objects.EXPECT().CompleteMultipartUpload(mock.Anything, mock.Anything).Return(domain.ObjectInfo{}, domain.ErrEntityTooSmall).Once()
	requireError(t, f.do(http.MethodPost, "/b/k?uploadId=u1", `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>x</ETag></Part></CompleteMultipartUpload>`),
		http.StatusBadRequest, "EntityTooSmall")
}

func TestCompleteMultipartUploadVirtualHost(t *testing.T) {
	f := newFixture(t, withDomains("s3.example.com"))
	f.objects.EXPECT().CompleteMultipartUpload(mock.Anything, mock.MatchedBy(func(in object.CompleteMultipartInput) bool {
		return in.Bucket == "photos" && in.Key == "k"
	})).Return(domain.ObjectInfo{ETag: "e-1"}, nil).Once()
	req := request(http.MethodPost, "/k?uploadId=u", `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>x</ETag></Part></CompleteMultipartUpload>`,
		"X-Forwarded-Proto", "https")
	req.Host = "photos.s3.example.com"
	rec := f.serve(req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var res s3.CompleteMultipartUploadResult
	decodeXML(t, rec, &res)
	assert.Equal(t, "https://photos.s3.example.com/k", res.Location)
	assert.Equal(t, "photos", res.Bucket)
}

func TestAbortMultipartUpload(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().AbortMultipartUpload(mock.Anything, "b", "k", "u1").Return(nil).Once()
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, "/b/k?uploadId=u1", "").Code)
	f.objects.EXPECT().AbortMultipartUpload(mock.Anything, "b", "k", "u2").Return(domain.ErrNoSuchUpload).Once()
	requireError(t, f.do(http.MethodDelete, "/b/k?uploadId=u2", ""), http.StatusNotFound, "NoSuchUpload")
}

func TestListParts(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().ListObjectParts(mock.Anything, domain.ListPartsInput{Bucket: "b", Key: "k", UploadID: "u1", PartNumberMarker: 2, MaxParts: 1000}).
		Return(domain.ListPartsResult{
			Upload: domain.MultipartUpload{UploadID: "u1", Checksum: domain.ChecksumCRC32},
			Parts: []domain.Part{
				{Number: 3, ETag: "e3", Size: 5 << 20, ModTime: now, Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: "c3"}},
				{Number: 4, ETag: "e4", Size: 1, ModTime: now, Checksum: domain.Checksum{Algorithm: domain.ChecksumSHA256, Value: "c4"}},
				{Number: 5, ETag: "e5", Size: 1, ModTime: now, Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: "c5"}},
				{Number: 6, ETag: "e6", Size: 1, ModTime: now, Checksum: domain.Checksum{Algorithm: domain.ChecksumSHA1, Value: "c6"}},
			},
			IsTruncated: true, NextPartNumberMarker: 6,
		}, nil).Once()
	rec := f.do(http.MethodGet, "/b/k?uploadId=u1&part-number-marker=2&max-parts=5000", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var res s3.ListPartsResult
	decodeXML(t, rec, &res)
	assert.Equal(t, 1000, res.MaxParts)
	assert.Equal(t, 2, res.PartNumberMarker)
	assert.Equal(t, 6, res.NextPartNumberMarker)
	assert.True(t, res.IsTruncated)
	assert.Equal(t, "STANDARD", res.StorageClass)
	assert.Equal(t, "CRC32", res.ChecksumAlgorithm)
	assert.Equal(t, "root", res.Owner.ID)
	require.Len(t, res.Parts, 4)
	assert.Equal(t, s3.PartEntry{PartNumber: 3, LastModified: "2026-10-08T12:00:00.000Z", ETag: `"e3"`, Size: 5 << 20, ChecksumFields: s3.ChecksumFields{ChecksumCRC32: "c3"}}, res.Parts[0])
	assert.Equal(t, "c4", res.Parts[1].ChecksumSHA256)
	assert.Equal(t, "c5", res.Parts[2].ChecksumCRC32C)
	assert.Equal(t, "c6", res.Parts[3].ChecksumSHA1)

	requireError(t, f.do(http.MethodGet, "/b/k?uploadId=u1&max-parts=-2", ""), http.StatusBadRequest, "InvalidArgument")
	requireError(t, f.do(http.MethodGet, "/b/k?uploadId=u1&part-number-marker=x", ""), http.StatusBadRequest, "InvalidArgument")
	f.objects.EXPECT().ListObjectParts(mock.Anything, mock.Anything).Return(domain.ListPartsResult{}, domain.ErrNoSuchUpload).Once()
	requireError(t, f.do(http.MethodGet, "/b/k?uploadId=nope", ""), http.StatusNotFound, "NoSuchUpload")
}

func TestListMultipartUploads(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().ListMultipartUploads(mock.Anything, domain.ListMultipartUploadsInput{
		Bucket: "b", Prefix: "a b/", Delimiter: "/", KeyMarker: "a b/0", UploadIDMarker: "u0", MaxUploads: 10,
	}).Return(domain.ListMultipartUploadsResult{
		Uploads:        []domain.MultipartUpload{{Key: "a b/k", UploadID: "u1", Initiated: now}},
		CommonPrefixes: []string{"a b/sub dir/"}, IsTruncated: true, NextKeyMarker: "a b/k", NextUploadIDMarker: "u1",
	}, nil).Once()
	rec := f.do(http.MethodGet, "/b?uploads&prefix=a%20b/&delimiter=/&key-marker=a%20b/0&upload-id-marker=u0&max-uploads=10&encoding-type=url", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var res s3.ListMultipartUploadsResult
	decodeXML(t, rec, &res)
	assert.Equal(t, "url", res.EncodingType)
	assert.Equal(t, "a+b/", res.Prefix)
	assert.Equal(t, "a+b/0", res.KeyMarker)
	assert.Equal(t, "a+b/k", res.NextKeyMarker)
	assert.Equal(t, "u1", res.NextUploadIDMarker)
	assert.Equal(t, "a+b/sub+dir/", res.CommonPrefixes[0].Prefix)
	require.Len(t, res.Uploads, 1)
	assert.Equal(t, s3.UploadEntry{Key: "a+b/k", UploadID: "u1", Initiator: res.Uploads[0].Owner, Owner: res.Uploads[0].Owner,
		StorageClass: "STANDARD", Initiated: "2026-10-08T12:00:00.000Z"}, res.Uploads[0])

	requireError(t, f.do(http.MethodGet, "/b?uploads&max-uploads=x", ""), http.StatusBadRequest, "InvalidArgument")
	requireError(t, f.do(http.MethodGet, "/b?uploads&encoding-type=zip", ""), http.StatusBadRequest, "InvalidArgument")
	f.objects.EXPECT().ListMultipartUploads(mock.Anything, mock.Anything).Return(domain.ListMultipartUploadsResult{}, domain.ErrBucketNotFound).Once()
	requireError(t, f.do(http.MethodGet, "/b?uploads", ""), http.StatusNotFound, "NoSuchBucket")
}
