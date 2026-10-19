package dist_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	distmock "github.com/r3n3r0/nexo/internal/service/distribution/mock"
	"github.com/r3n3r0/nexo/internal/service/object"
	objmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	polmock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/dist"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// imageOrigin is a bucket holding an image, its variants and status.json
// as the media worker writes them; it records the keys read.
type imageOrigin struct {
	mu      sync.Mutex
	objects map[string]string
	read    []string
}

func (o *imageOrigin) info(key string) (domain.ObjectInfo, string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.read = append(o.read, key)
	body, ok := o.objects[key]
	return domain.ObjectInfo{Bucket: "pics", Key: key, Size: int64(len(body)), ETag: "etag-" + key,
		ModTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ContentType: "image/jpeg"}, body, ok
}

func (o *imageOrigin) layer(t *testing.T) *objmock.Service {
	t.Helper()
	m := objmock.NewService(t)
	m.EXPECT().GetObjectInfo(tmock.Anything, "pics", tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, _, key string, opts object.GetObjectOptions) (domain.ObjectInfo, error) {
			info, _, ok := o.info(key)
			if !ok {
				return domain.ObjectInfo{}, domain.ErrObjectNotFound
			}
			if strings.Trim(opts.Conditions.IfNoneMatch, `"`) == info.ETag {
				return info, domain.ErrNotModified
			}
			return info, nil
		}).Maybe()
	m.EXPECT().GetObject(tmock.Anything, "pics", tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, _, key string, opts object.GetObjectOptions) (*object.Reader, error) {
			info, body, ok := o.info(key)
			if !ok {
				return nil, domain.ErrObjectNotFound
			}
			if strings.Trim(opts.Conditions.IfNoneMatch, `"`) == info.ETag {
				return nil, domain.ErrNotModified
			}
			return &object.Reader{ReadCloser: io.NopCloser(strings.NewReader(body)), Info: info, Length: info.Size}, nil
		}).Maybe()
	return m
}

