package distribution_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/media"
	mediamock "github.com/r3n3r0/nexo/internal/service/media/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	polmock "github.com/r3n3r0/nexo/internal/service/policy/mock"
)

// videoSite is a website distribution streaming the videos under
// /videos/ (and a signed behavior under /private/).
func videoSite(t *testing.T) *distribution.Distribution {
	t.Helper()
	svc := newService(t)
	_, err := svc.CreateKeyGroup(context.Background(), "players", "")
	require.NoError(t, err)
	d := sample("videos", "tv.example.com")
	d.Behaviors = []distribution.Behavior{
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/videos/"},
			Video: &distribution.Video{HLS: true}},
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/private/"},
			Video: &distribution.Video{HLS: true}, Access: distribution.AccessSigned, Signing: &distribution.Signing{KeyGroup: "players"}},
	}
	stored, err := svc.Create(context.Background(), d)
	require.NoError(t, err)
	return &stored
}

// videoPolicy denies explicitly the actions of denied (object → actions).
func videoPolicy(t *testing.T, denied map[string][]policy.Action) *polmock.Service {
	t.Helper()
	m := polmock.NewService(t)
	m.EXPECT().Evaluate(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, a policy.Args) (policy.Decision, error) {
		for _, act := range denied[a.Object] {
			if act == a.Action {
				return policy.Decision{Source: policy.SourceBucket, MatchedStatement: &domain.Statement{Effect: domain.Deny}}, nil
			}
		}
		return policy.Decision{}, nil
	}).Maybe()
	return m
}

func newVideoEngine(t *testing.T, m media.Service, denied map[string][]policy.Action) *engineFixture {
	t.Helper()
	fx := &engineFixture{media: m}
	return fx.start(t, videoPolicy(t, denied))
}

// links writes the URIs the packager would put in a playlist.
func links(l media.Links) []byte {
	return []byte(l.Media("e1") + "\n" + l.Init("e1") + "\n" + l.Segment("e1", 3) + "\n" + l.Rendition("720p", "e7"))
}

// TestVideoStreamed: a video behavior serves the presentation of the
// video below its URL, with relative URIs, cacheable when pinned.
func TestVideoStreamed(t *testing.T) {
	d := videoSite(t)
	m := mediamock.NewService(t)
	fx := newVideoEngine(t, m, nil)
	fx.put("videos/intro.mp4", "mp4", "video/mp4")
	src := media.Source{Bucket: d.Origin.Bucket, Key: "videos/intro.mp4"}

	m.EXPECT().MasterPlaylist(tmock.Anything, src, tmock.Anything).RunAndReturn(func(_ context.Context, _ media.Source, l media.Links) ([]byte, error) {
		return links(l), nil
	})
	resp, body := fx.do(t, d, webRequest{url: "https://tv.example.com/videos/intro.mp4/master.m3u8"})
	require.Equal(t, http.StatusOK, resp.Status, body)
	assert.Equal(t, "application/vnd.apple.mpegurl", resp.Header.Get("Content-Type"))
	assert.Equal(t, "public, no-cache", resp.Header.Get("Cache-Control"))
	assert.Equal(t, distribution.OutcomeVideo, resp.Outcome)
	assert.Equal(t, "videos/intro.mp4", resp.Key)
	assert.Equal(t, "media.m3u8?rev=e1\ninit.mp4?rev=e1\nsegment.m4s?n=3&rev=e1\nmedia.m3u8?r=720p&rev=e7", body)

	pinned := src
	pinned.Revision = "e1"
	rendition := pinned
	rendition.Rendition = "720p"
	m.EXPECT().MediaPlaylist(tmock.Anything, rendition, tmock.Anything).RunAndReturn(func(_ context.Context, _ media.Source, l media.Links) ([]byte, error) {
		return []byte(l.Segment("e1", 0)), nil
	})
	m.EXPECT().Init(tmock.Anything, pinned).Return([]byte("init"), nil)
	m.EXPECT().Segment(tmock.Anything, pinned, 3).Return([]byte("seg"), nil)
	for _, tc := range []struct{ path, ctype, body string }{
		{"media.m3u8?rev=e1&r=720p", "application/vnd.apple.mpegurl", "segment.m4s?n=0&r=720p&rev=e1"},
		{"init.mp4?rev=e1", "video/mp4", "init"},
		{"segment.m4s?rev=e1&n=3", "video/iso.segment", "seg"},
	} {
		resp, body := fx.do(t, d, webRequest{url: "https://tv.example.com/videos/intro.mp4/" + tc.path})
		require.Equal(t, http.StatusOK, resp.Status, tc.path+": "+body)
		assert.Equal(t, tc.ctype, resp.Header.Get("Content-Type"), tc.path)
		assert.Equal(t, "public, max-age=86400, immutable", resp.Header.Get("Cache-Control"), tc.path)
		assert.Equal(t, tc.body, body, tc.path)
	}

	// HEAD: the headers without the body.
	resp, body = fx.do(t, d, webRequest{method: http.MethodHead, url: "https://tv.example.com/videos/intro.mp4/init.mp4?rev=e1"})
	require.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "4", resp.Header.Get("Content-Length"))
	assert.Empty(t, body)

	// What the media worker derived.
	m.EXPECT().Sidecar(tmock.Anything, src, domain.MediaPosterFile).Return([]byte("jpeg"), nil)
	m.EXPECT().Sidecar(tmock.Anything, src, domain.MediaThumbnailsFile).Return([]byte("WEBVTT\n\n00:00.000 --> 00:05.000\nsprite.jpg#xywh=0,0,160,90\n"), nil)
	resp, body = fx.do(t, d, webRequest{url: "https://tv.example.com/videos/intro.mp4/poster.jpg"})
	require.Equal(t, http.StatusOK, resp.Status, body)
	assert.Equal(t, "image/jpeg", resp.Header.Get("Content-Type"))
	assert.Equal(t, "jpeg", body)
	resp, body = fx.do(t, d, webRequest{url: "https://tv.example.com/videos/intro.mp4/thumbnails.vtt"})
	require.Equal(t, http.StatusOK, resp.Status, body)
	assert.Equal(t, "text/vtt; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Contains(t, body, "sprite.jpg#xywh=0,0,160,90")

	// Malformed requests.
	for _, path := range []string{"init.mp4", "segment.m4s?rev=e1", "segment.m4s?rev=e1&n=-2"} {
		resp, body := fx.do(t, d, webRequest{url: "https://tv.example.com/videos/intro.mp4/" + path})
		assert.Equal(t, http.StatusBadRequest, resp.Status, path+": "+body)
	}
}

