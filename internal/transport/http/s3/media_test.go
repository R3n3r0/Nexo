package s3_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
	"github.com/r3n3r0/nexo/internal/service/media"
	mediamock "github.com/r3n3r0/nexo/internal/service/media/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

type mediaFixture struct {
	media  *mediamock.Service
	policy *policymock.Service
	signer *authmock.Signer
	creds  *authmock.CredentialLookup
	router chi.Router
	now    time.Time
}

func newMediaFixture(t *testing.T) *mediaFixture {
	t.Helper()
	f := &mediaFixture{
		media: mediamock.NewService(t), policy: policymock.NewService(t), signer: authmock.NewSigner(t), creds: authmock.NewCredentialLookup(t),
		router: chi.NewRouter(), now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
	}
	player := fstest.MapFS{
		"player.html":             {Data: []byte(`<script src="./assets/player-abc123.js"></script>`)},
		"assets/player-abc123.js": {Data: []byte("play()")},
		"index.html":              {Data: []byte("console")},
	}
	routes, err := s3.NewMediaRoutes(s3.MediaDeps{Media: f.media, Policy: f.policy, Signer: f.signer, Creds: f.creds, Clock: clock.NewFake(f.now),
		Player: player},
		s3.WithRegion("eu-south-1"))
	require.NoError(t, err)
	routes.Register(f.router)
	return f
}

// serve sends a request as the identity id (nil: no auth middleware).
func (f *mediaFixture) serve(target string, id *domain.Identity) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "s3.example.com"
	if id != nil {
		req = req.WithContext(middleware.WithIdentity(req.Context(), id))
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func TestNewMediaRoutesRequiresDeps(t *testing.T) {
	_, err := s3.NewMediaRoutes(s3.MediaDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestMediaRoutesAnonymous(t *testing.T) {
	f := newMediaFixture(t)
	// No nexo:GetMediaStream: s3:GetObject allows the stream.
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Action == policy.ActionGetMediaStream
	})).Return(policy.Decision{Reason: "no"}, nil)
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Action == policy.ActionGetObject && a.Bucket == "films" && a.Identity.IsAnonymous
	})).Return(policy.Decision{Allowed: true}, nil)
	f.media.EXPECT().MasterPlaylist(mock.Anything, media.Source{Bucket: "films", Key: "a b.mp4"}, mock.Anything).
		RunAndReturn(func(_ context.Context, _ media.Source, l media.Links) ([]byte, error) {
			return []byte(l.Media("e1") + "\n" + l.Segment("e1", 2)), nil
		})
	rec := f.serve(s3.MediaPrefix+"films/master.m3u8?key=a+b.mp4", domain.Anonymous())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/vnd.apple.mpegurl", rec.Header().Get("Content-Type"))
	assert.Equal(t, "media.m3u8?key=a+b.mp4&rev=e1\nsegment.m4s?key=a+b.mp4&n=2&rev=e1", rec.Body.String(), "unsigned URIs")

	f.media.EXPECT().Segment(mock.Anything, media.Source{Bucket: "films", Key: "a b.mp4", Revision: "e1"}, 2).Return([]byte("seg"), nil)
	rec = f.serve(s3.MediaPrefix+"films/segment.m4s?key=a+b.mp4&rev=e1&n=2", domain.Anonymous())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "video/iso.segment", rec.Header().Get("Content-Type"))
	assert.Equal(t, "seg", rec.Body.String())

	for _, target := range []string{"master.m3u8", "segment.m4s?key=a.mp4&n=1", "segment.m4s?key=a.mp4&rev=e1&n=x"} {
		assert.Equal(t, http.StatusBadRequest, f.serve(s3.MediaPrefix+"films/"+target, domain.Anonymous()).Code, target)
	}
}

func TestMediaRoutesDenied(t *testing.T) {
	f := newMediaFixture(t)
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Action == policy.ActionGetObjectVersion || a.Action == policy.ActionGetMediaStream
	})).Return(policy.Decision{Reason: "no"}, nil)
	rec := f.serve(s3.MediaPrefix+"films/master.m3u8?key=a.mp4&versionId=v1", domain.Anonymous())
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestMediaRoutesStreamOnly: nexo:GetMediaStream streams a video whose
// download s3:GetObject denies.
func TestMediaRoutesStreamOnly(t *testing.T) {
	f := newMediaFixture(t)
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Action == policy.ActionGetMediaStream && a.Bucket == "films" && a.Object == "a.mp4"
	})).Return(policy.Decision{Allowed: true}, nil)
	f.media.EXPECT().Init(mock.Anything, media.Source{Bucket: "films", Key: "a.mp4", Revision: "e1"}).Return([]byte("init"), nil)
	rec := f.serve(s3.MediaPrefix+"films/init.mp4?key=a.mp4&rev=e1", domain.Anonymous())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "init", rec.Body.String())
}

