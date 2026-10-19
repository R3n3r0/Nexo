package admin_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

func TestHLSRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)

	// The master playlist links the media playlist relative to it, with
	// the key, the version and the revision.
	f.media.EXPECT().MasterPlaylist(mock.Anything, media.Source{Bucket: "b", Key: "v/a b.mp4", VersionID: "v1"}, mock.Anything).
		RunAndReturn(func(_ context.Context, _ media.Source, l media.Links) ([]byte, error) {
			return []byte(l.Media("e1") + "\n" + l.Init("e1") + "\n" + l.Segment("e1", 3)), nil
		})
	f.resetChecks()
	rec := c.do(http.MethodGet, "/buckets/b/objects/hls/master.m3u8?key=v%2Fa+b.mp4&versionId=v1", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/vnd.apple.mpegurl", rec.Header().Get("Content-Type"))
	assert.Equal(t, "private, no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "media.m3u8?key=v%2Fa+b.mp4&rev=e1&versionId=v1\n"+
		"init.mp4?key=v%2Fa+b.mp4&rev=e1&versionId=v1\n"+
		"segment.m4s?key=v%2Fa+b.mp4&n=3&rev=e1&versionId=v1", rec.Body.String())
	check := f.lastCheck()
	assert.Equal(t, "b", check.Bucket)
	assert.Equal(t, "v/a b.mp4", check.Object)
	assert.Equal(t, []policy.Action{policy.ActionGetObjectVersion}, check.Actions)

	// The pinned resources.
	src := media.Source{Bucket: "b", Key: "v.mp4", Revision: "e1"}
	f.media.EXPECT().MediaPlaylist(mock.Anything, src, mock.Anything).Return([]byte("#EXTM3U\n"), nil)
	f.media.EXPECT().Init(mock.Anything, src).Return([]byte("init"), nil)
	f.media.EXPECT().Segment(mock.Anything, src, 2).Return([]byte("seg"), nil)
	for _, tc := range []struct{ path, contentType, body string }{
		{"media.m3u8?key=v.mp4&rev=e1", "application/vnd.apple.mpegurl", "#EXTM3U\n"},
		{"init.mp4?key=v.mp4&rev=e1", "video/mp4", "init"},
		{"segment.m4s?key=v.mp4&rev=e1&n=2", "video/iso.segment", "seg"},
	} {
		f.resetChecks()
		rec := c.do(http.MethodGet, "/buckets/b/objects/hls/"+tc.path, nil)
		require.Equal(t, http.StatusOK, rec.Code, tc.path)
		assert.Equal(t, tc.contentType, rec.Header().Get("Content-Type"), tc.path)
		assert.Equal(t, "private, max-age=86400, immutable", rec.Header().Get("Cache-Control"), tc.path)
		assert.Equal(t, tc.body, rec.Body.String(), tc.path)
		assert.Equal(t, []policy.Action{policy.ActionGetObject}, f.lastCheck().Actions, tc.path)
	}

	// Malformed requests never reach the service.
	for _, path := range []string{
		"master.m3u8", "media.m3u8?key=v.mp4", "init.mp4?key=v.mp4", "segment.m4s?key=v.mp4&rev=e1",
		"segment.m4s?key=v.mp4&rev=e1&n=-1", "segment.m4s?key=v.mp4&rev=e1&n=x",
	} {
		rec := c.do(http.MethodGet, "/buckets/b/objects/hls/"+path, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, path)
	}

	// The errors of the service.
	for _, tc := range []struct {
		err    error
		status int
	}{
		{fmt.Errorf("%w: fragmented", media.ErrUnsupported), http.StatusBadRequest},
		{fmt.Errorf("%w: changed", domain.ErrPreconditionFailed), http.StatusPreconditionFailed},
		{fmt.Errorf("%w: k", domain.ErrObjectNotFound), http.StatusNotFound},
	} {
		f.media.EXPECT().Init(mock.Anything, media.Source{Bucket: "b", Key: "x.mp4", Revision: "e2"}).Return(nil, tc.err).Once()
		rec := c.do(http.MethodGet, "/buckets/b/objects/hls/init.mp4?key=x.mp4&rev=e2", nil)
		assert.Equal(t, tc.status, rec.Code, tc.err.Error())
	}

	// A denied read.
	f.denyAll()
	rec = c.do(http.MethodGet, "/buckets/b/objects/hls/master.m3u8?key=v.mp4", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// imageContent is the content of a variant served from memory.
type imageContent struct{ *bytes.Reader }

func (imageContent) Close() error { return nil }

func testImage(format string, width int, data string) *media.Image {
	return &media.Image{
		MediaImageOutput: domain.MediaImageOutput{Format: format, Width: width, PixelWidth: 640, PixelHeight: 480, Size: int64(len(data))},
		ContentType:      domain.MediaImageContentType(format), ETag: `"e1-` + format + `-` + strconv.Itoa(width) + `"`,
		Modified: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), Content: imageContent{bytes.NewReader([]byte(data))},
	}
}

func TestImageRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)

	// An explicit format and width.
	f.media.EXPECT().Image(mock.Anything, media.Source{Bucket: "b", Key: "p/a b.jpg"}, media.ImageRequest{Width: 640, Format: "webp", Accept: "image/webp"}).
		Return(testImage("webp", 640, "webp bytes"), nil).Once()
	f.resetChecks()
	req := c.newRequest(http.MethodGet, "/buckets/b/objects/image?key=p%2Fa+b.jpg&w=640&format=webp", nil)
	req.Header.Set("Accept", "image/webp")
	rec := c.serve(req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "image/webp", rec.Header().Get("Content-Type"))
	assert.Equal(t, `"e1-webp-640"`, rec.Header().Get("ETag"))
	assert.Equal(t, "Mon, 19 Oct 2026 12:00:00 GMT", rec.Header().Get("Last-Modified"))
	assert.Equal(t, "private, no-cache", rec.Header().Get("Cache-Control"))
	assert.Empty(t, rec.Header().Get("Vary"), "an explicit format does not vary")
	assert.Equal(t, "webp bytes", rec.Body.String())
	assert.Equal(t, []policy.Action{policy.ActionGetObject}, f.lastCheck().Actions)
	assert.Equal(t, "p/a b.jpg", f.lastCheck().Object)

	// Format auto (the default) varies on Accept; jpg is jpeg.
	f.media.EXPECT().Image(mock.Anything, media.Source{Bucket: "b", Key: "a.jpg", VersionID: "v1"}, media.ImageRequest{Format: "auto", Accept: "image/avif,*/*"}).
		Return(testImage("avif", 0, "avif bytes"), nil).Once()
	f.resetChecks()
	req = c.newRequest(http.MethodGet, "/buckets/b/objects/image?key=a.jpg&versionId=v1", nil)
	req.Header.Set("Accept", "image/avif,*/*")
	rec = c.serve(req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "image/avif", rec.Header().Get("Content-Type"))
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
	assert.Equal(t, []policy.Action{policy.ActionGetObjectVersion}, f.lastCheck().Actions)
	f.media.EXPECT().Image(mock.Anything, media.Source{Bucket: "b", Key: "a.jpg"}, media.ImageRequest{Format: "jpeg"}).
		Return(testImage("jpeg", 0, "jpeg bytes"), nil).Once()
	rec = c.do(http.MethodGet, "/buckets/b/objects/image?key=a.jpg&format=JPG", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Conditional requests and ranges.
	f.media.EXPECT().Image(mock.Anything, media.Source{Bucket: "b", Key: "a.jpg"}, mock.Anything).
		RunAndReturn(func(context.Context, media.Source, media.ImageRequest) (*media.Image, error) {
			return testImage("jpeg", 0, "0123456789"), nil
		}).Times(2)
	req = c.newRequest(http.MethodGet, "/buckets/b/objects/image?key=a.jpg&format=jpeg", nil)
	req.Header.Set("If-None-Match", `"e1-jpeg-0"`)
	rec = c.serve(req)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	req = c.newRequest(http.MethodGet, "/buckets/b/objects/image?key=a.jpg&format=jpeg", nil)
	req.Header.Set("Range", "bytes=2-4")
	rec = c.serve(req)
	require.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, "234", rec.Body.String())
	assert.Equal(t, "bytes 2-4/10", rec.Header().Get("Content-Range"))

	// Malformed requests never reach the service.
	for _, q := range []string{"", "key=a.jpg&w=x", "key=a.jpg&w=-1", "key=a.jpg&format=gif"} {
		rec := c.do(http.MethodGet, "/buckets/b/objects/image?"+q, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, q)
	}

	// Not processed, stale or another format: 404.
	f.media.EXPECT().Image(mock.Anything, media.Source{Bucket: "b", Key: "new.jpg"}, mock.Anything).
		Return(nil, fmt.Errorf("%w: no media output", domain.ErrNotFound)).Once()
	rec = c.do(http.MethodGet, "/buckets/b/objects/image?key=new.jpg", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// The status lists the variants with their URL.
	f.media.EXPECT().Status(mock.Anything, media.Source{Bucket: "b", Key: "p/a b.jpg"}).Return(&media.OutputStatus{VersionID: "v1", Current: true,
		MediaStatus: domain.MediaStatus{State: domain.MediaStateDone, Rule: "images", Width: 1000, Height: 750, Images: []domain.MediaImageOutput{
			{Format: "webp", Width: 640, PixelWidth: 640, PixelHeight: 480, Size: 20},
			{Format: "jpeg", Width: 0, PixelWidth: 1000, PixelHeight: 750, Size: 50},
		}}}, nil).Once()
	rec = c.do(http.MethodGet, "/buckets/b/objects/media/status?key=p%2Fa+b.jpg", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	st := decode[admin.MediaStatusResponse](t, rec)
	require.Len(t, st.Images, 2)
	assert.Equal(t, admin.MediaImageResponse{MediaImageOutput: domain.MediaImageOutput{Format: "webp", Width: 640, PixelWidth: 640, PixelHeight: 480, Size: 20},
		ContentType: "image/webp", URL: "/api/v1/buckets/b/objects/image?format=webp&key=p%2Fa+b.jpg&w=640"}, st.Images[0])
	assert.Equal(t, "/api/v1/buckets/b/objects/image?format=jpeg&key=p%2Fa+b.jpg", st.Images[1].URL, "the image at its own size")

	// A denied read.
	f.denyAll()
	rec = c.do(http.MethodGet, "/buckets/b/objects/image?key=a.jpg", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
