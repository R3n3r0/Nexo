package cache_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/cache"
	cachemock "github.com/r3n3r0/nexo/internal/service/cache/mock"
	"github.com/r3n3r0/nexo/internal/service/distribution"
)

// hookCall is a call received by a purge endpoint.
type hookCall struct {
	Method string
	Path   string
	Query  string
	Host   string
	Header http.Header
	Body   string
}

// hookServer is a purge endpoint (a CDN API or a proxy) recording its
// calls; it refuses them while down.
type hookServer struct {
	srv   *httptest.Server
	down  atomic.Bool
	mu    sync.Mutex
	calls []hookCall
	// refused counts the calls answered 503.
	refused atomic.Int64
}

func newHookServer(t *testing.T) *hookServer {
	t.Helper()
	h := &hookServer{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.down.Load() {
			h.refused.Add(1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.calls = append(h.calls, hookCall{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Host: r.Host,
			Header: r.Header.Clone(), Body: string(body)})
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// received returns the calls recorded.
func (h *hookServer) received() []hookCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookCall(nil), h.calls...)
}

// wait waits for n calls.
func (h *hookServer) wait(t *testing.T, n int) []hookCall {
	t.Helper()
	require.Eventually(t, func() bool { return len(h.received()) >= n }, timeout, tick, "%d webhook calls", n)
	return h.received()
}

// secrets resolves the secret references of the webhooks.
func secrets(t *testing.T, kv map[string]string) cache.SecretSource {
	m := cachemock.NewSecretSource(t)
	m.EXPECT().LookupSecret(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, name string) (string, bool) {
		v, ok := kv[name]
		return v, ok
	}).Maybe()
	return m
}

// TestWebhookPayloads is TCC-15: every invalidation of a distribution
// calls its webhooks with the payload of their type.
func TestWebhookPayloads(t *testing.T) {
	both := []string{"https://www.example.com/a.html", "https://example.com/a.html"}
	tests := []struct {
		name  string
		hook  cache.Webhook
		req   cache.InvalidationRequest
		calls int
		check func(t *testing.T, calls []hookCall)
	}{
		{
			name: "cloudflare files", hook: cache.Webhook{Type: cache.WebhookCloudflare, Zone: "z1", Token: "${secret:webhook-cf}"},
			req: cache.InvalidationRequest{Paths: []string{"/a.html"}}, calls: 1,
			check: func(t *testing.T, calls []hookCall) {
				c := calls[0]
				assert.Equal(t, http.MethodPost, c.Method)
				assert.Equal(t, "/zones/z1/purge_cache", c.Path)
				assert.Equal(t, "Bearer cf-token", c.Header.Get("Authorization"))
				var body struct {
					Files []string `json:"files"`
				}
				require.NoError(t, json.Unmarshal([]byte(c.Body), &body))
				assert.Equal(t, both, body.Files)
			},
		},
		{
			name: "cloudflare tags", hook: cache.Webhook{Type: cache.WebhookCloudflare, Zone: "z1", Token: "tok"},
			req: cache.InvalidationRequest{Tags: []string{"product-42"}}, calls: 1,
			check: func(t *testing.T, calls []hookCall) {
				assert.JSONEq(t, `{"tags":["product-42"]}`, calls[0].Body)
			},
		},
		{
			name: "cloudflare everything", hook: cache.Webhook{Type: cache.WebhookCloudflare, Zone: "z1", Token: "tok"},
			req: cache.InvalidationRequest{All: true}, calls: 1,
			check: func(t *testing.T, calls []hookCall) {
				assert.JSONEq(t, `{"purge_everything":true}`, calls[0].Body)
			},
		},
		{
			name: "cloudflare prefix", hook: cache.Webhook{Type: cache.WebhookCloudflare, Zone: "z1", Token: "tok"},
			req:   cache.InvalidationRequest{Patterns: []distribution.PathPattern{{Type: distribution.PatternGlob, Pattern: "/assets/**"}}},
			calls: 1,
			check: func(t *testing.T, calls []hookCall) {
				assert.JSONEq(t, `{"prefixes":["www.example.com/assets/","example.com/assets/"]}`, calls[0].Body)
			},
		},
		{
			name: "fastly soft", hook: cache.Webhook{Type: cache.WebhookFastly, Zone: "svc", Token: "fk"},
			req: cache.InvalidationRequest{Paths: []string{"/a.html"}, Soft: true}, calls: 2,
			check: func(t *testing.T, calls []hookCall) {
				var paths []string
				for _, c := range calls {
					assert.Equal(t, http.MethodPost, c.Method)
					assert.Equal(t, "fk", c.Header.Get("Fastly-Key"))
					assert.Equal(t, "1", c.Header.Get("Fastly-Soft-Purge"))
					paths = append(paths, c.Path)
				}
				assert.ElementsMatch(t, []string{"/purge/www.example.com/a.html", "/purge/example.com/a.html"}, paths)
			},
		},
		{
			name: "fastly surrogate keys", hook: cache.Webhook{Type: cache.WebhookFastly, Zone: "svc", Token: "fk"},
			req: cache.InvalidationRequest{Tags: []string{"a", "b"}}, calls: 1,
			check: func(t *testing.T, calls []hookCall) {
				assert.Equal(t, "/service/svc/purge", calls[0].Path)
				assert.Equal(t, "a b", calls[0].Header.Get("Surrogate-Key"))
			},
		},
		{
			name: "akamai", hook: cache.Webhook{Type: cache.WebhookAkamai, Token: "at", ClientToken: "ct", ClientSecret: "${secret:webhook-ak}"},
			req: cache.InvalidationRequest{Paths: []string{"/a.html"}}, calls: 1,
			check: func(t *testing.T, calls []hookCall) {
				c := calls[0]
				assert.Equal(t, "/ccu/v3/delete/url/production", c.Path)
				assert.True(t, strings.HasPrefix(c.Header.Get("Authorization"), "EG1-HMAC-SHA256 client_token=ct;access_token=at;"))
				assert.Contains(t, c.Header.Get("Authorization"), ";signature=")
				assert.JSONEq(t, `{"objects":["https://www.example.com/a.html","https://example.com/a.html"]}`, c.Body)
			},
		},
		{
			name: "nginx", hook: cache.Webhook{Type: cache.WebhookNginx},
			req: cache.InvalidationRequest{Paths: []string{"/a b.html"}}, calls: 2,
			check: func(t *testing.T, calls []hookCall) {
				var hosts []string
				for _, c := range calls {
					assert.Equal(t, "PURGE", c.Method)
					assert.Equal(t, "/a%20b.html", c.Path)
					hosts = append(hosts, c.Host)
				}
				assert.ElementsMatch(t, []string{"www.example.com", "example.com"}, hosts)
			},
		},
		{
			name: "varnish", hook: cache.Webhook{Type: cache.WebhookVarnish},
			req:   cache.InvalidationRequest{Paths: []string{"/a.html"}, Patterns: []distribution.PathPattern{{Type: distribution.PatternGlob, Pattern: "/img/*.png"}}},
			calls: 1,
			check: func(t *testing.T, calls []hookCall) {
				c := calls[0]
				assert.Equal(t, "BAN", c.Method)
				assert.Equal(t, `^(www\.example\.com|example\.com)$`, c.Header.Get("X-Ban-Host"))
				assert.Equal(t, `^/a\.html$|^/img/[^/]*\.png$`, c.Header.Get("X-Ban-Url"))
			},
		},
		{
			name: "varnish tags", hook: cache.Webhook{Type: cache.WebhookVarnish},
			req: cache.InvalidationRequest{Tags: []string{"t1"}}, calls: 1,
			check: func(t *testing.T, calls []hookCall) {
				assert.Equal(t, `(^|[ ,])(t1)([ ,]|$)`, calls[0].Header.Get("X-Ban-Tags"))
			},
		},
		{
			name: "generic template",
			hook: cache.Webhook{Type: cache.WebhookGeneric, Method: http.MethodPut, Body: `{"urls":{{json .URLs}},"dist":"{{.Distribution}}"}`,
				Token: "${secret:webhook-gk}", Headers: map[string]string{"X-Api-Key": "plain", "X-Count": "{{len .Paths}}"}},
			req: cache.InvalidationRequest{Paths: []string{"/a.html"}}, calls: 1,
			check: func(t *testing.T, calls []hookCall) {
				c := calls[0]
				assert.Equal(t, http.MethodPut, c.Method)
				assert.Equal(t, "/purge/web", c.Path)
				assert.Equal(t, "Bearer generic-key", c.Header.Get("Authorization"))
				assert.Equal(t, "plain", c.Header.Get("X-Api-Key"))
				assert.Equal(t, "1", c.Header.Get("X-Count"))
				assert.JSONEq(t, `{"urls":["https://www.example.com/a.html","https://example.com/a.html"],"dist":"web"}`, c.Body)
			},
		},
		{
			name: "generic default body", hook: cache.Webhook{Type: cache.WebhookGeneric},
			req: cache.InvalidationRequest{Tags: []string{"x"}, Soft: true}, calls: 1,
			check: func(t *testing.T, calls []hookCall) {
				var data cache.PurgeTemplateData
				require.NoError(t, json.Unmarshal([]byte(calls[0].Body), &data))
				assert.Equal(t, []string{"x"}, data.Tags)
				assert.True(t, data.Soft)
				assert.NotEmpty(t, data.ID, "the invalidation id")
				assert.Equal(t, []string{"www.example.com", "example.com"}, data.Hosts)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := newHookServer(t)
			e := newEnv(t, envOptions{secret: secrets(t, map[string]string{"webhook-cf": "cf-token", "webhook-ak": "ak-secret", "webhook-gk": "generic-key"})})
			w := tt.hook
			w.ID = "edge"
			switch w.Type {
			case cache.WebhookGeneric:
				w.URL = hs.srv.URL + "/purge/{{.Distribution}}"
			default:
				w.URL = hs.srv.URL
			}
			e.setDist(cache.DistributionConfig{Distribution: "web", Webhooks: []cache.Webhook{w}})
			req := tt.req
			req.Distribution = "web"
			_, err := e.svc.Invalidate(context.Background(), req)
			require.NoError(t, err)
			calls := hs.wait(t, tt.calls)
			cache.SendWebhooks(e.svc)
			require.Len(t, hs.received(), tt.calls)
			tt.check(t, calls)
			st, err := e.svc.NodeStats(context.Background(), cache.StatsQuery{})
			require.NoError(t, err)
			assert.Zero(t, st.WebhookQueue)
		})
	}
}

// TestWebhookDomains calls a webhook for the domains it is bound to.
func TestWebhookDomains(t *testing.T) {
	www, apex := newHookServer(t), newHookServer(t)
	e := newEnv(t, envOptions{})
	e.setDist(cache.DistributionConfig{
		Distribution: "web",
		Domains:      []cache.DomainSettings{{Host: "example.com", Cache: cache.Settings{PurgeWebhooks: []string{"apex"}}}},
		Webhooks: []cache.Webhook{
			{ID: "www", Type: cache.WebhookNginx, URL: www.srv.URL, Domains: []string{"www.example.com"}},
			{ID: "apex", Type: cache.WebhookNginx, URL: apex.srv.URL},
		},
	})
	_, err := e.svc.Invalidate(context.Background(), cache.InvalidationRequest{Distribution: "web", Paths: []string{"/a.html"}})
	require.NoError(t, err)
	assert.Equal(t, "www.example.com", www.wait(t, 1)[0].Host)
	assert.Equal(t, "example.com", apex.wait(t, 1)[0].Host)
	// An invalidation of one domain calls its webhook only.
	_, err = e.svc.Invalidate(context.Background(), cache.InvalidationRequest{Distribution: "web", Domain: "example.com", Paths: []string{"/b.html"}})
	require.NoError(t, err)
	apex.wait(t, 2)
	cache.SendWebhooks(e.svc)
	assert.Len(t, www.received(), 1)
}

// TestWebhookAutomatic purges the CDN on the writes too (the index
// document of a directory purges the directory URL).
func TestWebhookAutomatic(t *testing.T) {
	hs := newHookServer(t)
	e := newEnv(t, envOptions{})
	e.setDist(cache.DistributionConfig{Distribution: "web", Webhooks: []cache.Webhook{
		{ID: "gen", Type: cache.WebhookGeneric, URL: hs.srv.URL + "/hook"},
	}})
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "docs/index.html", Tags: []string{"docs"}}))
	var data cache.PurgeTemplateData
	require.NoError(t, json.Unmarshal([]byte(hs.wait(t, 1)[0].Body), &data))
	assert.True(t, data.Automatic)
	assert.Equal(t, []string{"/docs/index.html", "/docs/"}, data.Paths)
	assert.Equal(t, []string{"docs"}, data.Tags)
	// A bucket the distribution does not serve calls nothing.
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "other", Key: "x"}))
	cache.SendWebhooks(e.svc)
	assert.Len(t, hs.received(), 1)
}