// TestMediaRoutesPresigned: the URIs of a presigned playlist are presigned
// with the credentials of its signer until it expires.
func TestMediaRoutesPresigned(t *testing.T) {
	f := newMediaFixture(t)
	share := &domain.Identity{AccessKey: "share-abc", Parent: "alice", AuthType: domain.AuthTypeQuery}
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Allowed: true}, nil)
	creds := domain.Credentials{AccessKey: "share-abc", SecretKey: "secret"}
	f.creds.EXPECT().LookupCredentials(mock.Anything, "share-abc").Return(creds, *share, nil)
	var signed []auth.PresignInput
	f.signer.EXPECT().Presign(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in auth.PresignInput) (*url.URL, error) {
		signed = append(signed, in)
		u := *in.URL
		q := u.Query()
		q.Set("X-Amz-Signature", "sig")
		u.RawQuery = q.Encode()
		return &u, nil
	})
	f.media.EXPECT().MediaPlaylist(mock.Anything, media.Source{Bucket: "films", Key: "a.mp4", Revision: "e1"}, mock.Anything).
		RunAndReturn(func(_ context.Context, _ media.Source, l media.Links) ([]byte, error) {
			return []byte(l.Init("e1") + "\n" + l.Segment("e1", 0)), nil
		})
	// Signed ten minutes ago for an hour.
	query := "key=a.mp4&rev=e1&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20261018T115000Z&X-Amz-Expires=3600&X-Amz-Signature=x"
	rec := f.serve(s3.MediaPrefix+"films/media.m3u8?"+query, share)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "init.mp4?X-Amz-Signature=sig&key=a.mp4&rev=e1\nsegment.m4s?X-Amz-Signature=sig&key=a.mp4&n=0&rev=e1", rec.Body.String())
	require.Len(t, signed, 2)
	assert.Equal(t, "s3.example.com", signed[0].URL.Host)
	assert.Equal(t, s3.MediaPrefix+"films/init.mp4", signed[0].URL.Path)
	assert.Equal(t, 50*time.Minute, signed[0].Expires)
	assert.Equal(t, creds, signed[0].Creds)
	assert.Equal(t, "eu-south-1", signed[0].Region)

	// Expired: nothing to sign.
	query = "key=a.mp4&rev=e1&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20261018T100000Z&X-Amz-Expires=3600&X-Amz-Signature=x"
	rec = f.serve(s3.MediaPrefix+"films/media.m3u8?"+query, share)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestMediaPlayer: the player page and its assets, public and confined;
// nothing else of the console build.
func TestMediaPlayer(t *testing.T) {
	f := newMediaFixture(t)
	rec := f.serve(s3.PlayerPath, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "media-src 'self' blob:")
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	assert.Contains(t, rec.Body.String(), "./assets/player-abc123.js")

	rec = f.serve(s3.PlayerPath+"assets/player-abc123.js", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "play()", rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "javascript")
	assert.Contains(t, rec.Header().Get("Cache-Control"), "immutable")

	for _, target := range []string{"assets/missing.js", "assets/..%2Findex.html", "assets/index.html", "index.html"} {
		assert.Equal(t, http.StatusNotFound, f.serve(s3.PlayerPath+target, nil).Code, target)
	}
}

// imageContent is the content of a variant served from memory.
type imageContent struct{ *bytes.Reader }

func (imageContent) Close() error { return nil }

func testImage(format string, width int, data string) *media.Image {
	return &media.Image{
		MediaImageOutput: domain.MediaImageOutput{Format: format, Width: width, PixelWidth: 640, PixelHeight: 480, Size: int64(len(data))},
		ContentType:      domain.MediaImageContentType(format), ETag: `"e1-` + format + `"`,
		Modified: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), Content: imageContent{bytes.NewReader([]byte(data))},
	}
}

