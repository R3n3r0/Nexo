package admin_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
)

func newUploads(t *testing.T, opts ...admin.UploadManagerOption) (admin.UploadManager, *objectmock.Service) {
	objects := objectmock.NewService(t)
	m, err := admin.NewUploadManager(admin.UploadManagerDeps{Objects: objects}, opts...)
	require.NoError(t, err)
	return m, objects
}

func TestNewUploadManagerRequiresObjects(t *testing.T) {
	_, err := admin.NewUploadManager(admin.UploadManagerDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestUploadDefaultContentType: an upload without a type is stored as
// application/octet-stream; a given type is kept.
func TestUploadDefaultContentType(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct{ given, want string }{{"", admin.DefaultContentType}, {"image/png", "image/png"}} {
		t.Run(tt.want, func(t *testing.T) {
			m, objects := newUploads(t)
			objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool { return in.ContentType == tt.want })).
				Return(domain.ObjectInfo{Key: "k"}, nil)
			_, err := m.Upload(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", ContentType: tt.given, Reader: strings.NewReader("x"), Size: 1})
			require.NoError(t, err)
			objects.EXPECT().NewMultipartUpload(mock.Anything, object.NewMultipartInput{Bucket: "bkt", Key: "k", ContentType: tt.want}).
				Return(domain.MultipartUpload{UploadID: "u"}, nil)
			up, err := m.Create(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "k", ContentType: tt.given})
			require.NoError(t, err)
			assert.Equal(t, "u", up.Upload.UploadID)
		})
	}
}

// readAll is a PutObject/PutObjectPart stand-in that consumes the body as
// the object layer does and reports its read error.
func readAll(r io.Reader) error {
	_, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("put: %w", err)
	}
	return nil
}

// TestUploadLimits: the limits of the single-request uploads and of the
// parts are the service's: an announced size over the limit is refused
// before reading, a body of unknown size fails while streaming.
func TestUploadLimits(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		body string
		size int64
		// put reports whether the object layer is called.
		put  bool
		want error
	}{
		{name: "within the limit", body: "12345678", size: 8, put: true},
		{name: "announced over the limit", body: "123456789", size: 9, want: domain.ErrEntityTooLarge},
		{name: "unknown size over the limit", body: "123456789", size: -1, put: true, want: domain.ErrEntityTooLarge},
		{name: "unknown size within the limit", body: "1234", size: -1, put: true},
	}
	for _, tt := range tests {
		t.Run("upload "+tt.name, func(t *testing.T) {
			m, objects := newUploads(t, admin.WithMaxUploadSize(8))
			if tt.put {
				objects.EXPECT().PutObject(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in object.PutObjectInput) (domain.ObjectInfo, error) {
					return domain.ObjectInfo{Key: in.Key}, readAll(in.Reader)
				}).Once()
			}
			_, err := m.Upload(ctx, object.PutObjectInput{Bucket: "b", Key: "k", Reader: strings.NewReader(tt.body), Size: tt.size})
			if tt.want != nil {
				assert.ErrorIs(t, err, tt.want)
			} else {
				require.NoError(t, err)
			}
		})
	}
	const maxPart = 6 << 20
	partTests := []struct {
		name   string
		number int
		size   int64
		body   io.Reader
		put    bool
		want   error
	}{
		{name: "stored", number: 2, size: 4, body: strings.NewReader("data"), put: true},
		{name: "number 0", number: 0, size: 4, body: strings.NewReader("data"), want: domain.ErrInvalidArgument},
		{name: "number over the S3 maximum", number: admin.MaxUploadParts + 1, size: 4, body: strings.NewReader("data"), want: domain.ErrInvalidArgument},
		{name: "size required", number: 1, size: -1, body: strings.NewReader("data"), want: domain.ErrInvalidArgument},
		{name: "announced over the limit", number: 1, size: maxPart + 1, body: strings.NewReader("x"), want: domain.ErrEntityTooLarge},
		{name: "longer than announced", number: 1, size: 1, body: io.LimitReader(zeros{}, maxPart+1), put: true, want: domain.ErrEntityTooLarge},
	}
	for _, tt := range partTests {
		t.Run("part "+tt.name, func(t *testing.T) {
			m, objects := newUploads(t, admin.WithMaxPartSize(maxPart))
			if tt.put {
				objects.EXPECT().PutObjectPart(mock.Anything, mock.MatchedBy(func(in object.PutObjectPartInput) bool {
					return in.UploadID == "u1" && in.PartNumber == tt.number && in.Size == tt.size
				})).RunAndReturn(func(_ context.Context, in object.PutObjectPartInput) (domain.Part, error) {
					return domain.Part{Number: in.PartNumber, ETag: "e"}, readAll(in.Reader)
				}).Once()
			}
			p, err := m.UploadPart(ctx, object.PutObjectPartInput{Bucket: "b", Key: "k", UploadID: "u1", PartNumber: tt.number, Size: tt.size, Reader: tt.body})
			if tt.want != nil {
				assert.ErrorIs(t, err, tt.want)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "e", p.ETag)
		})
	}
}

