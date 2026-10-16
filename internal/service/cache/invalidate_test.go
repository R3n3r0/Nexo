package cache_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
	cachemock "github.com/r3n3r0/nexo/internal/service/cache/mock"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/event"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
)

// fill caches the paths on the hosts.
func (e *env) fill(hosts []string, paths ...string) {
	e.t.Helper()
	for _, h := range hosts {
		for _, p := range paths {
			r := e.get(h, p)
			require.NoError(e.t, r.err, "%s%s", h, p)
		}
	}
}

// cached reports whether a path of a host is fresh in the cache (the
// inspector: nothing is filled).
func (e *env) cached(host, path string) bool {
	e.t.Helper()
	res, err := e.svc.Inspect(context.Background(), cache.InspectInput{URL: "http://" + host + path})
	require.NoError(e.t, err)
	require.NotEmpty(e.t, res.Nodes)
	entry := res.Nodes[0].Entry
	return entry.Present && !entry.Stale
}

// TestManualInvalidation is TCC-06: by path, glob, regex, tag, domain,
// everything; soft purges mark the entries stale.
func TestManualInvalidation(t *testing.T) {
	hosts := []string{"www.example.com", "example.com"}
	paths := []string{"/index.html", "/assets/app.js", "/assets/img/logo.png", "/docs/a.pdf", "/tagged.html"}
	setup := func(t *testing.T) *env {
		e := newEnv(t, envOptions{})
		for _, p := range paths {
			e.origin.put("web", p[1:], []byte("content of "+p))
		}
		e.origin.put("web", "tagged.html", []byte("tagged"), func(i *domain.ObjectInfo) {
			i.UserMetadata = map[string]string{"cache-tag": "product-42, home"}
		})
		e.fill(hosts, paths...)
		return e
	}
	tests := []struct {
		name string
		req  cache.InvalidationRequest
		gone map[string][]string // host → paths dropped
	}{
		{name: "path", req: cache.InvalidationRequest{Paths: []string{"/index.html"}},
			gone: map[string][]string{"www.example.com": {"/index.html"}, "example.com": {"/index.html"}}},
		{name: "path on a domain", req: cache.InvalidationRequest{Domain: "www.example.com", Paths: []string{"/index.html"}},
			gone: map[string][]string{"www.example.com": {"/index.html"}}},
		{name: "glob", req: cache.InvalidationRequest{Patterns: []distribution.PathPattern{{Type: distribution.PatternGlob, Pattern: "/assets/**"}}},
			gone: map[string][]string{"www.example.com": {"/assets/app.js", "/assets/img/logo.png"}, "example.com": {"/assets/app.js", "/assets/img/logo.png"}}},
		{name: "regex", req: cache.InvalidationRequest{Domain: "example.com", Patterns: []distribution.PathPattern{{Type: distribution.PatternRegex, Pattern: `\.(png|pdf)$`}}},
			gone: map[string][]string{"example.com": {"/assets/img/logo.png", "/docs/a.pdf"}}},
		{name: "tag", req: cache.InvalidationRequest{Tags: []string{"product-42"}},
			gone: map[string][]string{"www.example.com": {"/tagged.html"}, "example.com": {"/tagged.html"}}},
		{name: "domain", req: cache.InvalidationRequest{Domain: "example.com"},
			gone: map[string][]string{"example.com": paths}},
		{name: "everything", req: cache.InvalidationRequest{All: true},
			gone: map[string][]string{"www.example.com": paths, "example.com": paths}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			tc.req.Distribution = "web"
			inv, err := e.svc.Invalidate(context.Background(), tc.req)
			require.NoError(t, err)
			assert.Equal(t, cache.InvalidationCompleted, inv.State)
			require.Len(t, inv.Nodes, 1)
			assert.Equal(t, cache.NodeCompleted, inv.Nodes[0].State)
			for _, h := range hosts {
				for _, p := range paths {
					dropped := false
					for _, g := range tc.gone[h] {
						dropped = dropped || g == p
					}
					assert.Equal(t, !dropped, e.cached(h, p), "%s%s", h, p)
				}
			}
			got, err := e.svc.GetInvalidation(context.Background(), "web", inv.ID)
			require.NoError(t, err)
			assert.Equal(t, inv.ID, got.ID)
			list, err := e.svc.ListInvalidations(context.Background(), "web")
			require.NoError(t, err)
			require.Len(t, list, 1)
		})
	}
	t.Run("soft", func(t *testing.T) {
		e := setup(t)
		e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{StaleWhileRevalidate: dur(time.Minute)}}})
		e.fill(hosts, paths...)
		_, err := e.svc.Invalidate(context.Background(), cache.InvalidationRequest{Distribution: "web", Paths: []string{"/index.html"}, Soft: true})
		require.NoError(t, err)
		r := e.get("www.example.com", "/index.html")
		assert.Equal(t, "STALE", r.xcache(), "served stale while revalidated")
		assert.Equal(t, "HIT", e.get("www.example.com", "/assets/app.js").xcache())
	})
	t.Run("directory path drops its index", func(t *testing.T) {
		e := setup(t)
		_, err := e.svc.Invalidate(context.Background(), cache.InvalidationRequest{Distribution: "web", Paths: []string{"/"}})
		require.NoError(t, err)
		assert.False(t, e.cached("www.example.com", "/index.html"))
	})
	t.Run("invalid", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		for _, req := range []cache.InvalidationRequest{
			{Distribution: "web"},
			{Distribution: "web", Paths: []string{"no-slash"}},
			{Distribution: "web", Domain: "evil.example.org", All: true},
			{Distribution: "web", Patterns: []distribution.PathPattern{{Type: distribution.PatternRegex, Pattern: "("}}},
			{Paths: []string{"/a"}},
		} {
			_, err := e.svc.Invalidate(context.Background(), req)
			require.ErrorIs(t, err, domain.ErrInvalidArgument, "%+v", req)
		}
		_, err := e.svc.Invalidate(context.Background(), cache.InvalidationRequest{Distribution: "nope", All: true})
		require.ErrorIs(t, err, domain.ErrNotFound)
		_, err = e.svc.GetInvalidation(context.Background(), "web", "inv-none")
		require.ErrorIs(t, err, cache.ErrInvalidationNotFound)
	})
}

