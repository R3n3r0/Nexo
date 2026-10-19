package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/distribution"
)

// putImageVariants writes the variants of an image and its status.json in
// its sidecar directory, as the media worker would: every variant's
// content is its file name.
func (e *env) putImageVariants(bucket, key, etag string, formats []string, widths ...int) {
	e.t.Helper()
	dir := domain.MediaSidecarDir(key, "")
	var variants []domain.MediaImageOutput
	for _, f := range formats {
		for _, w := range append([]int{0}, widths...) {
			name := domain.MediaImageFile(w, f)
			e.putTyped(bucket, dir+name, name, domain.MediaImageContentType(f))
			pw := 1000
			if w > 0 {
				pw = w
			}
			variants = append(variants, domain.MediaImageOutput{Format: f, Width: w, PixelWidth: pw, PixelHeight: pw * 3 / 4, Size: int64(len(name))})
		}
	}
	status, err := json.Marshal(domain.MediaStatus{State: domain.MediaStateDone, Rule: "images", ETag: etag, Width: 1000, Height: 750,
		Images: variants, Updated: time.Now()})
	require.NoError(e.t, err)
	e.put(bucket, dir+domain.MediaStatusFile, status)
}

// TestDistributionImages: a behavior negotiating the images serves, at the
// URL of an image, the variant its Accept header and its ?w= and ?format=
// select, from the cache of the distribution (one entry per variant), the
// original while the variants do not derive from the current content; a
// signed URL covers its ?w= and ?format=.
func TestDistributionImages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("pics")
	original := []byte("the original photo")
	etag := e.put("pics", "photos/beach.jpg", original)
	e.putImageVariants("pics", "photos/beach.jpg", etag, []string{"avif", "webp", "jpeg"}, 320, 640)
	e.putTyped("pics", "index.html", "<h1>gallery</h1>", "text/html")

	dists := e.srv.Nodes[0].App.Distributions()
	_, err := dists.CreateKeyGroup(e.ctx, "photos", "")
	require.NoError(t, err)
	key, err := dists.GenerateKey(e.ctx, distribution.GenerateKeyInput{KeyGroup: "photos"})
	require.NoError(t, err)
	_, err = dists.Create(e.ctx, distribution.Distribution{
		ID: "gallery", Enabled: true, Aliases: []string{"img.site.test"}, Mode: distribution.ModeWebsite,
		Origin:          distribution.Origin{Bucket: "pics"},
		DefaultBehavior: distribution.Behavior{Access: distribution.AccessPublic, IndexDocument: "index.html"},
		Behaviors: []distribution.Behavior{
			{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/photos/"},
				Images: &distribution.Images{Negotiate: true}},
			{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/private/"},
				Images: &distribution.Images{Negotiate: true}, Access: distribution.AccessSigned,
				Signing: &distribution.Signing{KeyGroup: "photos"}},
		},
	})
	require.NoError(t, err)
	e.cacheSynced()
	// One node: the caches of the nodes are their own.
	node := e.nodeURLs()[0]
	get := func(path string, hdr http.Header) response {
		t.Helper()
		return hostRequest(t, http.MethodGet, node, "img.site.test", path, hdr)
	}
	accept := func(v string) http.Header { return http.Header{"Accept": {v}} }

	for _, tc := range []struct {
		path, accept, body, contentType, vary string
	}{
		{"/photos/beach.jpg", "image/avif,image/webp,*/*", "images/full.avif", "image/avif", "Accept"},
		{"/photos/beach.jpg", "image/webp,*/*", "images/full.webp", "image/webp", "Accept"},
		{"/photos/beach.jpg", "*/*", "images/full.jpg", "image/jpeg", "Accept"},
		{"/photos/beach.jpg?w=400", "image/webp,*/*", "images/640.webp", "image/webp", "Accept"},
		{"/photos/beach.jpg?w=640&format=jpeg", "image/avif", "images/640.jpg", "image/jpeg", ""},
		{"/photos/beach.jpg?format=original", "image/avif", string(original), "", ""},
		{"/photos/beach.jpg?format=png", "image/avif", string(original), "", ""},
	} {
		first := get(tc.path, accept(tc.accept))
		require.Equal(t, http.StatusOK, first.StatusCode, "%s %s: %s", tc.path, tc.accept, first.Body)
		assert.Equal(t, tc.body, string(first.Body), "%s %s", tc.path, tc.accept)
		if tc.contentType != "" {
			assert.Equal(t, tc.contentType, first.Header.Get("Content-Type"), tc.path)
		}
		assert.Equal(t, tc.vary, first.Header.Get("Vary"), tc.path)
		// The second request of the same variant is a HIT, whatever the
		// spelling of its Accept header.
		second := get(tc.path, accept(tc.accept+", text/html;q=0.1"))
		assert.Equal(t, tc.body, string(second.Body))
		assert.Equal(t, "HIT", xcache(second), "%s %s", tc.path, tc.accept)
		notMod := get(tc.path, http.Header{"Accept": {tc.accept}, "If-None-Match": {first.Header.Get("ETag")}})
		assert.Equal(t, http.StatusNotModified, notMod.StatusCode, "%s %s", tc.path, tc.accept)
	}
	rng := get("/photos/beach.jpg?w=320", http.Header{"Accept": {"image/webp"}, "Range": {"bytes=7-9"}})
	assert.Equal(t, http.StatusPartialContent, rng.StatusCode)
	assert.Equal(t, "320", string(rng.Body))
	head := hostRequest(t, http.MethodHead, node, "img.site.test", "/photos/beach.jpg", accept("image/webp"))
	assert.Equal(t, http.StatusOK, head.StatusCode)
	assert.Equal(t, "image/webp", head.Header.Get("Content-Type"))
	assert.Equal(t, "webp full", head.Header.Get(distribution.HeaderImageVariant))
	// The index of the site is served as before.
	assert.Equal(t, "<h1>gallery</h1>", string(get("/", accept("image/avif,*/*")).Body))

	// A signed URL signs its query: ?w= and ?format= included.
	signer := e.srv.App.DistributionSigner()
	signed := func(raw string) string {
		u, err := signer.SignURL(e.ctx, distribution.SignInput{URL: raw, KeyPairID: key.PublicKey.ID, PrivateKeyPEM: key.PrivateKeyPEM,
			Expires: time.Now().Add(time.Hour)})
		require.NoError(t, err)
		parsed, err := url.Parse(u)
		require.NoError(t, err)
		return parsed.RequestURI()
	}
	privEtag := e.put("pics", "private/door.jpg", []byte("private original"))
	e.putImageVariants("pics", "private/door.jpg", privEtag, []string{"webp", "jpeg"}, 320)
	ok := get(signed("http://img.site.test/private/door.jpg?w=320"), accept("image/webp"))
	require.Equal(t, http.StatusOK, ok.StatusCode, "%s", ok.Body)
	assert.Equal(t, "images/320.webp", string(ok.Body))
	tampered, err := url.Parse(signed("http://img.site.test/private/door.jpg?w=320"))
	require.NoError(t, err)
	q := tampered.Query()
	q.Set("w", "1000")
	tampered.RawQuery = q.Encode()
	assert.Equal(t, http.StatusForbidden, get(tampered.RequestURI(), accept("image/webp")).StatusCode, "?w= is signed")
	assert.Equal(t, http.StatusForbidden, get("/private/door.jpg?w=320", accept("image/webp")).StatusCode)
	// A custom policy with a wildcard lets the page choose the width.
	wild, err := signer.SignURL(e.ctx, distribution.SignInput{URL: "http://img.site.test/private/door.jpg", Resource: "http://img.site.test/private/*",
		KeyPairID: key.PublicKey.ID, PrivateKeyPEM: key.PrivateKeyPEM, Expires: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	wildURL, err := url.Parse(wild)
	require.NoError(t, err)
	wq := wildURL.Query()
	wq.Set("w", "200")
	wq.Set("format", "jpeg")
	wildURL.RawQuery = wq.Encode()
	resp := get(wildURL.RequestURI(), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "images/320.jpg", string(resp.Body))

	// A new content: the original until the worker derived the variants
	// again; the entries of the variants no longer apply.
	etag = e.put("pics", "photos/beach.jpg", []byte("a new photo"))
	resp = get("/photos/beach.jpg", accept("image/avif,*/*"))
	assert.Equal(t, "a new photo", string(resp.Body))
	assert.Empty(t, resp.Header.Get(distribution.HeaderImageVariant))
	e.putImageVariants("pics", "photos/beach.jpg", etag, []string{"webp"})
	resp = get("/photos/beach.jpg", accept("image/avif,image/webp,*/*"))
	assert.Equal(t, "images/full.webp", string(resp.Body))
	assert.NotEqual(t, "HIT", xcache(resp), "the variant rewritten by the worker was dropped")
}

// TestS3GetObjectImageVariants: GetObject on the object path answers a
// variant only when x-nexo-image-format or x-nexo-image-width ask for one
// (signed with SigV4, in the header or presigned); without them the
// original bytes, as S3.
func TestS3GetObjectImageVariants(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("pics3")
	original := []byte("the original photo")
	etag := e.put("pics3", "beach.jpg", original)
	e.putImageVariants("pics3", "beach.jpg", etag, []string{"avif", "webp", "jpeg"}, 320)
	root := e.rootCreds()

	plain := e.signedDo(http.MethodGet, "/pics3/beach.jpg", root, nil, http.Header{"Accept": {"image/avif,image/webp"}})
	require.Equal(t, http.StatusOK, plain.StatusCode)
	assert.True(t, bytes.Equal(original, plain.Body), "GetObject without the parameters is S3")
	assert.Empty(t, plain.Header.Get("Vary"))
	sdk, err := e.root.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("pics3"), Key: aws.String("beach.jpg")})
	require.NoError(t, err)
	_ = sdk.Body.Close()
	assert.Equal(t, `"`+etag+`"`, aws.ToString(sdk.ETag))

	resp := e.signedDo(http.MethodGet, "/pics3/beach.jpg?x-nexo-image-format=auto", root, nil, http.Header{"Accept": {"image/avif,image/webp"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "images/full.avif", string(resp.Body))
	assert.Equal(t, "Accept", resp.Header.Get("Vary"))
	assert.Equal(t, "avif full", resp.Header.Get("X-Nexo-Image-Variant"))
	resp = e.signedDo(http.MethodGet, "/pics3/beach.jpg?x-nexo-image-width=100&x-nexo-image-format=jpeg", root, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "images/320.jpg", string(resp.Body))
	assert.Equal(t, "image/jpeg", resp.Header.Get("Content-Type"))
	resp = e.signedDo(http.MethodHead, "/pics3/beach.jpg?x-nexo-image-format=webp", root, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "image/webp", resp.Header.Get("Content-Type"))
	resp = e.signedDo(http.MethodGet, "/pics3/beach.jpg?x-nexo-image-format=original", root, nil, nil)
	assert.Equal(t, original, resp.Body)
	requireS3Error(t, e.signedDo(http.MethodGet, "/pics3/beach.jpg?x-nexo-image-format=gif", root, nil, nil), http.StatusBadRequest, "InvalidArgument")

	// Presigned: the parameters are signed.
	presigned := e.presignRaw(http.MethodGet, e.srv.S3URL+"/pics3/beach.jpg?x-nexo-image-format=webp&X-Amz-Expires=300", root, nil, time.Now())
	resp = do(t, newRequest(t, http.MethodGet, presigned, nil))
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "images/full.webp", string(resp.Body))
	u, err := url.Parse(presigned)
	require.NoError(t, err)
	q := u.Query()
	q.Set("x-nexo-image-format", "jpeg")
	u.RawQuery = q.Encode()
	assert.Equal(t, http.StatusForbidden, do(t, newRequest(t, http.MethodGet, u.String(), nil)).StatusCode, "the parameter is signed")

	// Authorization is GetObject on the image.
	frank := e.createUser("frank", "frank-secret-1", "diagnostics")
	requireS3Error(t, e.signedDo(http.MethodGet, "/pics3/beach.jpg?x-nexo-image-format=webp", frank, nil, nil), http.StatusForbidden, "AccessDenied")

	// A new content: the original until the variants are derived again.
	e.put("pics3", "beach.jpg", []byte("new"))
	resp = e.signedDo(http.MethodGet, "/pics3/beach.jpg?x-nexo-image-format=webp", root, nil, nil)
	assert.Equal(t, "new", string(resp.Body))
}