// TestWebhookRetry is TCC-15: a purge to an endpoint offline is kept in a
// persistent queue and retried (with a growing delay) until delivered,
// across a restart of the node.
func TestWebhookRetry(t *testing.T) {
	hs := newHookServer(t)
	hs.down.Store(true)
	queue := t.TempDir()
	docs := &memDocs{docs: map[string][]byte{}}
	eo := envOptions{docs: docs, noStop: true, opts: []cache.Option{cache.WithWebhookQueue(queue), cache.WithWebhookRetry(time.Second, 8*time.Second)}}
	e := newEnv(t, eo)
	e.setDist(cache.DistributionConfig{Distribution: "web", Webhooks: []cache.Webhook{{ID: "v", Type: cache.WebhookVarnish, URL: hs.srv.URL}}})
	_, err := e.svc.Invalidate(context.Background(), cache.InvalidationRequest{Distribution: "web", Paths: []string{"/a.html"}})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return hs.refused.Load() >= 1 }, timeout, tick)
	// Not due before its delay.
	cache.SendWebhooks(e.svc)
	assert.Equal(t, int64(1), hs.refused.Load())
	e.clk.Advance(time.Second)
	cache.SendWebhooks(e.svc)
	assert.Equal(t, int64(2), hs.refused.Load())
	// The delay doubles.
	e.clk.Advance(time.Second)
	cache.SendWebhooks(e.svc)
	assert.Equal(t, int64(2), hs.refused.Load())
	st, err := e.svc.NodeStats(context.Background(), cache.StatsQuery{})
	require.NoError(t, err)
	assert.Equal(t, 1, st.WebhookQueue)
	require.NoError(t, e.svc.Stop(context.Background()))

	// The node restarts: the queue is reloaded and delivered once the
	// endpoint is back.
	hs.down.Store(false)
	eo.origin, eo.dists, eo.clk = e.origin, e.dists, e.clk
	e2 := newEnv(t, eo)
	t.Cleanup(func() { _ = e2.svc.Stop(context.Background()) })
	e2.clk.Advance(time.Minute)
	cache.SendWebhooks(e2.svc)
	calls := hs.wait(t, 1)
	assert.Equal(t, "BAN", calls[0].Method)
	assert.Equal(t, `^/a\.html$`, calls[0].Header.Get("X-Ban-Url"))
	st, err = e2.svc.NodeStats(context.Background(), cache.StatsQuery{})
	require.NoError(t, err)
	assert.Zero(t, st.WebhookQueue)
	cache.SendWebhooks(e2.svc)
	assert.Len(t, hs.received(), 1, "delivered once")
}

