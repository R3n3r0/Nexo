package distribution_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/media"
)

// putImage stores an image and the variants the media worker would have
// derived from it (status.json done for its ETag, one file per variant
// whose content names it).
func (fx *engineFixture) putImage(t *testing.T, key string, alpha bool, widths []int, formats ...string) {
	t.Helper()
	fx.put(key, "original-bytes", "image/jpeg")
	dir := domain.MediaSidecarDir(key, "")
	st := domain.MediaStatus{State: domain.MediaStateDone, ETag: "etag-" + key, Width: 2000, Height: 1000, Alpha: alpha}
	for _, f := range formats {
		for _, w := range append([]int{0}, widths...) {
			px := w
			if w == 0 {
				px = 2000
			}
			st.Images = append(st.Images, domain.MediaImageOutput{Format: f, Width: w, PixelWidth: px, PixelHeight: px / 2})
			fx.put(dir+domain.MediaImageFile(w, f), "variant "+domain.MediaImageFile(w, f), "")
		}
	}
	data, err := json.Marshal(st)
	require.NoError(t, err)
	fx.put(dir+domain.MediaStatusFile, string(data), "application/json")
}

// imageSite is a website distribution negotiating the images under
// /photos/.
func imageSite(t *testing.T, mutate ...func(d *distribution.Distribution)) *distribution.Distribution {
	t.Helper()
	svc := newService(t)
	d := sample("photos", "cdn.example.com")
	d.Behaviors = []distribution.Behavior{{
		PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/photos/"},
		Images:      &distribution.Images{Negotiate: true},
	}}
	for _, m := range mutate {
		m(&d)
	}
	stored, err := svc.Create(context.Background(), d)
	require.NoError(t, err)
	return &stored
}

const (
	acceptChrome = "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8"
	acceptSafari = "image/webp,image/png,image/svg+xml,image/*;q=0.8,video/*;q=0.8,*/*;q=0.5"
	acceptOld    = "image/png,image/*;q=0.8,*/*;q=0.5"
)