// serveReq sends a request as the identity id.
func (f *mediaFixture) serveReq(req *http.Request, id *domain.Identity) *httptest.ResponseRecorder {
	req.Host = "s3.example.com"
	req = req.WithContext(middleware.WithIdentity(req.Context(), id))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func TestMediaImageRoute(t *testing.T) {
	f := newMediaFixture(t)
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Action == policy.ActionGetObject && a.Bucket == "photos" && a.Object == "2026/a b.jpg"
	})).Return(policy.Decision{Allowed: true}, nil)

	// Format auto: the Accept header goes to the service, the response
	// varies on it.
	f.media.EXPECT().Image(mock.Anything, media.Source{Bucket: "photos", Key: "2026/a b.jpg"}, media.ImageRequest{Width: 300, Format: "auto", Accept: "image/avif,image/webp,*/*"}).
		Return(testImage("avif", 320, "avif bytes"), nil).Once()
	req := httptest.NewRequest(http.MethodGet, s3.MediaPrefix+"photos/image/2026/a%20b.jpg?w=300", nil)
	req.Header.Set("Accept", "image/avif,image/webp,*/*")
	rec := f.serveReq(req, domain.Anonymous())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "image/avif", rec.Header().Get("Content-Type"))
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
	assert.Equal(t, `"e1-avif"`, rec.Header().Get("ETag"))
	assert.Equal(t, "private, no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "Mon, 19 Oct 2026 12:00:00 GMT", rec.Header().Get("Last-Modified"))
	assert.Equal(t, "avif bytes", rec.Body.String())

	// An explicit format, HEAD, conditional requests and ranges.
	f.media.EXPECT().Image(mock.Anything, media.Source{Bucket: "photos", Key: "2026/a b.jpg"}, media.ImageRequest{Format: "png"}).
		RunAndReturn(func(context.Context, media.Source, media.ImageRequest) (*media.Image, error) {
			return testImage("png", 0, "0123456789"), nil
		}).Times(4)
	rec = f.serveReq(httptest.NewRequest(http.MethodHead, s3.MediaPrefix+"photos/image/2026/a%20b.jpg?format=png", nil), domain.Anonymous())
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "10", rec.Header().Get("Content-Length"))
	assert.Empty(t, rec.Header().Get("Vary"))
	assert.Empty(t, rec.Body.String())
	req = httptest.NewRequest(http.MethodGet, s3.MediaPrefix+"photos/image/2026/a%20b.jpg?format=png", nil)
	req.Header.Set("If-None-Match", `"e1-png"`)
	assert.Equal(t, http.StatusNotModified, f.serveReq(req, domain.Anonymous()).Code)
	req = httptest.NewRequest(http.MethodGet, s3.MediaPrefix+"photos/image/2026/a%20b.jpg?format=png", nil)
	req.Header.Set("If-Modified-Since", "Mon, 19 Oct 2026 12:00:00 GMT")
	assert.Equal(t, http.StatusNotModified, f.serveReq(req, domain.Anonymous()).Code)
	req = httptest.NewRequest(http.MethodGet, s3.MediaPrefix+"photos/image/2026/a%20b.jpg?format=png", nil)
	req.Header.Set("Range", "bytes=-3")
	rec = f.serveReq(req, domain.Anonymous())
	require.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, "789", rec.Body.String())

	// Not derived from the current content: 404.
	f.media.EXPECT().Image(mock.Anything, media.Source{Bucket: "photos", Key: "2026/a b.jpg"}, media.ImageRequest{Format: "webp"}).
		Return(nil, fmt.Errorf("%w: no media output", domain.ErrNotFound)).Once()
	rec = f.serve(s3.MediaPrefix+"photos/image/2026/a%20b.jpg?format=webp", domain.Anonymous())
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Malformed requests never reach the service.
	for _, target := range []string{"image/2026/a%20b.jpg?w=x", "image/2026/a%20b.jpg?format=gif", "image/2026/a%20b.jpg?w=-5"} {
		assert.Equal(t, http.StatusBadRequest, f.serve(s3.MediaPrefix+"photos/"+target, domain.Anonymous()).Code, target)
	}
}