// TestWebhookRemoved drops the purges of a webhook removed meanwhile.
func TestWebhookRemoved(t *testing.T) {
	hs := newHookServer(t)
	hs.down.Store(true)
	e := newEnv(t, envOptions{})
	e.setDist(cache.DistributionConfig{Distribution: "web", Webhooks: []cache.Webhook{{ID: "v", Type: cache.WebhookVarnish, URL: hs.srv.URL}}})
	_, err := e.svc.Invalidate(context.Background(), cache.InvalidationRequest{Distribution: "web", All: true})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return hs.refused.Load() >= 1 }, timeout, tick)
	require.NoError(t, e.svc.DeleteWebhook(context.Background(), "web", "v"))
	e.clk.Advance(time.Minute)
	cache.SendWebhooks(e.svc)
	st, err := e.svc.NodeStats(context.Background(), cache.StatsQuery{})
	require.NoError(t, err)
	assert.Zero(t, st.WebhookQueue)
}

// TestWebhookManagement covers the webhooks API: validation, secrets
// never returned, the test call.
func TestWebhookManagement(t *testing.T) {
	hs := newHookServer(t)
	e := newEnv(t, envOptions{secret: secrets(t, map[string]string{"webhook-cf": "cf-token"})})
	ctx := context.Background()
	for _, bad := range []cache.Webhook{
		{ID: "Bad ID", Type: cache.WebhookNginx, URL: hs.srv.URL},
		{ID: "x", Type: "squid", URL: hs.srv.URL},
		{ID: "x", Type: cache.WebhookCloudflare, Token: "t"},
		{ID: "x", Type: cache.WebhookNginx, URL: "ftp://proxy"},
		{ID: "x", Type: cache.WebhookAkamai, URL: hs.srv.URL, Token: "a", ClientToken: "b", ClientSecret: "c", Network: "moon"},
		{ID: "x", Type: cache.WebhookGeneric, URL: hs.srv.URL + "/{{.Nope}}"},
	} {
		_, err := e.svc.AddWebhook(ctx, "web", bad)
		require.ErrorIs(t, err, cache.ErrInvalid, "%+v", bad)
	}
	added, err := e.svc.AddWebhook(ctx, "web", cache.Webhook{ID: "cf", Type: cache.WebhookCloudflare, URL: hs.srv.URL, Zone: "z", Token: "plain-secret"})
	require.NoError(t, err)
	assert.Empty(t, added.Token, "a secret is never returned")
	_, err = e.svc.AddWebhook(ctx, "web", cache.Webhook{ID: "cf", Type: cache.WebhookNginx, URL: hs.srv.URL})
	require.ErrorIs(t, err, cache.ErrAlreadyExists)
	ref, err := e.svc.AddWebhook(ctx, "web", cache.Webhook{ID: "cf2", Type: cache.WebhookCloudflare, URL: hs.srv.URL, Zone: "z", Token: "${secret:webhook-cf}"})
	require.NoError(t, err)
	assert.Equal(t, "${secret:webhook-cf}", ref.Token, "a reference is shown")
	list, err := e.svc.ListWebhooks(ctx, "web")
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, w := range list {
		assert.NotEqual(t, "plain-secret", w.Token)
	}
	// The stored secret is used by the calls.
	res, err := e.svc.TestWebhook(ctx, "web", "cf")
	require.NoError(t, err)
	assert.True(t, res.OK, res.Error)
	assert.Equal(t, http.StatusOK, res.Status)
	calls := hs.received()
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodGet, calls[0].Method)
	assert.Equal(t, "/zones/z", calls[0].Path)
	assert.Equal(t, "Bearer plain-secret", calls[0].Header.Get("Authorization"))
	res, err = e.svc.TestWebhook(ctx, "web", "cf2")
	require.NoError(t, err)
	assert.True(t, res.OK)
	assert.Equal(t, "Bearer cf-token", hs.received()[1].Header.Get("Authorization"))
	// A refusal is reported, not returned as an error.
	hs.down.Store(true)
	res, err = e.svc.TestWebhook(ctx, "web", "cf")
	require.NoError(t, err)
	assert.False(t, res.OK)
	assert.Equal(t, http.StatusServiceUnavailable, res.Status)
	_, err = e.svc.TestWebhook(ctx, "web", "nope")
	require.ErrorIs(t, err, cache.ErrWebhookNotFound)
	require.NoError(t, e.svc.DeleteWebhook(ctx, "web", "cf"))
	require.ErrorIs(t, e.svc.DeleteWebhook(ctx, "web", "cf"), cache.ErrWebhookNotFound)
	list, err = e.svc.ListWebhooks(ctx, "web")
	require.NoError(t, err)
	ids := make([]string, 0, len(list))
	for _, w := range list {
		ids = append(ids, w.ID)
	}
	sort.Strings(ids)
	assert.Equal(t, []string{"cf2"}, ids)
}