// TestImagesNegotiated: the URL of an image serves the variant its Accept
// header, ?w= and ?format= select; the original otherwise.
func TestImagesNegotiated(t *testing.T) {
	d := imageSite(t)
	fx := newEngine(t, nil)
	fx.putImage(t, "photos/beach.jpg", false, []int{320, 640, 1280}, "avif", "webp", "jpeg", "png")
	fx.put("photos/notes.txt", "text", "text/plain")
	fx.put("other/beach.jpg", "original-bytes", "image/jpeg")
	fx.putImage(t, "other/sun.jpg", false, []int{640}, "webp")

	for _, tc := range []struct {
		name, url, accept   string
		body, ctype, vary   string
		variant             string
		status              int
		original, noVariant bool
	}{
		{name: "avif for chrome", url: "/photos/beach.jpg", accept: acceptChrome, body: "variant images/full.avif", ctype: "image/avif", vary: "Accept", variant: "avif full"},
		{name: "webp for safari", url: "/photos/beach.jpg", accept: acceptSafari, body: "variant images/full.webp", ctype: "image/webp", vary: "Accept", variant: "webp full"},
		{name: "jpeg for an old client", url: "/photos/beach.jpg", accept: acceptOld, body: "variant images/full.jpg", ctype: "image/jpeg", vary: "Accept", variant: "jpeg full"},
		{name: "jpeg without accept", url: "/photos/beach.jpg", body: "variant images/full.jpg", ctype: "image/jpeg", vary: "Accept", variant: "jpeg full"},
		{name: "width", url: "/photos/beach.jpg?w=500", accept: acceptChrome, body: "variant images/640.avif", ctype: "image/avif", vary: "Accept", variant: "avif 640"},
		{name: "width exact", url: "/photos/beach.jpg?w=320", accept: acceptSafari, body: "variant images/320.webp", ctype: "image/webp", vary: "Accept", variant: "webp 320"},
		{name: "width wider than all", url: "/photos/beach.jpg?w=1600", accept: acceptChrome, body: "variant images/full.avif", ctype: "image/avif", vary: "Accept", variant: "avif full"},
		{name: "format named", url: "/photos/beach.jpg?format=png&w=640", accept: acceptChrome, body: "variant images/640.png", ctype: "image/png", variant: "png 640"},
		{name: "jpg alias", url: "/photos/beach.jpg?format=jpg", accept: acceptChrome, body: "variant images/full.jpg", ctype: "image/jpeg", variant: "jpeg full"},
		{name: "original", url: "/photos/beach.jpg?format=original&w=640", accept: acceptChrome, original: true},
		{name: "original case", url: "/photos/beach.jpg?format=Original", accept: acceptChrome, original: true},
		{name: "not an image", url: "/photos/notes.txt?w=abc", accept: acceptChrome, body: "text", ctype: "text/plain", noVariant: true},
		{name: "behavior without negotiation", url: "/other/sun.jpg?format=webp", accept: acceptChrome, original: true},
		{name: "no variants", url: "/other/beach.jpg", accept: acceptChrome, original: true},
		{name: "bad width", url: "/photos/beach.jpg?w=-3", accept: acceptChrome, status: http.StatusBadRequest, noVariant: true},
		{name: "bad format", url: "/photos/beach.jpg?format=gif", accept: acceptChrome, status: http.StatusBadRequest, noVariant: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.accept != "" {
				h.Set("Accept", tc.accept)
			}
			resp, body := fx.do(t, d, webRequest{url: "https://cdn.example.com" + tc.url, header: h})
			want := tc.status
			if want == 0 {
				want = http.StatusOK
			}
			require.Equal(t, want, resp.Status, body)
			if want != http.StatusOK {
				return
			}
			if tc.original {
				assert.Equal(t, "original-bytes", body)
				assert.Equal(t, "image/jpeg", resp.Header.Get("Content-Type"))
				assert.Empty(t, resp.Header.Values("Vary"))
				assert.Empty(t, resp.Header.Get(distribution.HeaderImageVariant))
				assert.Equal(t, distribution.OutcomeObject, resp.Outcome)
				return
			}
			assert.Equal(t, tc.body, body)
			assert.Equal(t, tc.ctype, resp.Header.Get("Content-Type"))
			if tc.noVariant {
				return
			}
			assert.Equal(t, distribution.OutcomeImage, resp.Outcome)
			assert.Equal(t, tc.variant, resp.Header.Get(distribution.HeaderImageVariant))
			assert.Equal(t, "photos/beach.jpg", resp.Key)
			if tc.vary != "" {
				assert.Equal(t, []string{tc.vary}, resp.Header.Values("Vary"))
			} else {
				assert.Empty(t, resp.Header.Values("Vary"))
			}
			assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
		})
	}
}

// TestImagesAlphaAndStale: PNG rather than a flattened JPEG for a
// transparent image; the variants of another content (the worker has not
// processed the new one yet) or of an unfinished job are never served.
func TestImagesAlphaAndStale(t *testing.T) {
	d := imageSite(t)
	fx := newEngine(t, nil)
	fx.putImage(t, "photos/logo.png", true, []int{320}, "avif", "webp", "jpeg", "png")
	resp, body := fx.do(t, d, webRequest{url: "https://cdn.example.com/photos/logo.png", header: http.Header{"Accept": {acceptOld}}})
	require.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "variant images/full.png", body)

	// The image changed: its status names the previous ETag.
	fx.putImage(t, "photos/old.jpg", false, []int{320}, "webp")
	fx.state.mu.Lock()
	dir := domain.MediaSidecarDir("photos/old.jpg", "")
	fx.state.objects[dir+domain.MediaStatusFile] = strings.Replace(fx.state.objects[dir+domain.MediaStatusFile], "etag-photos/old.jpg", "etag-before", 1)
	fx.state.mu.Unlock()
	resp, body = fx.do(t, d, webRequest{url: "https://cdn.example.com/photos/old.jpg", header: http.Header{"Accept": {acceptChrome}}})
	require.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "original-bytes", body)

	// Processing: no variant yet.
	fx.putImage(t, "photos/new.jpg", false, []int{320}, "webp")
	fx.state.mu.Lock()
	dir = domain.MediaSidecarDir("photos/new.jpg", "")
	fx.state.objects[dir+domain.MediaStatusFile] = strings.Replace(fx.state.objects[dir+domain.MediaStatusFile], `"done"`, `"processing"`, 1)
	fx.state.mu.Unlock()
	resp, body = fx.do(t, d, webRequest{url: "https://cdn.example.com/photos/new.jpg?w=320"})
	require.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "original-bytes", body)

	// A variant file missing: the original.
	fx.putImage(t, "photos/gone.jpg", false, nil, "webp")
	fx.state.mu.Lock()
	delete(fx.state.objects, domain.MediaSidecarDir("photos/gone.jpg", "")+domain.MediaImageFile(0, "webp"))
	fx.state.mu.Unlock()
	resp, body = fx.do(t, d, webRequest{url: "https://cdn.example.com/photos/gone.jpg", header: http.Header{"Accept": {acceptChrome}}})
	require.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "original-bytes", body)

	// A format the worker did not produce: the original.
	resp, body = fx.do(t, d, webRequest{url: "https://cdn.example.com/photos/logo.png?format=avif"})
	require.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "variant images/full.avif", body)
	resp, body = fx.do(t, d, webRequest{url: "https://cdn.example.com/photos/new.jpg?format=png"})
	require.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "original-bytes", body)
}

