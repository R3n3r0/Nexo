package cache_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// imageSite is a website distribution of the "web" bucket (prefix
// "prod") serving the variants of the images of /photos/ at their URL.
func imageSite() distribution.Distribution {
	d := site("img", "web", "img.example.com")
	d.Origin.PathPrefix = "/prod"
	d.Behaviors = []distribution.Behavior{{
		PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/photos/"},
		Images:      &distribution.Images{Negotiate: true},
	}}
	return d
}

// readKey reads key for a request of path, as the website engine reads a
// variant of the image of path.
func (e *env) readKey(host, path, key string, opts ...reqOpt) result {
	e.t.Helper()
	route := e.dists.resolve(host)
	require.NotNil(e.t, route.Distribution)
	d := route.Distribution
	req := cache.Request{Distribution: d, Alias: route.Alias, Host: route.Host, Method: http.MethodGet, Path: path, Header: http.Header{}}
	var gopts object.GetObjectOptions
	for _, o := range opts {
		o(&req, &gopts)
	}
	ctx, ex := e.svc.Begin(context.Background(), req)
	var res result
	rd, handled, err := e.svc.GetObject(ctx, d.Origin.Bucket, key, gopts)
	res.handled, res.err = handled, err
	if rd != nil {
		res.info = rd.Info
		res.body, _ = io.ReadAll(rd)
		_ = rd.Close()
	}
	res.status = http.StatusOK
	if err != nil {
		res.status = http.StatusNotFound
	}
	res.header = http.Header{}
	ex.Decorate(res.status, res.header)
	ex.Finish(context.Background(), res.status, int64(len(res.body)))
	return res
}

// TestImageVariantEntries: the variants of an image served at its URL are
// cached under the key of their file (format and width, never Accept),
// with the policy and the path of the image: an invalidation of the path
// of the image drops them, a write of a variant drops it.
func TestImageVariantEntries(t *testing.T) {
	e := newEnv(t, envOptions{dists: newDists(imageSite())})
	e.setDist(cache.DistributionConfig{Distribution: "img", Behaviors: []cache.BehaviorSettings{{
		PathPattern: distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/photos/"},
		Cache:       cache.Settings{PolicySpec: cache.PolicySpec{BrowserCacheControl: ptr("public, max-age=99")}},
	}}})
	dir := domain.MediaSidecarDir("prod/photos/beach.jpg", "")
	webp, avif := dir+domain.MediaImageFile(640, "webp"), dir+domain.MediaImageFile(640, "avif")
	e.origin.put("web", webp, []byte("webp-640"), textInfo("image/webp"))
	e.origin.put("web", avif, []byte("avif-640"), textInfo("image/avif"))

	const host, path = "img.example.com", "/photos/beach.jpg"
	r := e.readKey(host, path, webp, withHeader("Accept", "image/webp"))
	require.NoError(t, r.err)
	assert.Equal(t, "MISS", r.xcache())
	assert.Equal(t, "public, max-age=99", r.header.Get("Cache-Control"), "the policy of the behavior of the image")
	// Another Accept selecting the same variant shares its entry.
	r = e.readKey(host, path, webp, withHeader("Accept", "image/webp,*/*"))
	assert.Equal(t, "HIT", r.xcache())
	assert.Equal(t, "webp-640", string(r.body))
	assert.Empty(t, r.header.Values("Vary"), "the cache adds no Vary of its own: the engine adds Accept")
	// Another variant has its own entry.
	r = e.readKey(host, path, avif, withHeader("Accept", "image/avif"))
	assert.Equal(t, "MISS", r.xcache())
	assert.Equal(t, "avif-640", string(r.body))
	assert.Equal(t, 2, cache.Entries(e.svc))

	// The worker rewrote a variant: its entry is dropped.
	e.origin.put("web", webp, []byte("webp-640-v2"), textInfo("image/webp"))
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: webp}))
	r = e.readKey(host, path, webp)
	assert.Equal(t, "MISS", r.xcache())
	assert.Equal(t, "webp-640-v2", string(r.body))
	assert.Equal(t, "HIT", e.readKey(host, path, avif).xcache())

	// An invalidation of the path of the image drops its variants.
	_, err := e.svc.Invalidate(context.Background(), cache.InvalidationRequest{Distribution: "img", Paths: []string{path}})
	require.NoError(t, err)
	assert.Equal(t, 0, cache.Entries(e.svc))
	assert.Equal(t, "MISS", e.readKey(host, path, avif).xcache())
}

// TestImageVariantPurge: the status.json the worker writes when it
// finished the variants of an image purges the URL of the image at the
// CDNs in front, for the distributions negotiating the images.
func TestImageVariantPurge(t *testing.T) {
	hs := newHookServer(t)
	plain := site("web", "web", "www.example.com")
	e := newEnv(t, envOptions{dists: newDists(imageSite(), plain)})
	for _, id := range []string{"img", "web"} {
		e.setDist(cache.DistributionConfig{Distribution: id, Webhooks: []cache.Webhook{
			{ID: "gen", Type: cache.WebhookGeneric, URL: hs.srv.URL + "/" + id},
		}})
	}
	status := domain.MediaSidecarDir("prod/photos/beach.jpg", "v1") + domain.MediaStatusFile
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: status}))
	calls := hs.wait(t, 2)
	paths := map[string][]string{}
	for _, c := range calls {
		var data cache.PurgeTemplateData
		require.NoError(t, json.Unmarshal([]byte(c.Body), &data))
		paths[c.Path] = data.Paths
	}
	assert.Equal(t, []string{"/photos/beach.jpg"}, paths["/img"])
	assert.Equal(t, []string{"/" + status}, paths["/web"], "a distribution that does not negotiate purges the file itself")

	// The variant files themselves purge nothing more for the image.
	variant := domain.MediaSidecarDir("prod/photos/beach.jpg", "v1") + domain.MediaImageFile(0, "webp")
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: variant}))
	cache.SendWebhooks(e.svc)
	for _, c := range hs.wait(t, 3)[2:] {
		assert.Equal(t, "/web", c.Path)
	}
}