func TestMediaImageRouteDenied(t *testing.T) {
	f := newMediaFixture(t)
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Action == policy.ActionGetObjectVersion && a.Object == "a.jpg"
	})).Return(policy.Decision{Reason: "no"}, nil)
	rec := f.serve(s3.MediaPrefix+"photos/image/a.jpg?versionId=v1", domain.Anonymous())
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestMediaStatusImages: status.json lists the variants of an image with
// their URI, presigned like the request.
func TestMediaStatusImages(t *testing.T) {
	f := newMediaFixture(t)
	share := &domain.Identity{AccessKey: "share-abc", Parent: "alice", AuthType: domain.AuthTypeQuery}
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Allowed: true}, nil)
	creds := domain.Credentials{AccessKey: "share-abc", SecretKey: "secret"}
	f.creds.EXPECT().LookupCredentials(mock.Anything, "share-abc").Return(creds, *share, nil)
	var signed []auth.PresignInput
	f.signer.EXPECT().Presign(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in auth.PresignInput) (*url.URL, error) {
		signed = append(signed, in)
		u := *in.URL
		q := u.Query()
		q.Set("X-Amz-Signature", "sig")
		u.RawQuery = q.Encode()
		return &u, nil
	})
	st := &media.OutputStatus{Current: true, MediaStatus: domain.MediaStatus{State: domain.MediaStateDone, Width: 1000, Height: 750,
		Images: []domain.MediaImageOutput{{Format: "webp", Width: 640, PixelWidth: 640, PixelHeight: 480, Size: 20}, {Format: "jpeg", PixelWidth: 1000, PixelHeight: 750, Size: 50}}}}
	f.media.EXPECT().Status(mock.Anything, media.Source{Bucket: "photos", Key: "2026/a b.jpg", VersionID: "v1"}).Return(st, nil)
	query := "key=2026%2Fa+b.jpg&versionId=v1&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20261018T115000Z&X-Amz-Expires=3600&X-Amz-Signature=x"
	rec := f.serve(s3.MediaPrefix+"photos/status.json?"+query, share)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"state":"done","current":true,"width":1000,"height":750,"images":[
		{"format":"webp","width":640,"pixelWidth":640,"pixelHeight":480,"size":20,"contentType":"image/webp",
		 "url":"image/2026/a%20b.jpg?X-Amz-Signature=sig&format=webp&versionId=v1&w=640"},
		{"format":"jpeg","width":0,"pixelWidth":1000,"pixelHeight":750,"size":50,"contentType":"image/jpeg",
		 "url":"image/2026/a%20b.jpg?X-Amz-Signature=sig&format=jpeg&versionId=v1"}]}`, rec.Body.String())
	require.Len(t, signed, 2)
	assert.Equal(t, s3.MediaPrefix+"photos/image/2026/a b.jpg", signed[0].URL.Path, "the decoded path is signed")
	assert.Equal(t, 50*time.Minute, signed[0].Expires)
}

// TestMediaRoutesKey: the key of a protected video goes, never cached, to
// a signed request allowed to stream it from an allowed origin; not to an
// anonymous one, nor a share link unless the protection lets them play.
//
// covers: S-OBJ-17
func TestMediaRoutesKey(t *testing.T) {
	f := newMediaFixture(t)
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Allowed: true}, nil)
	src := media.Source{Bucket: "films", Key: "a.mp4", Revision: "e1"}
	prot := &domain.MediaProtection{Enabled: true, AllowedOrigins: []string{"https://learn.example.com"}}
	f.media.EXPECT().Protection(mock.Anything, src).Return(prot, nil)
	f.media.EXPECT().Key(mock.Anything, src, 3).Return([]byte("0123456789abcdef"), nil)
	alice := &domain.Identity{AccessKey: "alice", AuthType: domain.AuthTypeHeader}
	target := s3.MediaPrefix + "films/key?key=a.mp4&rev=e1&i=3"

	rec := f.serve(target, alice)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "0123456789abcdef", rec.Body.String())

	assert.Equal(t, http.StatusForbidden, f.serve(target, domain.Anonymous()).Code, "anonymous")
	assert.Equal(t, http.StatusForbidden, f.serve(target+"&X-Amz-Signature=x&X-Amz-Credential=c", alice).Code, "share link")

	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Origin", "https://evil.example")
	req = req.WithContext(middleware.WithIdentity(req.Context(), alice))
	rec = httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, "origin")

	for _, bad := range []string{"films/key?key=a.mp4&i=3", "films/key?key=a.mp4&rev=e1&i=-1", "films/key?key=a.mp4&rev=e1"} {
		assert.Equal(t, http.StatusBadRequest, f.serve(s3.MediaPrefix+bad, alice).Code, bad)
	}

	clearSrc := media.Source{Bucket: "films", Key: "b.mp4", Revision: "e1"}
	f.media.EXPECT().Protection(mock.Anything, clearSrc).Return(nil, nil)
	assert.Equal(t, http.StatusNotFound, f.serve(s3.MediaPrefix+"films/key?key=b.mp4&rev=e1&i=0", alice).Code, "not protected")
}
