package s3_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

// readSeekCloser is the content of a variant of the tests.
type readSeekCloser struct{ *bytes.Reader }

func (readSeekCloser) Close() error { return nil }

func variantOf(format string, width int, data string) *media.Image {
	return &media.Image{
		MediaImageOutput: domain.MediaImageOutput{Format: format, Width: width, PixelWidth: max(width, 1000), Size: int64(len(data))},
		ContentType:      domain.MediaImageContentType(format),
		ETag:             fmt.Sprintf(`"src-%s-%d"`, format, width),
		Modified:         now.Add(-time.Hour),
		Content:          readSeekCloser{bytes.NewReader([]byte(data))},
	}
}

// TestGetObjectWithoutImageParams: GetObject without the Nexo parameters
// never asks for a variant, whatever its Accept: the bytes and headers of
// the object, as S3.
func TestGetObjectWithoutImageParams(t *testing.T) {
	f := newFixture(t)
	info := domain.ObjectInfo{Bucket: "pics", Key: "a.jpg", Size: 8, ETag: "abc", ModTime: now, ContentType: "image/jpeg"}
	f.objects.EXPECT().GetObject(mock.Anything, "pics", "a.jpg", object.GetObjectOptions{}).Return(body("original", info, 0, 8), nil).Once()
	f.objects.EXPECT().GetObjectInfo(mock.Anything, "pics", "a.jpg", object.GetObjectOptions{}).Return(info, nil).Once()
	rec := f.do(http.MethodGet, "/pics/a.jpg?w=640&format=webp", "", "Accept", "image/avif,image/webp,*/*")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "original", rec.Body.String())
	assert.Equal(t, "image/jpeg", rec.Header().Get("Content-Type"))
	assert.Empty(t, rec.Header().Values("Vary"))
	assert.Empty(t, rec.Header().Get(s3.HeaderImageVariant))
	rec = f.do(http.MethodHead, "/pics/a.jpg", "", "Accept", "image/avif")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "8", rec.Header().Get("Content-Length"))
	// The media service is never called (its mock has no expectation).
}

// TestGetObjectImageVariant: x-nexo-image-format and x-nexo-image-width
// serve a variant, the original when there is none, refuse a malformed
// value; the authorization is GetObject (GetObjectVersion) of the image.
func TestGetObjectImageVariant(t *testing.T) {
	f := newFixture(t)
	src := media.Source{Bucket: "pics", Key: "a b.jpg"}
	f.media.EXPECT().Image(mock.Anything, src, media.ImageRequest{Format: media.ImageFormatAuto, Width: 640, Accept: "image/webp"}).
		Return(variantOf("webp", 640, "webp-640"), nil).Once()
	rec := f.do(http.MethodGet, "/pics/a%20b.jpg?x-nexo-image-width=640", "", "Accept", "image/webp")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "webp-640", rec.Body.String())
	assert.Equal(t, "image/webp", rec.Header().Get("Content-Type"))
	assert.Equal(t, `"src-webp-640"`, rec.Header().Get("ETag"))
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
	assert.Equal(t, "webp 640", rec.Header().Get(s3.HeaderImageVariant))

	// A format named: no Vary; a range and a conditional request.
	f.media.EXPECT().Image(mock.Anything, src, media.ImageRequest{Format: "jpeg", Accept: ""}).
		RunAndReturn(func(context.Context, media.Source, media.ImageRequest) (*media.Image, error) {
			return variantOf("jpeg", 0, "jpeg-full"), nil
		}).Maybe()
	rec = f.do(http.MethodGet, "/pics/a%20b.jpg?x-nexo-image-format=jpg", "", "Range", "bytes=0-3")
	require.Equal(t, http.StatusPartialContent, rec.Code, rec.Body.String())
	assert.Equal(t, "jpeg", rec.Body.String())
	assert.Empty(t, rec.Header().Get("Vary"))
	rec = f.do(http.MethodGet, "/pics/a%20b.jpg?x-nexo-image-format=jpeg", "", "If-None-Match", `"src-jpeg-0"`)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	rec = f.do(http.MethodHead, "/pics/a%20b.jpg?x-nexo-image-format=jpeg", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "9", rec.Header().Get("Content-Length"))
	assert.Empty(t, rec.Body.String())

	// No variant: the original.
	info := domain.ObjectInfo{Bucket: "pics", Key: "plain.jpg", Size: 8, ETag: "abc", ModTime: now, ContentType: "image/jpeg"}
	f.media.EXPECT().Image(mock.Anything, media.Source{Bucket: "pics", Key: "plain.jpg"}, mock.Anything).
		Return(nil, fmt.Errorf("%w: no media output", domain.ErrNotFound)).Once()
	f.objects.EXPECT().GetObject(mock.Anything, "pics", "plain.jpg", object.GetObjectOptions{}).
		RunAndReturn(func(context.Context, string, string, object.GetObjectOptions) (*object.Reader, error) {
			return body("original", info, 0, 8), nil
		}).Twice()
	rec = f.do(http.MethodGet, "/pics/plain.jpg?x-nexo-image-format=webp", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "original", rec.Body.String())
	assert.Empty(t, rec.Header().Get(s3.HeaderImageVariant))
	// original: the media service is not asked.
	rec = f.do(http.MethodGet, "/pics/plain.jpg?x-nexo-image-format=original&x-nexo-image-width=10", "")
	assert.Equal(t, "original", rec.Body.String())

	// Malformed values, a part.
	for _, q := range []string{"x-nexo-image-format=gif", "x-nexo-image-width=-1", "x-nexo-image-width=big", "x-nexo-image-format=webp&partNumber=1"} {
		requireError(t, f.do(http.MethodGet, "/pics/plain.jpg?"+q, ""), http.StatusBadRequest, "InvalidArgument")
	}

	// A version: GetObjectVersion of the image.
	f.media.EXPECT().Image(mock.Anything, media.Source{Bucket: "pics", Key: "v.jpg", VersionID: "v1"}, mock.Anything).
		Return(variantOf("avif", 0, "avif"), nil).Once()
	rec = f.do(http.MethodGet, "/pics/v.jpg?versionId=v1&x-nexo-image-format=auto", "", "Accept", "image/avif")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "avif", rec.Body.String())
}

// TestGetObjectImageVariantDenied: the read of the image is authorized
// before any variant is looked up.
func TestGetObjectImageVariantDenied(t *testing.T) {
	f := newFixture(t, withStrictPolicy(), asIdentity(&domain.Identity{AccessKey: "bob"}))
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Action == policy.ActionGetObject && a.Bucket == "pics" && a.Object == "a.jpg"
	})).Return(policy.Decision{Reason: "no"}, nil).Once()
	requireError(t, f.do(http.MethodGet, "/pics/a.jpg?x-nexo-image-format=webp", ""), http.StatusForbidden, "AccessDenied")
}

// TestPresignedGetObjectImageVariant: a presigned GetObject carries the
// parameters in its signed query.
func TestPresignedGetObjectImageVariant(t *testing.T) {
	f := newFixture(t, withoutIdentity())
	f.verifier.EXPECT().VerifyPresigned(mock.Anything, mock.Anything).Return(&domain.Identity{AccessKey: "alice"}, nil).Once()
	f.media.EXPECT().Image(mock.Anything, media.Source{Bucket: "b", Key: "k"}, media.ImageRequest{Format: "webp", Width: 320}).
		Return(variantOf("webp", 320, "webp-320"), nil).Once()
	rec := f.do(http.MethodGet, "/b/k?x-nexo-image-format=webp&x-nexo-image-width=320&"+presignedQuery, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "webp-320", rec.Body.String())
}
