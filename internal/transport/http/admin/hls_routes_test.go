package admin_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/policy"
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