// TestVideoErrors: the errors of the packager, and the paths that are not
// about a video.
func TestVideoErrors(t *testing.T) {
	d := videoSite(t)
	m := mediamock.NewService(t)
	fx := newVideoEngine(t, m, nil)
	fx.put("videos/intro.mp4", "mp4", "video/mp4")
	fx.put("videos/notes.txt", "text", "text/plain")
	fx.put("videos/notes.txt/master.m3u8", "a real object", "text/plain")
	fx.put("other/clip.mp4", "mp4", "video/mp4")
	bucket := d.Origin.Bucket

	m.EXPECT().Init(tmock.Anything, media.Source{Bucket: bucket, Key: "videos/intro.mp4", Revision: "old"}).
		Return(nil, fmt.Errorf("%w: changed", domain.ErrPreconditionFailed))
	m.EXPECT().Segment(tmock.Anything, media.Source{Bucket: bucket, Key: "videos/intro.mp4", Revision: "e1"}, 99).
		Return(nil, fmt.Errorf("%w: segment 99", domain.ErrNotFound))
	m.EXPECT().Sidecar(tmock.Anything, media.Source{Bucket: bucket, Key: "videos/intro.mp4"}, domain.MediaSpriteFile).
		Return(nil, errors.New("disk on fire"))
	m.EXPECT().MasterPlaylist(tmock.Anything, media.Source{Bucket: bucket, Key: "videos/notes.txt"}, tmock.Anything).
		Return(nil, fmt.Errorf("%w: not an MP4", media.ErrUnsupported))
	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{
		{path: "/videos/intro.mp4/init.mp4?rev=old", status: http.StatusPreconditionFailed},
		{path: "/videos/intro.mp4/segment.m4s?rev=e1&n=99", status: http.StatusNotFound},
		{path: "/videos/intro.mp4/sprite.jpg", status: http.StatusInternalServerError},
		// Not a video: the object of the key answers.
		{path: "/videos/notes.txt/master.m3u8", status: http.StatusOK, body: "a real object"},
		// No video at that key.
		{path: "/videos/missing.mp4/master.m3u8", status: http.StatusNotFound},
		// A behavior without video streaming.
		{path: "/other/clip.mp4/master.m3u8", status: http.StatusNotFound},
		// Not a file of a presentation.
		{path: "/videos/intro.mp4/other.m3u8", status: http.StatusNotFound},
	} {
		resp, body := fx.do(t, d, webRequest{url: "https://tv.example.com" + tc.path})
		assert.Equal(t, tc.status, resp.Status, tc.path+": "+body)
		if tc.body != "" {
			assert.Equal(t, tc.body, body, tc.path)
		}
	}
}