// TestAutomaticInvalidation is TCC-05 at the level of the cache: an
// object written is dropped on every domain serving it, its variants and
// blocks too; a bucket created or removed drops the bucket.
func TestAutomaticInvalidation(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.origin.put("web", "index.html", []byte("v1"), textInfo("text/html"))
	e.fill([]string{"www.example.com", "example.com"}, "/index.html")
	e.get("www.example.com", "/index.html", withHeader("Accept-Encoding", "gzip"))
	e.origin.put("web", "index.html", []byte("v2"), textInfo("text/html"))
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "index.html", Event: string(event.ObjectCreatedPut)}))
	for _, h := range []string{"www.example.com", "example.com"} {
		r := e.get(h, "/index.html")
		assert.Equal(t, "v2", string(r.body), h)
		assert.Equal(t, "MISS", r.xcache(), h)
	}
	e.fill([]string{"www.example.com"}, "/index.html")
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", BucketWide: true}))
	assert.Equal(t, 0, cache.Entries(e.svc))
	// A distribution may keep its entries on writes.
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{AutoInvalidate: ptr(false)}})
	e.fill([]string{"www.example.com"}, "/index.html")
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "index.html"}))
	assert.Equal(t, "HIT", e.get("www.example.com", "/index.html").xcache())
}