// TestImagesConditionalAndRange: the validators, the conditional requests
// and the ranges are those of the variant; HEAD answers the headers of
// GET.
func TestImagesConditionalAndRange(t *testing.T) {
	d := imageSite(t)
	fx := newEngine(t, nil)
	fx.putImage(t, "photos/beach.jpg", false, []int{640}, "webp", "jpeg")
	variant := domain.MediaSidecarDir("photos/beach.jpg", "") + domain.MediaImageFile(640, "webp")
	accept := http.Header{"Accept": {acceptSafari}}

	resp, body := fx.do(t, d, webRequest{url: "https://cdn.example.com/photos/beach.jpg?w=640", header: accept})
	require.Equal(t, http.StatusOK, resp.Status)
	etag := resp.Header.Get("ETag")
	assert.Equal(t, `"etag-`+variant+`"`, etag)
	assert.NotEmpty(t, resp.Header.Get("Last-Modified"))
	assert.Equal(t, "bytes", resp.Header.Get("Accept-Ranges"))
	assert.Equal(t, "variant images/640.webp", body)

	head, _ := fx.do(t, d, webRequest{method: http.MethodHead, url: "https://cdn.example.com/photos/beach.jpg?w=640", header: accept})
	require.Equal(t, http.StatusOK, head.Status)
	for _, h := range []string{"Content-Type", "Content-Length", "ETag", "Vary", distribution.HeaderImageVariant} {
		assert.Equal(t, resp.Header.Values(h), head.Header.Values(h), h)
	}

	cond := accept.Clone()
	cond.Set("If-None-Match", etag)
	resp, _ = fx.do(t, d, webRequest{url: "https://cdn.example.com/photos/beach.jpg?w=640", header: cond})
	assert.Equal(t, http.StatusNotModified, resp.Status)
	assert.Equal(t, []string{"Accept"}, resp.Header.Values("Vary"))

	// The validator of another variant does not match.
	cond.Set("Accept", acceptOld)
	resp, body = fx.do(t, d, webRequest{url: "https://cdn.example.com/photos/beach.jpg?w=640", header: cond})
	assert.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "variant images/640.jpg", body)

	rng := accept.Clone()
	rng.Set("Range", "bytes=0-6")
	resp, body = fx.do(t, d, webRequest{url: "https://cdn.example.com/photos/beach.jpg?w=640", header: rng})
	assert.Equal(t, http.StatusPartialContent, resp.Status)
	assert.Equal(t, "variant", body)
	assert.Equal(t, "image/webp", resp.Header.Get("Content-Type"))
	assert.Equal(t, "bytes 0-6/23", resp.Header.Get("Content-Range"))
}