// TestWebhookPatterns converts the patterns of the invalidations.
func TestWebhookPatterns(t *testing.T) {
	e := newEnv(t, envOptions{})
	tests := []struct {
		pattern distribution.PathPattern
		prefix  string
		whole   bool
		regex   string
	}{
		{distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/assets/"}, "/assets/", true, `^/assets/`},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/assets/**"}, "/assets/", true, `^/assets/.*$`},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/img/*.{png,jpg}"}, "/img/", false, `^/img/[^/]*\.(?:png|jpg)$`},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/exact.html"}, "/exact.html", false, `^/exact\.html$`},
		{distribution.PathPattern{Type: distribution.PatternRegex, Pattern: `^/v[0-9]+/`}, "", false, `^/v[0-9]+/`},
	}
	for _, tt := range tests {
		t.Run(tt.pattern.Pattern, func(t *testing.T) {
			prefix, whole := cache.LiteralPrefix(tt.pattern)
			assert.Equal(t, tt.prefix, prefix)
			assert.Equal(t, tt.whole, whole)
			assert.Equal(t, tt.regex, cache.PatternRegex(e.svc, tt.pattern))
		})
	}
}

// TestWebhookSecurity: whoever manages the cache cannot read the secrets
// of the node nor probe the internal network with the purge webhooks.
func TestWebhookSecurity(t *testing.T) {
	ctx := context.Background()
	nodeSecrets := map[string]string{"proxy-a": "TOP-SECRET-ORIGIN", "webhook-cf": "cf-token"}

	t.Run("no secret outside of the webhook namespace", func(t *testing.T) {
		hs := newHookServer(t)
		e := newEnv(t, envOptions{secret: secrets(t, nodeSecrets)})
		for _, bad := range []cache.Webhook{
			{ID: "leak", Type: cache.WebhookGeneric, URL: hs.srv.URL, Headers: map[string]string{"X-Leak": "${secret:proxy-a}"}},
			{ID: "leak", Type: cache.WebhookGeneric, URL: hs.srv.URL, Headers: map[string]string{"X-Leak": "x ${secret:webhook-cf}"}},
			{ID: "leak", Type: cache.WebhookGeneric, URL: hs.srv.URL, Token: "${secret:proxy-a}"},
			{ID: "leak", Type: cache.WebhookCloudflare, URL: hs.srv.URL, Zone: "z", Token: "${secret:proxy-a}"},
			{ID: "leak", Type: cache.WebhookAkamai, URL: hs.srv.URL, Token: "a", ClientToken: "b", ClientSecret: "${secret:proxy-a}"},
		} {
			_, err := e.svc.AddWebhook(ctx, "web", bad)
			require.ErrorIs(t, err, cache.ErrInvalid, "%+v", bad)
		}
		// A configuration saved before the namespace existed sends
		// nothing.
		cfg := cache.DistributionConfig{Distribution: "web", Webhooks: []cache.Webhook{
			{ID: "old", Type: cache.WebhookGeneric, URL: hs.srv.URL, Token: "${secret:proxy-a}"}}}
		data, err := json.Marshal(cfg)
		require.NoError(t, err)
		e.docs.mu.Lock()
		e.docs.docs["cache/distributions/web.json"] = data
		e.docs.mu.Unlock()
		require.NoError(t, cache.Reload(e.svc))
		res, err := e.svc.TestWebhook(ctx, "web", "old")
		require.NoError(t, err)
		require.True(t, res.OK, res.Error)
		calls := hs.received()
		require.Len(t, calls, 1)
		assert.Empty(t, calls[0].Header.Get("Authorization"))
		for _, vs := range calls[0].Header {
			for _, v := range vs {
				assert.NotContains(t, v, "TOP-SECRET")
			}
		}
	})

	t.Run("a stored secret follows its endpoint only", func(t *testing.T) {
		hs, attacker := newHookServer(t), newHookServer(t)
		e := newEnv(t, envOptions{secret: secrets(t, nodeSecrets)})
		_, err := e.svc.AddWebhook(ctx, "web", cache.Webhook{ID: "cf", Type: cache.WebhookCloudflare, URL: hs.srv.URL, Zone: "z", Token: "CF-STORED-TOKEN"})
		require.NoError(t, err)
		cur, err := e.svc.GetDistribution(ctx, "web")
		require.NoError(t, err)
		require.Empty(t, cur.Webhooks[0].Token, "masked")
		// Re-pointed to another endpoint without its token: refused.
		moved := cur
		moved.Webhooks = []cache.Webhook{cur.Webhooks[0]}
		moved.Webhooks[0].URL = attacker.srv.URL
		_, err = e.svc.UpdateDistribution(ctx, moved, cur.ETag)
		require.ErrorIs(t, err, cache.ErrInvalid)
		for _, change := range []func(w *cache.Webhook){
			func(w *cache.Webhook) { w.Zone = "other" },
			func(w *cache.Webhook) { w.Type = cache.WebhookFastly },
		} {
			c := cur
			c.Webhooks = []cache.Webhook{cur.Webhooks[0]}
			change(&c.Webhooks[0])
			_, err = e.svc.UpdateDistribution(ctx, c, cur.ETag)
			require.ErrorIs(t, err, cache.ErrInvalid)
		}
		// Same endpoint: the stored token is kept.
		same := cur
		same.Webhooks = []cache.Webhook{cur.Webhooks[0]}
		same.Webhooks[0].Domains = []string{"www.example.com"}
		_, err = e.svc.UpdateDistribution(ctx, same, cur.ETag)
		require.NoError(t, err)
		res, err := e.svc.TestWebhook(ctx, "web", "cf")
		require.NoError(t, err)
		require.True(t, res.OK, res.Error)
		assert.Equal(t, "Bearer CF-STORED-TOKEN", hs.received()[0].Header.Get("Authorization"))
		assert.Empty(t, attacker.received())
	})

	t.Run("internal endpoints refused by default", func(t *testing.T) {
		hs := newHookServer(t)
		e := newEnv(t, envOptions{strictEgress: true})
		_, port, err := net.SplitHostPort(hs.srv.Listener.Addr().String())
		require.NoError(t, err)
		for _, u := range []string{hs.srv.URL, "http://localhost:" + port, "http://[::1]:" + port, "http://10.1.2.3/", "http://169.254.169.254/latest"} {
			_, err := e.svc.AddWebhook(ctx, "web", cache.Webhook{ID: "int", Type: cache.WebhookNginx, URL: u})
			require.ErrorIs(t, err, cache.ErrInvalid, u)
		}
		// The addresses are checked when they are dialed, whatever name
		// led to them (a rebinding included).
		for _, addr := range []string{"127.0.0.1:80", "[::1]:443", "10.0.0.1:80", "[fd00::1]:80", "169.254.169.254:80", "100.64.1.1:80", "[::ffff:127.0.0.1]:80"} {
			require.ErrorIs(t, cache.EgressCheck(e.svc, addr), cache.ErrInvalid, addr)
		}
		require.NoError(t, cache.EgressCheck(e.svc, "93.184.215.14:443"))
		// The name of this machine resolves to an internal address: a
		// webhook naming it is saved, and refused when it is dialed.
		host, err := os.Hostname()
		require.NoError(t, err)
		addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(addrs) == 0 || cache.EgressCheck(e.svc, netip.AddrPortFrom(addrs[0], 80).String()) == nil {
			t.Skipf("the name of this machine (%s) does not resolve to an internal address", host)
		}
		_, err = e.svc.AddWebhook(ctx, "web", cache.Webhook{ID: "name", Type: cache.WebhookNginx, URL: "http://" + net.JoinHostPort(host, port)})
		require.NoError(t, err)
		res, err := e.svc.TestWebhook(ctx, "web", "name")
		require.NoError(t, err)
		assert.False(t, res.OK)
		assert.Contains(t, res.Error, "internal address")
		assert.Empty(t, hs.received(), "never reached")
	})

	t.Run("no readback of the answers", func(t *testing.T) {
		internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "INTERNAL-ONLY-DATA", http.StatusForbidden)
		}))
		t.Cleanup(internal.Close)
		e := newEnv(t, envOptions{})
		_, err := e.svc.AddWebhook(ctx, "web", cache.Webhook{ID: "probe", Type: cache.WebhookGeneric, URL: internal.URL})
		require.NoError(t, err)
		res, err := e.svc.TestWebhook(ctx, "web", "probe")
		require.NoError(t, err)
		assert.False(t, res.OK)
		assert.Equal(t, http.StatusForbidden, res.Status)
		assert.NotContains(t, res.Error, "INTERNAL-ONLY-DATA")
	})

	t.Run("never a node of the cluster", func(t *testing.T) {
		hs := newHookServer(t)
		// The endpoint is this node, though the loopback is allowed.
		e := newEnv(t, envOptions{node: hs.srv.Listener.Addr().String()})
		_, err := e.svc.AddWebhook(ctx, "web", cache.Webhook{ID: "self", Type: cache.WebhookNginx, URL: hs.srv.URL})
		require.NoError(t, err)
		res, err := e.svc.TestWebhook(ctx, "web", "self")
		require.NoError(t, err)
		assert.False(t, res.OK)
		assert.Contains(t, res.Error, "node of the cluster")
		assert.Empty(t, hs.received())
	})
}