// zeros is an endless body.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestUploadPartLimits: the limits returned with a new upload follow the
// configured part limit (the recommendation never exceeds it).
func TestUploadPartLimits(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		opts    []admin.UploadManagerOption
		maxPart int64
		size    int64
	}{
		{name: "defaults", maxPart: admin.DefaultMaxPartSize, size: admin.RecommendedPartSize},
		{name: "small part limit", opts: []admin.UploadManagerOption{admin.WithMaxPartSize(6 << 20)}, maxPart: 6 << 20, size: 6 << 20},
		{name: "below the S3 minimum is ignored", opts: []admin.UploadManagerOption{admin.WithMaxPartSize(1 << 20)}, maxPart: admin.DefaultMaxPartSize, size: admin.RecommendedPartSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, objects := newUploads(t, tt.opts...)
			objects.EXPECT().NewMultipartUpload(mock.Anything, mock.Anything).Return(domain.MultipartUpload{UploadID: "u"}, nil)
			up, err := m.Create(ctx, object.NewMultipartInput{Bucket: "b", Key: "k"})
			require.NoError(t, err)
			assert.Equal(t, admin.UploadLimits{
				MaxUploadSize: admin.DefaultMaxUploadSize, PartSize: tt.size, MinPartSize: admin.MinPartSize, MaxPartSize: tt.maxPart, MaxParts: admin.MaxUploadParts,
			}, up.Limits)
		})
	}
}

// TestUploadListsPage: the service pages the pending uploads and the parts
// with opaque cursors.
func TestUploadListsPage(t *testing.T) {
	ctx := context.Background()
	m, objects := newUploads(t)

	objects.EXPECT().ListMultipartUploads(mock.Anything, domain.ListMultipartUploadsInput{Bucket: "b", Prefix: "x/", MaxUploads: 1}).
		Return(domain.ListMultipartUploadsResult{Uploads: []domain.MultipartUpload{{Key: "x/1", UploadID: "u1"}}, IsTruncated: true, NextKeyMarker: "x/1", NextUploadIDMarker: "u1"}, nil).Once()
	page, err := m.List(ctx, admin.ListUploadsInput{Bucket: "b", Prefix: "x/", Page: admin.Page{Limit: 1}})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.NotEmpty(t, page.NextCursor)
	objects.EXPECT().ListMultipartUploads(mock.Anything, domain.ListMultipartUploadsInput{Bucket: "b", Prefix: "x/", KeyMarker: "x/1", UploadIDMarker: "u1", MaxUploads: admin.MaxPageLimit}).
		Return(domain.ListMultipartUploadsResult{}, nil).Once()
	page, err = m.List(ctx, admin.ListUploadsInput{Bucket: "b", Prefix: "x/", Page: admin.Page{Cursor: page.NextCursor}})
	require.NoError(t, err)
	assert.Empty(t, page.NextCursor)
	assert.NotNil(t, page.Items, "an empty page is an empty list")

	up := admin.UploadRef{Bucket: "b", Key: "k", UploadID: "u1"}
	objects.EXPECT().ListObjectParts(mock.Anything, domain.ListPartsInput{Bucket: "b", Key: "k", UploadID: "u1", MaxParts: 2}).
		Return(domain.ListPartsResult{Parts: []domain.Part{{Number: 1}, {Number: 2}}, IsTruncated: true, NextPartNumberMarker: 2}, nil).Once()
	parts, err := m.Parts(ctx, admin.ListPartsInput{Upload: up, Page: admin.Page{Limit: 2}})
	require.NoError(t, err)
	require.Len(t, parts.Items, 2)
	objects.EXPECT().ListObjectParts(mock.Anything, domain.ListPartsInput{Bucket: "b", Key: "k", UploadID: "u1", PartNumberMarker: 2, MaxParts: admin.MaxPageLimit}).
		Return(domain.ListPartsResult{Parts: []domain.Part{{Number: 3}}}, nil).Once()
	parts, err = m.Parts(ctx, admin.ListPartsInput{Upload: up, Page: admin.Page{Cursor: parts.NextCursor}})
	require.NoError(t, err)
	assert.Equal(t, 3, parts.Items[0].Number)
	assert.Empty(t, parts.NextCursor)

	for _, p := range []admin.Page{{Cursor: "%%%"}, {Limit: admin.MaxPageLimit + 1}} {
		_, err = m.List(ctx, admin.ListUploadsInput{Bucket: "b", Page: p})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		_, err = m.Parts(ctx, admin.ListPartsInput{Upload: up, Page: p})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	}
}

func TestUploadCompleteAndAbort(t *testing.T) {
	ctx := context.Background()
	m, objects := newUploads(t)
	in := object.CompleteMultipartInput{Bucket: "b", Key: "k", UploadID: "u1", Parts: []domain.CompletePart{{Number: 1, ETag: "e1"}}}
	objects.EXPECT().CompleteMultipartUpload(mock.Anything, in).Return(domain.ObjectInfo{Key: "k", Size: 9}, nil)
	info, err := m.Complete(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, int64(9), info.Size)
	objects.EXPECT().AbortMultipartUpload(mock.Anything, "b", "k", "u9").Return(fmt.Errorf("x: %w", domain.ErrNoSuchUpload))
	assert.ErrorIs(t, m.Abort(ctx, admin.UploadRef{Bucket: "b", Key: "k", UploadID: "u9"}), domain.ErrNoSuchUpload)
}