// TestWebsiteImageVariants: the website routes serve, at the URL of an
// image of a behavior negotiating the images, the variant its Accept, ?w=
// and ?format= select (Vary: Accept), the original otherwise; a signed
// behavior verifies the query of the variant.
func TestWebsiteImageVariants(t *testing.T) {
	origin := &imageOrigin{objects: map[string]string{"photos/sea.jpg": "original", "signed/sea.jpg": "signed original"}}
	for _, key := range []string{"photos/sea.jpg", "signed/sea.jpg"} {
		dir := domain.MediaSidecarDir(key, "")
		st := domain.MediaStatus{State: domain.MediaStateDone, ETag: "etag-" + key}
		for _, f := range []string{"avif", "webp", "jpeg"} {
			for _, w := range []int{0, 640} {
				px := w
				if w == 0 {
					px = 1600
				}
				st.Images = append(st.Images, domain.MediaImageOutput{Format: f, Width: w, PixelWidth: px})
				origin.objects[dir+domain.MediaImageFile(w, f)] = f + "-" + domain.MediaImageFile(w, f)
			}
		}
		data, err := json.Marshal(st)
		require.NoError(t, err)
		origin.objects[dir+domain.MediaStatusFile] = string(data)
	}
	pol := polmock.NewService(t)
	pol.EXPECT().Evaluate(tmock.Anything, tmock.Anything).Return(policy.Decision{}, nil).Maybe()
	var verified []string
	sigs := distmock.NewSignedRequestVerifier(t)
	sigs.EXPECT().VerifySignedRequest(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, in distribution.SignedRequest) error {
		verified = append(verified, in.RawQuery)
		if strings.Contains(in.RawQuery, "Signature=good") {
			return nil
		}
		return distribution.ErrBadSignature
	}).Maybe()
	engine, err := distribution.NewWebsite(distribution.WebsiteDeps{Objects: origin.layer(t), Policy: pol, Signatures: sigs,
		Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	d := &distribution.Distribution{ID: "gallery", Enabled: true, Mode: distribution.ModeWebsite, Origin: distribution.Origin{Bucket: "pics"},
		DefaultBehavior: distribution.Behavior{Access: distribution.AccessPublic, AllowedMethods: []string{"GET", "HEAD"}},
		Behaviors: []distribution.Behavior{
			{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/photos/"},
				Images: &distribution.Images{Negotiate: true}},
			{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/signed/"},
				Images: &distribution.Images{Negotiate: true}, Access: distribution.AccessSigned, Signing: &distribution.Signing{KeyGroup: "k"}},
		}}
	resolve := distmock.NewResolver(t)
	resolve.EXPECT().Resolve(tmock.Anything, tmock.Anything).Return(distribution.Route{Kind: distribution.RouteDistribution,
		Host: "img.example.com", Alias: "img.example.com", Distribution: d, Bucket: "pics"}).Maybe()
	routes, err := dist.NewWebsiteRoutes(dist.WebsiteDeps{Website: engine, Resolver: resolve})
	require.NoError(t, err)
	router, err := dist.NewHostRouter(dist.HostRouterDeps{Resolver: resolve, Website: routes, Admission: engine, Clock: clock.NewReal()})
	require.NoError(t, err)
	h := router.Wrap(http.NotFoundHandler())
	serve := func(method, target string, hdr http.Header) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://img.example.com"+target, nil)
		r = r.WithContext(middleware.WithClient(r.Context(), middleware.Client{IP: "198.51.100.7", Host: "img.example.com"}))
		for k, v := range hdr {
			r.Header[k] = v
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	for _, tc := range []struct{ target, accept, body, ctype, vary string }{
		{"/photos/sea.jpg", "image/avif,image/webp,*/*", "avif-images/full.avif", "image/avif", "Accept"},
		{"/photos/sea.jpg", "image/webp,*/*", "webp-images/full.webp", "image/webp", "Accept"},
		{"/photos/sea.jpg", "", "jpeg-images/full.jpg", "image/jpeg", "Accept"},
		{"/photos/sea.jpg?w=320", "image/webp", "webp-images/640.webp", "image/webp", "Accept"},
		{"/photos/sea.jpg?w=320&format=jpeg", "image/avif", "jpeg-images/640.jpg", "image/jpeg", ""},
		{"/photos/sea.jpg?format=original", "image/avif", "original", "image/jpeg", ""},
	} {
		w := serve(http.MethodGet, tc.target, http.Header{"Accept": {tc.accept}})
		require.Equal(t, http.StatusOK, w.Code, "%s %s: %s", tc.target, tc.accept, w.Body)
		assert.Equal(t, tc.body, w.Body.String(), "%s %s", tc.target, tc.accept)
		assert.Equal(t, tc.ctype, w.Header().Get("Content-Type"), tc.target)
		assert.Equal(t, tc.vary, w.Header().Get("Vary"), tc.target)
		head := serve(http.MethodHead, tc.target, http.Header{"Accept": {tc.accept}})
		assert.Equal(t, http.StatusOK, head.Code)
		assert.Empty(t, head.Body.String())
		assert.Equal(t, w.Header().Get("ETag"), head.Header().Get("ETag"), tc.target)
		assert.Equal(t, w.Header().Get("Content-Length"), head.Header().Get("Content-Length"), tc.target)
		notMod := serve(http.MethodGet, tc.target, http.Header{"Accept": {tc.accept}, "If-None-Match": {w.Header().Get("ETag")}})
		assert.Equal(t, http.StatusNotModified, notMod.Code, tc.target)
		assert.Empty(t, notMod.Body.String())
	}
	assert.Equal(t, http.StatusBadRequest, serve(http.MethodGet, "/photos/sea.jpg?w=wide", nil).Code)

	// Signed: the query of the variant is verified with the signature.
	w := serve(http.MethodGet, "/signed/sea.jpg?w=640&Expires=1&Signature=good&Key-Pair-Id=k", http.Header{"Accept": {"image/webp"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "webp-images/640.webp", w.Body.String())
	assert.Contains(t, verified, "w=640&Expires=1&Signature=good&Key-Pair-Id=k")
	w = serve(http.MethodGet, "/signed/sea.jpg?w=640", http.Header{"Accept": {"image/webp"}})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.NotContains(t, w.Body.String(), "webp")
}