// TestInvalidationDuringFill checks that a fill that read the origin
// before a write is not stored after it (the race of TCC-05).
func TestInvalidationDuringFill(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.origin.put("web", "race.txt", []byte("old"))
	gate := make(chan struct{})
	e.origin.mu.Lock()
	e.origin.gate = gate
	e.origin.mu.Unlock()
	var (
		wg  sync.WaitGroup
		res result
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		res = e.get("www.example.com", "/race.txt")
	}()
	require.Eventually(t, func() bool { return e.origin.gets.Load() == 1 }, time.Second, time.Millisecond)
	// The read is in flight: the object changes and the write is
	// acknowledged (invalidated) before the read completes.
	e.origin.mu.Lock()
	e.origin.gate = nil
	e.origin.mu.Unlock()
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "race.txt"}))
	e.origin.put("web", "race.txt", []byte("new"))
	close(gate)
	wg.Wait()
	require.NoError(t, res.err)
	r := e.get("www.example.com", "/race.txt")
	assert.Equal(t, "new", string(r.body), "the old content read before the write is never cached after it")
}

// TestEventsHook checks the event hook: the writes invalidate, the reads
// do not, and the events still reach the notification service.
func TestEventsHook(t *testing.T) {
	var (
		mu        sync.Mutex
		changes   []cache.ObjectChange
		published int
	)
	inv := cachemock.NewInvalidator(t)
	inv.EXPECT().ObjectChanged(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, c cache.ObjectChange) error {
		mu.Lock()
		defer mu.Unlock()
		changes = append(changes, c)
		return nil
	})
	events := eventmock.NewService(t)
	events.EXPECT().Publish(tmock.Anything, tmock.Anything).RunAndReturn(func(context.Context, event.Event) error {
		published++
		return nil
	})
	hook, err := cache.NewEvents(cache.EventsDeps{Events: events, Invalidator: inv})
	require.NoError(t, err)
	ctx := context.Background()
	evs := []event.Event{
		event.NewEvent(event.ObjectCreatedPut, "us-east-1", t0),
		event.NewEvent(event.ObjectAccessedGet, "us-east-1", t0),
		event.NewEvent(event.ObjectRemovedDeleteMarkerCreated, "us-east-1", t0),
		event.NewEvent(event.ObjectTaggingPut, "us-east-1", t0),
		event.NewEvent(event.BucketRemoved, "us-east-1", t0),
		// A retention or legal hold change (docs/11 §11.5 "metadata
		// change").
		event.NewEvent(event.ObjectMetadataChanged, "us-east-1", t0),
	}
	for i := range evs {
		evs[i].S3.Bucket.Name = "web"
		if evs[i].EventName != event.BucketRemoved {
			evs[i].S3.Object.Key = "a.txt"
		}
	}
	evs[0].S3.Object.UserMetadata = map[string]string{"cache-tag": "t1 t2"}
	for _, ev := range evs {
		require.NoError(t, hook.Publish(ctx, ev))
	}
	require.Len(t, changes, 5)
	assert.Equal(t, []string{"t1", "t2"}, changes[0].Tags)
	assert.True(t, changes[3].BucketWide)
	assert.Equal(t, cache.ObjectChange{Bucket: "web", Key: "a.txt", Event: string(event.ObjectMetadataChanged)}, changes[4])
	assert.Equal(t, 6, published, "every event reaches the notification service (which drops the internal ones)")
	_, err = cache.NewEvents(cache.EventsDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestInvalidationHistoryBound keeps the newest records.
func TestInvalidationHistoryBound(t *testing.T) {
	e := newEnv(t, envOptions{opts: []cache.Option{cache.WithHistory(3)}})
	var last cache.Invalidation
	for range 5 {
		var err error
		last, err = e.svc.Invalidate(context.Background(), cache.InvalidationRequest{Distribution: "web", All: true})
		require.NoError(t, err)
		e.clk.Advance(time.Millisecond)
	}
	list, err := e.svc.ListInvalidations(context.Background(), "web")
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, last.ID, list[0].ID, "the newest first")
	_, err = e.svc.Invalidate(context.Background(), cache.InvalidationRequest{All: true})
	require.NoError(t, err, "a global purge")
	global, err := e.svc.ListInvalidations(context.Background(), "")
	require.NoError(t, err)
	assert.Len(t, global, 1)
}