// TestVideoStreamOnly: nexo:GetMediaStream streams a video whose download
// s3:GetObject denies; denying both refuses the stream.
func TestVideoStreamOnly(t *testing.T) {
	d := videoSite(t)
	m := mediamock.NewService(t)
	fx := newVideoEngine(t, m, map[string][]policy.Action{
		"videos/film.mp4":   {policy.ActionGetObject},
		"videos/secret.mp4": {policy.ActionGetObject, policy.ActionGetMediaStream},
	})
	fx.put("videos/film.mp4", "mp4", "video/mp4")
	fx.put("videos/secret.mp4", "mp4", "video/mp4")
	m.EXPECT().MasterPlaylist(tmock.Anything, media.Source{Bucket: d.Origin.Bucket, Key: "videos/film.mp4"}, tmock.Anything).
		Return([]byte("#EXTM3U\n"), nil)

	resp, body := fx.do(t, d, webRequest{url: "https://tv.example.com/videos/film.mp4/master.m3u8"})
	assert.Equal(t, http.StatusOK, resp.Status, body)
	resp, body = fx.do(t, d, webRequest{url: "https://tv.example.com/videos/film.mp4"})
	assert.Equal(t, http.StatusForbidden, resp.Status, "the original cannot be downloaded: "+body)
	resp, body = fx.do(t, d, webRequest{url: "https://tv.example.com/videos/secret.mp4/master.m3u8"})
	assert.Equal(t, http.StatusForbidden, resp.Status, body)

	// Block public access refuses the public stream.
	fx.blocked = true
	resp, body = fx.do(t, d, webRequest{url: "https://tv.example.com/videos/film.mp4/master.m3u8"})
	assert.Equal(t, http.StatusForbidden, resp.Status, body)
}

// TestVideoSigned: a signed behavior needs the signature on every file;
// the playlists carry the parameters of the signed URL to their URIs.
func TestVideoSigned(t *testing.T) {
	d := videoSite(t)
	m := mediamock.NewService(t)
	fx := newVideoEngine(t, m, nil)
	fx.put("private/talk.mp4", "mp4", "video/mp4")
	src := media.Source{Bucket: d.Origin.Bucket, Key: "private/talk.mp4"}
	m.EXPECT().MasterPlaylist(tmock.Anything, src, tmock.Anything).RunAndReturn(func(_ context.Context, _ media.Source, l media.Links) ([]byte, error) {
		return []byte(l.Media("e1")), nil
	})

	resp, body := fx.do(t, d, webRequest{url: "https://tv.example.com/private/talk.mp4/master.m3u8"})
	assert.Equal(t, http.StatusForbidden, resp.Status, body)

	signed := "Policy=cG9saWN5&Signature=good&Key-Pair-Id=K1"
	resp, body = fx.do(t, d, webRequest{url: "https://tv.example.com/private/talk.mp4/master.m3u8?" + signed})
	require.Equal(t, http.StatusOK, resp.Status, body)
	assert.Equal(t, "private, no-cache", resp.Header.Get("Cache-Control"))
	u, err := url.Parse(body)
	require.NoError(t, err)
	assert.Equal(t, "media.m3u8", u.Path)
	q := u.Query()
	assert.Equal(t, "e1", q.Get("rev"))
	assert.Equal(t, "good", q.Get("Signature"))
	assert.Equal(t, "cG9saWN5", q.Get("Policy"))
	assert.Equal(t, "K1", q.Get("Key-Pair-Id"))

	// Signed cookies work the same, and the URIs carry nothing.
	h := http.Header{}
	h.Add("Cookie", distribution.CookieSignature+"=good")
	resp, body = fx.do(t, d, webRequest{url: "https://tv.example.com/private/talk.mp4/master.m3u8", header: h})
	require.Equal(t, http.StatusOK, resp.Status, body)
	assert.Equal(t, "media.m3u8?rev=e1", body)
}

// TestVideoValidation: video streaming is a website behavior.
func TestVideoValidation(t *testing.T) {
	svc := newService(t)
	d := sample("s3-video", "s3v.example.com")
	d.Mode = distribution.ModeS3
	d.DefaultBehavior = distribution.Behavior{Access: distribution.AccessPublic}
	d.DefaultBehavior.Video = &distribution.Video{HLS: true}
	_, err := svc.Create(context.Background(), d)
	require.ErrorIs(t, err, distribution.ErrInvalid)
	assert.Contains(t, err.Error(), "video streaming")

	// A behavior inherits the video streaming of the default behavior.
	d = sample("inherit", "inherit.example.com")
	d.DefaultBehavior.Video = &distribution.Video{HLS: true}
	d.Behaviors = []distribution.Behavior{{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/x/"}}}
	stored, err := svc.Create(context.Background(), d)
	require.NoError(t, err)
	require.NotNil(t, stored.DefaultBehavior.Video)
	assert.True(t, stored.DefaultBehavior.Video.HLS)
}