// TestImagesBehaviorHeadersAndCORS: the Cache-Control of the behavior, else
// of the image, applies to its variants; Vary keeps Accept next to the
// Origin of CORS.
func TestImagesBehaviorHeadersAndCORS(t *testing.T) {
	d := imageSite(t, func(d *distribution.Distribution) {
		d.Headers.CORS = &distribution.CORS{AllowedOrigins: []string{"https://app.example.com"}}
		d.Behaviors = append(d.Behaviors, distribution.Behavior{
			PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/static/"},
			Images:      &distribution.Images{Negotiate: true},
			Cache:       &distribution.Cache{CacheControl: "public, max-age=60"},
		})
	})
	fx := newEngine(t, nil)
	fx.putImage(t, "photos/beach.jpg", false, nil, "webp", "jpeg")
	fx.putImage(t, "static/beach.jpg", false, nil, "webp", "jpeg")
	h := http.Header{"Accept": {acceptSafari}, "Origin": {"https://app.example.com"}}
	resp, _ := fx.do(t, d, webRequest{url: "https://cdn.example.com/static/beach.jpg", header: h})
	require.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "public, max-age=60", resp.Header.Get("Cache-Control"))
	assert.ElementsMatch(t, []string{"Accept", "Origin"}, resp.Header.Values("Vary"))
	assert.Equal(t, "https://app.example.com", resp.Header.Get("Access-Control-Allow-Origin"))
}

// TestImagesDirectoriesAndSPA: index documents, the SPA fallback and the
// error pages are served as before.
func TestImagesDirectoriesAndSPA(t *testing.T) {
	d := imageSite(t, func(d *distribution.Distribution) {
		d.DefaultBehavior.SPAFallback = "/index.html"
		d.DefaultBehavior.Images = &distribution.Images{Negotiate: true}
	})
	fx := newEngine(t, nil)
	fx.put("index.html", "<h1>home</h1>", "text/html")
	fx.put("photos/index.html", "<h1>photos</h1>", "text/html")
	resp, body := fx.do(t, d, webRequest{url: "https://cdn.example.com/photos/", header: http.Header{"Accept": {acceptChrome}}})
	require.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "<h1>photos</h1>", body)
	resp, body = fx.do(t, d, webRequest{url: "https://cdn.example.com/missing.jpg?w=640", header: http.Header{"Accept": {acceptChrome}}})
	require.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "<h1>home</h1>", body)
	assert.Equal(t, distribution.OutcomeSPA, resp.Outcome)
}

// TestImagesValidation: the option is stored, inherited, cloned and
// refused in the s3 mode.
func TestImagesValidation(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	d := sample("img", "img.example.com")
	d.DefaultBehavior.Images = &distribution.Images{Negotiate: true}
	d.Behaviors = []distribution.Behavior{
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/raw/"}, Images: &distribution.Images{}},
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/inherit/"}},
	}
	stored, err := svc.Create(ctx, d)
	require.NoError(t, err)
	require.NotNil(t, stored.DefaultBehavior.Images)
	assert.True(t, stored.DefaultBehavior.Images.Negotiate)

	for path, want := range map[string]bool{"/raw/a.jpg": false, "/inherit/a.jpg": true, "/a.jpg": true} {
		res, err := svc.Test(ctx, distribution.TestInput{URL: "https://img.example.com" + path})
		require.NoError(t, err)
		require.NotNil(t, res.Behavior.Images, path)
		assert.Equal(t, want, res.Behavior.Images.Negotiate, path)
	}

	s3 := sample("s3img", "s3img.example.com")
	s3.Mode = distribution.ModeS3
	s3.DefaultBehavior = distribution.Behavior{Images: &distribution.Images{Negotiate: true}}
	_, err = svc.Create(ctx, s3)
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrInvalidArgument), err)
	assert.Contains(t, err.Error(), "x-nexo-image-format")
	s3.DefaultBehavior.Images.Negotiate = false
	_, err = svc.Create(ctx, s3)
	require.NoError(t, err)
}

// TestImageQuery parses the query of the URL of an image.
func TestImageQuery(t *testing.T) {
	req, ok, err := distribution.ImageQuery("w=640&format=webp&utm_source=x")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, media.ImageRequest{Width: 640, Format: "webp"}, req)
	req, ok, err = distribution.ImageQuery("")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, media.ImageFormatAuto, req.Format)
	_, ok, err = distribution.ImageQuery("format=original&w=x")
	require.NoError(t, err)
	assert.False(t, ok)
	_, _, err = distribution.ImageQuery("w=12px")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, _, err = distribution.ImageQuery("format=tiff")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}
