package distribution_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	distmock "github.com/r3n3r0/nexo/internal/service/distribution/mock"
	"github.com/r3n3r0/nexo/internal/service/object"
	objmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	polmock "github.com/r3n3r0/nexo/internal/service/policy/mock"
)

// bucketState is the content of the origin bucket of the engine tests,
// shared by the object layer mock.
type bucketState struct {
	mu      sync.Mutex
	objects map[string]string
	types   map[string]string
	// requested records every key the engine read.
	requested []string
}

func (b *bucketState) info(key string) (domain.ObjectInfo, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requested = append(b.requested, key)
	body, ok := b.objects[key]
	if !ok {
		return domain.ObjectInfo{}, false
	}
	ct := b.types[key]
	return domain.ObjectInfo{Bucket: "web", Key: key, Size: int64(len(body)), ETag: "etag-" + key, ContentType: ct,
		ModTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, true
}

// objectLayer returns an object layer mock serving state.
func objectLayer(t *testing.T, state *bucketState) *objmock.Service {
	t.Helper()
	m := objmock.NewService(t)
	m.EXPECT().GetObjectInfo(tmock.Anything, "web", tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, _, key string, opts object.GetObjectOptions) (domain.ObjectInfo, error) {
			info, ok := state.info(key)
			if !ok {
				return domain.ObjectInfo{}, domain.ErrObjectNotFound
			}
			if opts.Conditions.IfNoneMatch != "" && strings.Trim(opts.Conditions.IfNoneMatch, `"`) == info.ETag {
				return info, domain.ErrNotModified
			}
			return info, nil
		}).Maybe()
	m.EXPECT().GetObject(tmock.Anything, "web", tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, _, key string, opts object.GetObjectOptions) (*object.Reader, error) {
			info, ok := state.info(key)
			if !ok {
				return nil, domain.ErrObjectNotFound
			}
			if opts.Conditions.IfNoneMatch != "" && strings.Trim(opts.Conditions.IfNoneMatch, `"`) == info.ETag {
				return nil, domain.ErrNotModified
			}
			offset, length := int64(0), info.Size
			if opts.Range != nil {
				var err error
				if offset, length, err = opts.Range.Resolve(info.Size); err != nil {
					return nil, err
				}
			}
			state.mu.Lock()
			body := state.objects[key][offset : offset+length]
			state.mu.Unlock()
			return &object.Reader{ReadCloser: io.NopCloser(strings.NewReader(body)), Info: info, Offset: offset, Length: length}, nil
		}).Maybe()
	m.EXPECT().PutObject(tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, in object.PutObjectInput) (domain.ObjectInfo, error) {
			data, err := io.ReadAll(in.Reader)
			if err != nil {
				return domain.ObjectInfo{}, err
			}
			state.mu.Lock()
			state.objects[in.Key] = string(data)
			state.mu.Unlock()
			return domain.ObjectInfo{Key: in.Key, ETag: "new"}, nil
		}).Maybe()
	return m
}

// policyEngine returns a policy mock: the keys of denied are denied
// explicitly, the identity "alice" is allowed, the rest is an implicit
// deny. It records the distribution condition of the evaluations.
func policyEngine(t *testing.T, denied map[string]bool, seen *[]string) *polmock.Service {
	t.Helper()
	var mu sync.Mutex
	m := polmock.NewService(t)
	m.EXPECT().Evaluate(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, a policy.Args) (policy.Decision, error) {
		mu.Lock()
		*seen = append(*seen, strings.Join(a.Conditions[policy.KeyDistribution], ","))
		mu.Unlock()
		if denied[a.Object] {
			return policy.Decision{Source: policy.SourceBucket, MatchedStatement: &domain.Statement{Effect: domain.Deny}}, nil
		}
		if a.Identity != nil && a.Identity.AccessKey == "alice" {
			return policy.Decision{Allowed: true, Source: policy.SourceIAM}, nil
		}
		return policy.Decision{}, nil
	}).Maybe()
	return m
}

// engineFixture is a website engine over a mocked origin.
type engineFixture struct {
	engine  distribution.Website
	state   *bucketState
	blocked bool
	// blockErr fails the lookups of the block public access.
	blockErr error
	seen     []string
}

func newEngine(t *testing.T, denied map[string]bool) *engineFixture {
	t.Helper()
	fx := &engineFixture{}
	return fx.start(t, policyEngine(t, denied, &fx.seen))
}

// start builds the engine of fx with the policy engine pol.
func (fx *engineFixture) start(t *testing.T, pol policy.Service) *engineFixture {
	t.Helper()
	fx.state = &bucketState{objects: map[string]string{}, types: map[string]string{}}
	sigs := distmock.NewSignedRequestVerifier(t)
	sigs.EXPECT().VerifySignedRequest(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, in distribution.SignedRequest) error {
		if strings.Contains(in.RawQuery, "Signature=good") || in.Cookies[distribution.CookieSignature] == "good" {
			return nil
		}
		return distribution.ErrBadSignature
	}).Maybe()
	pab := distmock.NewPublicAccessBlock(t)
	pab.EXPECT().PublicAccessBlocked(tmock.Anything, "web").RunAndReturn(func(context.Context, string) (bool, error) {
		return fx.blocked, fx.blockErr
	}).Maybe()
	engine, err := distribution.NewWebsite(distribution.WebsiteDeps{
		Objects: objectLayer(t, fx.state), Policy: pol, Signatures: sigs, PublicAccess: pab,
		Logger: slog.New(slog.DiscardHandler),
	}, distribution.WithServerHeader("Nexo"))
	require.NoError(t, err)
	fx.engine = engine
	return fx
}

func (fx *engineFixture) put(key, body, contentType string) {
	fx.state.mu.Lock()
	defer fx.state.mu.Unlock()
	fx.state.objects[key] = body
	fx.state.types[key] = contentType
}

// webRequest builds the WebRequest of a URL.
type webRequest struct {
	method   string
	url      string
	header   http.Header
	clientIP string
	identity *domain.Identity
	body     string
}

func (fx *engineFixture) do(t *testing.T, d *distribution.Distribution, r webRequest) (*distribution.WebResponse, string) {
	t.Helper()
	u, err := url.Parse(r.url)
	require.NoError(t, err)
	if r.method == "" {
		r.method = http.MethodGet
	}
	if r.header == nil {
		r.header = http.Header{}
	}
	if r.clientIP == "" {
		r.clientIP = "198.51.100.7"
	}
	req := distribution.WebRequest{
		Route: distribution.Route{Kind: distribution.RouteDistribution, Distribution: d, Bucket: d.Origin.Bucket},
		Host:  u.Host, Method: r.method, Path: u.Path, EscapedPath: u.EscapedPath(), RawQuery: u.RawQuery,
		Header: r.header, ClientIP: r.clientIP, Secure: u.Scheme == "https", Identity: r.identity, ContentLength: -1,
	}
	if r.body != "" {
		req.Body, req.ContentLength = strings.NewReader(r.body), int64(len(r.body))
	}
	resp, err := fx.engine.Serve(context.Background(), req)
	require.NoError(t, err)
	var body string
	if resp.Body != nil {
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		body = string(data)
	}
	return resp, body
}

// site is the distribution of docs/10 §10.2, on the "web" bucket with the
// "prod" prefix.
func site(t *testing.T) *distribution.Distribution {
	t.Helper()
	svc := newService(t)
	_, err := svc.CreateKeyGroup(context.Background(), "rilasci", "")
	require.NoError(t, err)
	d := sample("sito-web", "www.example.com", "example.com", "*.preview.example.com")
	d.Origin.PathPrefix = "/prod"
	d.DefaultBehavior.SPAFallback = "/index.html"
	d.DefaultBehavior.Cache = &distribution.Cache{CacheControl: "public, max-age=300"}
	d.Behaviors = []distribution.Behavior{
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/assets/**"},
			Cache: &distribution.Cache{CacheControl: "public, max-age=31536000, immutable"}, SPAFallback: "/missing-asset.txt"},
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternRegex, Pattern: `^/downloads/.*\.(zip|iso)$`},
			Access: distribution.AccessSigned, Signing: &distribution.Signing{KeyGroup: "rilasci"}, SPAFallback: "/none"},
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/internal/"}, Access: distribution.AccessDeny},
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/members/"}, Access: distribution.AccessAuthenticated,
			SPAFallback: "/none"},
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/uploads/"}, Access: distribution.AccessSigned,
			Signing: &distribution.Signing{KeyGroup: "rilasci"}, AllowedMethods: []string{"GET", "PUT"}},
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/api/"},
			CORS: &distribution.CORS{AllowedOrigins: []string{"https://app.example.com"}, AllowedMethods: []string{"GET"}, MaxAge: 3600}},
	}
	d.ErrorPages = []distribution.ErrorPage{
		{Status: 404, Path: "/404.html", ResponseStatus: 404},
		{Status: 403, Path: "/404.html", ResponseStatus: 404},
	}
	d.Redirects = []distribution.Redirect{
		{From: `^/vecchio/(.*)$`, To: "/nuovo/$1", Status: 301},
		{HostFrom: "example.com", ToHost: "www.example.com", Status: 301},
		{From: `^/esterno$`, To: "//evil.example.net/"},
		{From: `^/docs-v1/(?P<page>.*)$`, To: "https://docs.example.com/${page}", Status: 302},
	}
	d.Headers.Response = map[string]string{"Strict-Transport-Security": "max-age=63072000; includeSubDomains", "X-Content-Type-Options": "nosniff"}
	stored, err := svc.Create(context.Background(), d)
	require.NoError(t, err)
	return &stored
}

// TestWebsite covers the website mode (TDS-03, TDS-05, TDS-09, TDS-12).
func TestWebsite(t *testing.T) {
	d := site(t)
	fx := newEngine(t, map[string]bool{"prod/secret.html": true})
	fx.put("prod/index.html", "<h1>home</h1>", "text/html")
	fx.put("prod/docs/index.html", "<h1>docs</h1>", "text/html")
	fx.put("prod/404.html", "<h1>not found</h1>", "text/html")
	fx.put("prod/assets/app.js", "console.log(1)", "application/javascript")
	fx.put("prod/secret.html", "secret", "text/html")
	fx.put("prod/downloads/v1.zip", "zipdata", "application/zip")
	fx.put("prod/members/a.txt", "members", "text/plain")
	fx.put("prod/api/data.json", `{"a":1}`, "application/json")
	fx.put("prod/range.txt", "0123456789", "text/plain")
	fx.put("other/outside.txt", "outside", "text/plain")

	tests := []struct {
		name     string
		req      webRequest
		status   int
		body     string
		outcome  string
		headers  map[string]string
		location string
	}{
		{name: "index at root", req: webRequest{url: "https://www.example.com/"}, status: 200, body: "<h1>home</h1>", outcome: "object",
			headers: map[string]string{"Content-Type": "text/html", "Cache-Control": "public, max-age=300",
				"Strict-Transport-Security": "max-age=63072000; includeSubDomains", "X-Content-Type-Options": "nosniff", "Server": "Nexo"}},
		{name: "index of directory", req: webRequest{url: "https://www.example.com/docs/"}, status: 200, body: "<h1>docs</h1>"},
		{name: "directory without slash", req: webRequest{url: "https://www.example.com/docs?x=1"}, status: 302, location: "/docs/?x=1"},
		{name: "wildcard alias", req: webRequest{url: "https://pr-1.preview.example.com/"}, status: 200, body: "<h1>home</h1>"},
		{name: "asset behavior cache", req: webRequest{url: "https://www.example.com/assets/app.js"}, status: 200, body: "console.log(1)",
			headers: map[string]string{"Cache-Control": "public, max-age=31536000, immutable", "Content-Type": "application/javascript"}},
		{name: "SPA fallback", req: webRequest{url: "https://www.example.com/app/settings"}, status: 200, body: "<h1>home</h1>", outcome: "spa"},
		{name: "SPA fallback missing gives error page", req: webRequest{url: "https://www.example.com/assets/none.js"}, status: 404,
			body: "<h1>not found</h1>", outcome: "error-page"},
		{name: "403 hidden as 404", req: webRequest{url: "https://www.example.com/secret.html"}, status: 404, body: "<h1>not found</h1>",
			headers: map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"}},
		{name: "deny behavior hidden as 404", req: webRequest{url: "https://www.example.com/internal/x"}, status: 404, body: "<h1>not found</h1>"},
		{name: "method not allowed", req: webRequest{method: http.MethodDelete, url: "https://www.example.com/index.html"}, status: 405,
			headers: map[string]string{"Allow": "GET, HEAD, OPTIONS"}},
		{name: "PUT not allowed outside signed", req: webRequest{method: http.MethodPut, url: "https://www.example.com/x", body: "x"}, status: 405},
		{name: "redirect regex groups", req: webRequest{url: "https://www.example.com/vecchio/a/b.html?q=1"}, status: 301, location: "/nuovo/a/b.html?q=1"},
		{name: "redirect named group absolute", req: webRequest{url: "https://www.example.com/docs-v1/intro"}, status: 302,
			location: "https://docs.example.com/intro"},
		{name: "redirect host", req: webRequest{url: "https://example.com/a?b=1"}, status: 301, location: "https://www.example.com/a?b=1"},
		{name: "protocol-relative target not followed", req: webRequest{url: "https://www.example.com/esterno"}, status: 200, outcome: "spa"},
		{name: "escape the prefix", req: webRequest{url: "https://www.example.com/../other/outside.txt"}, status: 404, body: "<h1>not found</h1>"},
		{name: "escape encoded", req: webRequest{url: "https://www.example.com/%2e%2e/other/outside.txt"}, status: 404},
		{name: "escape with backslash", req: webRequest{url: "https://www.example.com/..%5Cother/outside.txt"}, status: 404},
		{name: "signed without signature", req: webRequest{url: "https://www.example.com/downloads/v1.zip"}, status: 404, body: "<h1>not found</h1>"},
		{name: "signed URL", req: webRequest{url: "https://www.example.com/downloads/v1.zip?Expires=1&Signature=good&Key-Pair-Id=K"}, status: 200,
			body: "zipdata"},
		{name: "signed cookie", req: webRequest{url: "https://www.example.com/downloads/v1.zip",
			header: http.Header{"Cookie": {"CloudFront-Signature=good; CloudFront-Key-Pair-Id=K"}}}, status: 200, body: "zipdata"},
		{name: "authenticated without credentials", req: webRequest{url: "https://www.example.com/members/a.txt"}, status: 404},
		{name: "authenticated allowed", req: webRequest{url: "https://www.example.com/members/a.txt",
			identity: &domain.Identity{AccessKey: "alice", AuthType: domain.AuthTypeHeader}}, status: 200, body: "members"},
		{name: "authenticated not allowed", req: webRequest{url: "https://www.example.com/members/a.txt",
			identity: &domain.Identity{AccessKey: "bob", AuthType: domain.AuthTypeHeader}}, status: 404},
		{name: "range", req: webRequest{url: "https://www.example.com/range.txt", header: http.Header{"Range": {"bytes=2-5"}}}, status: 206, body: "2345",
			headers: map[string]string{"Content-Range": "bytes 2-5/10", "Content-Length": "4", "Accept-Ranges": "bytes"}},
		{name: "suffix range", req: webRequest{url: "https://www.example.com/range.txt", header: http.Header{"Range": {"bytes=-3"}}}, status: 206, body: "789"},
		{name: "unsatisfiable range", req: webRequest{url: "https://www.example.com/range.txt", header: http.Header{"Range": {"bytes=20-30"}}}, status: 416,
			headers: map[string]string{"Content-Range": "bytes */10"}},
		{name: "malformed range ignored", req: webRequest{url: "https://www.example.com/range.txt", header: http.Header{"Range": {"lines=1-2"}}}, status: 200,
			body: "0123456789"},
		{name: "not modified", req: webRequest{url: "https://www.example.com/range.txt", header: http.Header{"If-None-Match": {`"etag-prod/range.txt"`}}},
			status: 304, outcome: "not-modified", headers: map[string]string{"ETag": `"etag-prod/range.txt"`}},
		{name: "HEAD", req: webRequest{method: http.MethodHead, url: "https://www.example.com/range.txt"}, status: 200,
			headers: map[string]string{"Content-Length": "10", "Content-Type": "text/plain", "ETag": `"etag-prod/range.txt"`,
				"Last-Modified": "Thu, 01 Jan 2026 00:00:00 GMT"}},
		{name: "HEAD range", req: webRequest{method: http.MethodHead, url: "https://www.example.com/range.txt", header: http.Header{"Range": {"bytes=0-0"}}},
			status: 206, headers: map[string]string{"Content-Length": "1", "Content-Range": "bytes 0-0/10"}},
		{name: "default content type", req: webRequest{url: "https://www.example.com/docs/index.html"}, status: 200,
			headers: map[string]string{"Content-Type": "text/html"}},
		{name: "CORS preflight", req: webRequest{method: http.MethodOptions, url: "https://www.example.com/api/data.json",
			header: http.Header{"Origin": {"https://app.example.com"}, "Access-Control-Request-Method": {"GET"}}}, status: 204, outcome: "preflight",
			headers: map[string]string{"Access-Control-Allow-Origin": "https://app.example.com", "Access-Control-Max-Age": "3600"}},
		{name: "CORS preflight refused", req: webRequest{method: http.MethodOptions, url: "https://www.example.com/api/data.json",
			header: http.Header{"Origin": {"https://evil.example.net"}, "Access-Control-Request-Method": {"GET"}}}, status: 403},
		{name: "CORS simple", req: webRequest{url: "https://www.example.com/api/data.json", header: http.Header{"Origin": {"https://app.example.com"}}},
			status: 200, headers: map[string]string{"Access-Control-Allow-Origin": "https://app.example.com", "Vary": "Origin"}},
		{name: "plain OPTIONS", req: webRequest{method: http.MethodOptions, url: "https://www.example.com/"}, status: 204,
			headers: map[string]string{"Allow": "GET, HEAD, OPTIONS"}},
		{name: "signed upload refused", req: webRequest{method: http.MethodPut, url: "https://www.example.com/uploads/a.txt", body: "data"}, status: 403},
		{name: "signed upload", req: webRequest{method: http.MethodPut, url: "https://www.example.com/uploads/a.txt?Expires=1&Signature=good&Key-Pair-Id=K",
			body: "data"}, status: 200, outcome: "upload", headers: map[string]string{"ETag": `"new"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := fx.do(t, d, tc.req)
			assert.Equal(t, tc.status, resp.Status, body)
			if tc.body != "" {
				assert.Equal(t, tc.body, body)
			}
			if tc.outcome != "" {
				assert.Equal(t, tc.outcome, resp.Outcome)
			}
			for k, v := range tc.headers {
				assert.Equal(t, v, resp.Header.Get(k), k)
			}
			assert.Equal(t, tc.location, resp.Header.Get("Location"))
			assert.NotContains(t, resp.Header.Get("Content-Type"), "xml", "never S3 XML")
		})
	}
	// TDS-09: every key read stays under the prefix.
	for _, k := range fx.state.requested {
		assert.True(t, strings.HasPrefix(k, "prod/"), "key %q outside the prefix", k)
	}
	assert.Equal(t, "data", fx.state.objects["prod/uploads/a.txt"])
	// Every evaluation names the distribution.
	for _, id := range fx.seen {
		assert.Equal(t, "sito-web", id)
	}
}

func TestWebsiteHTTPSOnlyAndPublicAccess(t *testing.T) {
	d := sample("site", "www.example.com")
	d.Redirects = []distribution.Redirect{{HTTPSOnly: true}}
	fx := newEngine(t, nil)
	fx.put("index.html", "home", "text/html")

	resp, _ := fx.do(t, &d, webRequest{url: "http://www.example.com:8080/a?b=c"})
	assert.Equal(t, http.StatusMovedPermanently, resp.Status)
	assert.Equal(t, "https://www.example.com/a?b=c", resp.Header.Get("Location"))

	resp, body := fx.do(t, &d, webRequest{url: "https://www.example.com/"})
	assert.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "home", body)

	fx.blocked = true
	resp, _ = fx.do(t, &d, webRequest{url: "https://www.example.com/"})
	assert.Equal(t, http.StatusForbidden, resp.Status, "block public access")
}

// TestWebsiteRestrictions covers the origin secret with two active values
// during a rotation (TDS-07) and the address filters.
func TestWebsiteRestrictions(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	d := sample("site", "www.example.com")
	d.Restrictions.SourceIP = &distribution.IPRestriction{Allow: []string{"10.0.0.0/8", "2001:db8::/32"}, Deny: []string{"10.6.6.0/24"}}
	d.Restrictions.OriginSecretHeader = &distribution.OriginSecret{Name: "X-Origin-Secret", Values: []string{"proxy-a", "proxy-a-next"}}
	stored, err := svc.Create(ctx, d)
	require.NoError(t, err)
	fx := newEngine(t, nil)
	fx.put("index.html", "home", "text/html")

	secret := func(v string) http.Header { return http.Header{"X-Origin-Secret": {v}} }
	tests := []struct {
		name   string
		header http.Header
		ip     string
		status int
	}{
		{name: "first secret", header: secret("proxy-a"), ip: "10.1.2.3", status: 200},
		{name: "second secret", header: secret("proxy-a-next"), ip: "10.1.2.3", status: 200},
		{name: "IPv6 client", header: secret("proxy-a"), ip: "2001:db8::1", status: 200},
		{name: "wrong secret", header: secret("proxy-b"), ip: "10.1.2.3", status: 403},
		{name: "no secret", ip: "10.1.2.3", status: 403},
		{name: "denied range", header: secret("proxy-a"), ip: "10.6.6.6", status: 403},
		{name: "outside allow", header: secret("proxy-a"), ip: "192.0.2.1", status: 403},
		{name: "mapped IPv4", header: secret("proxy-a"), ip: "::ffff:10.1.2.3", status: 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := fx.do(t, &stored, webRequest{url: "https://www.example.com/", header: tc.header, clientIP: tc.ip})
			assert.Equal(t, tc.status, resp.Status)
			s3 := stored
			s3.Mode = distribution.ModeS3
			admitted := fx.engine.Admit(ctx, distribution.WebRequest{Route: distribution.Route{Distribution: &s3}, Header: tc.header, ClientIP: tc.ip})
			assert.Equal(t, tc.status == 200, admitted == nil, "s3 mode admission")
		})
	}
	off := stored
	off.Enabled = false
	off.ETag = "" // another version of the distribution
	resp, _ := fx.do(t, &off, webRequest{url: "https://www.example.com/", header: secret("proxy-a"), clientIP: "10.1.2.3"})
	assert.Equal(t, http.StatusForbidden, resp.Status, "disabled")
}

// FuzzWebsitePath checks that no path leaves the origin prefix nor panics
// (TDS-09, TDS-14).
func FuzzWebsitePath(f *testing.F) {
	for _, s := range []string{"/", "/../x", "/%2e%2e/x", "/a/./b", "/a//b", "/\x00", "/..\\x", "/assets/**", "/downloads/a.zip"} {
		f.Add(s, "a=b")
	}
	f.Fuzz(func(t *testing.T, rawPath, rawQuery string) {
		d := sample("site", "www.example.com")
		d.Origin.PathPrefix = "/prod"
		d.DefaultBehavior.SPAFallback = "/index.html"
		d.ErrorPages = []distribution.ErrorPage{{Status: 404, Path: "/404.html"}}
		fx := newEngine(t, nil)
		fx.put("prod/index.html", "home", "text/html")
		path, err := url.PathUnescape(rawPath)
		if err != nil {
			return
		}
		resp, err := fx.engine.Serve(context.Background(), distribution.WebRequest{
			Route: distribution.Route{Kind: distribution.RouteDistribution, Distribution: &d}, Host: "www.example.com",
			Method: http.MethodGet, Path: path, EscapedPath: rawPath, RawQuery: rawQuery, Header: http.Header{}, ClientIP: "192.0.2.1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		for _, k := range fx.state.requested {
			if !strings.HasPrefix(k, "prod/") {
				t.Fatalf("path %q read key %q outside the prefix", rawPath, k)
			}
			for _, seg := range strings.Split(k, "/") {
				if seg == ".." || seg == "." {
					t.Fatalf("path %q read key %q", rawPath, k)
				}
			}
		}
	})
}

// TestOriginSecretReference resolves an origin secret given by name.
func TestOriginSecretReference(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	d := sample("site", "www.example.com")
	d.Restrictions.OriginSecretHeader = &distribution.OriginSecret{Values: []string{"${secret:proxy-b}", "${secret:missing}"}}
	stored, err := svc.Create(ctx, d)
	require.NoError(t, err)
	assert.Equal(t, []string{"${secret:proxy-b}", "${secret:missing}"}, stored.Restrictions.OriginSecretHeader.Values, "references are not hashed")

	secrets := distmock.NewSecretSource(t)
	secrets.EXPECT().LookupSecret(tmock.Anything, "proxy-b").Return("token-b", true)
	secrets.EXPECT().LookupSecret(tmock.Anything, "missing").Return("", false)
	fx := newEngine(t, nil)
	fx.put("index.html", "home", "text/html")
	engine, err := distribution.NewWebsite(distribution.WebsiteDeps{
		Objects: objectLayer(t, fx.state), Policy: policyEngine(t, nil, &fx.seen), Signatures: distmock.NewSignedRequestVerifier(t),
		Secrets: secrets, Logger: slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	fx.engine = engine
	resp, _ := fx.do(t, &stored, webRequest{url: "https://www.example.com/", header: http.Header{"X-Origin-Secret": {"token-b"}}})
	assert.Equal(t, http.StatusOK, resp.Status)
	resp, _ = fx.do(t, &stored, webRequest{url: "https://www.example.com/", header: http.Header{"X-Origin-Secret": {""}}})
	assert.Equal(t, http.StatusForbidden, resp.Status, "an unresolved secret matches nothing")
}

// TestWebsiteBlockPublicAccess is TAR-08 for the distributions: a public
// behavior refuses its requests while the block public access is on,
// whatever grants the read (an access rule for distribution:<id>, a
// public bucket policy); an unknown setting is a 503 to retry.
func TestWebsiteBlockPublicAccess(t *testing.T) {
	d := sample("bpa", "www.example.com")
	pol := polmock.NewService(t)
	// Every read is allowed explicitly (an access rule allowing
	// distribution:bpa).
	pol.EXPECT().Evaluate(tmock.Anything, tmock.Anything).Return(policy.Decision{Allowed: true, Source: policy.SourceAccessRule}, nil).Maybe()
	fx := (&engineFixture{}).start(t, pol)
	fx.put("a.txt", "PUBLIC?", "text/plain")
	resp, body := fx.do(t, &d, webRequest{url: "https://www.example.com/a.txt"})
	require.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "PUBLIC?", body)

	fx.blocked = true
	resp, body = fx.do(t, &d, webRequest{url: "https://www.example.com/a.txt"})
	assert.Equal(t, http.StatusForbidden, resp.Status, "blocked although a rule allows the distribution")
	assert.NotContains(t, body, "PUBLIC?")

	fx.blocked, fx.blockErr = false, domain.ErrReadQuorum
	resp, _ = fx.do(t, &d, webRequest{url: "https://www.example.com/a.txt"})
	assert.Equal(t, http.StatusServiceUnavailable, resp.Status, "not decided: retry, never 403")
	assert.Equal(t, "1", resp.Header.Get("Retry-After"))

	fx.blockErr = domain.ErrBucketNotFound
	resp, _ = fx.do(t, &d, webRequest{url: "https://www.example.com/a.txt"})
	assert.Equal(t, http.StatusOK, resp.Status, "the read decides a missing bucket")
}

// TestWebsiteSecurity covers the redirects that would leave the site, the
// existence oracle of the index documents, CORS with credentials and the
// If-Range of the ranges (TDS-03, TDS-05, TDS-12).
func TestWebsiteSecurity(t *testing.T) {
	d := sample("sec", "www.example.com")
	d.Redirects = []distribution.Redirect{
		{From: `^/go/(.*)$`, To: "/$1", Status: http.StatusFound},
		// A template that would make "/\\host": never followed.
		{From: `^/bs/(.*)$`, To: `/\$1`, Status: http.StatusFound},
	}
	d.ErrorPages = []distribution.ErrorPage{{Status: 403, Path: "/404.html", ResponseStatus: 404}, {Status: 404, Path: "/404.html"}}
	d.Behaviors = []distribution.Behavior{
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/api/"},
			CORS: &distribution.CORS{AllowedOrigins: []string{"*"}, AllowCredentials: true}},
	}
	fx := newEngine(t, map[string]bool{"vault/index.html": true})
	fx.put("404.html", "missing", "text/html")
	fx.put("docs/index.html", "docs", "text/html")
	fx.put("vault/index.html", "secret", "text/html")
	fx.put("api/data.json", "{}", "application/json")
	fx.put("big.bin", "0123456789", "application/octet-stream")
	etag := `"etag-big.bin"`
	modified := "Thu, 01 Jan 2026 00:00:00 GMT"

	tests := []struct {
		name     string
		req      webRequest
		status   int
		body     string
		location string
		headers  map[string]string
	}{
		{name: "redirect kept on the site", req: webRequest{url: "https://www.example.com/go/a/b"}, status: 302, location: "/a/b"},
		{name: "redirect capture escaped", req: webRequest{url: "https://www.example.com/go/a%20b%3Fx"}, status: 302, location: "/a%20b%3Fx"},
		// The captures are escaped: a browser never reads them as "//host".
		{name: "backslash stays a path", req: webRequest{url: "https://www.example.com/go/%5Cevil.example/x"}, status: 302,
			location: "/%5Cevil.example/x"},
		{name: "tab stays a path", req: webRequest{url: "https://www.example.com/go/%09/evil.example/x"}, status: 302,
			location: "/%09/evil.example/x"},
		{name: "protocol-relative not followed", req: webRequest{url: "https://www.example.com/go//evil.example/x"}, status: 404},
		{name: "backslash target not followed", req: webRequest{url: "https://www.example.com/bs/evil.example"}, status: 404},
		{name: "index of an allowed directory", req: webRequest{url: "https://www.example.com/docs"}, status: 302, location: "/docs/"},
		{name: "no oracle of a denied index", req: webRequest{url: "https://www.example.com/vault"}, status: 404, body: "missing"},
		{name: "same answer as a missing path", req: webRequest{url: "https://www.example.com/nothing"}, status: 404, body: "missing"},
		{name: "no credentials with a wildcard origin", req: webRequest{url: "https://www.example.com/api/data.json",
			header: http.Header{"Origin": {"https://evil.example"}}}, status: 200,
			headers: map[string]string{"Access-Control-Allow-Origin": "*", "Access-Control-Allow-Credentials": ""}},
		{name: "If-Range strong match", req: webRequest{url: "https://www.example.com/big.bin",
			header: http.Header{"Range": {"bytes=2-5"}, "If-Range": {etag}}}, status: 206, body: "2345"},
		{name: "If-Range mismatch sends the whole object", req: webRequest{url: "https://www.example.com/big.bin",
			header: http.Header{"Range": {"bytes=2-5"}, "If-Range": {`"deadbeef"`}}}, status: 200, body: "0123456789",
			headers: map[string]string{"Content-Length": "10", "Content-Range": ""}},
		{name: "If-Range weak never matches", req: webRequest{url: "https://www.example.com/big.bin",
			header: http.Header{"Range": {"bytes=2-5"}, "If-Range": {"W/" + etag}}}, status: 200, body: "0123456789"},
		{name: "If-Range date match", req: webRequest{url: "https://www.example.com/big.bin",
			header: http.Header{"Range": {"bytes=0-1"}, "If-Range": {modified}}}, status: 206, body: "01"},
		{name: "If-Range date mismatch", req: webRequest{url: "https://www.example.com/big.bin",
			header: http.Header{"Range": {"bytes=0-1"}, "If-Range": {"Fri, 02 Jan 2026 00:00:00 GMT"}}}, status: 200, body: "0123456789"},
		{name: "HEAD If-Range mismatch", req: webRequest{method: http.MethodHead, url: "https://www.example.com/big.bin",
			header: http.Header{"Range": {"bytes=2-5"}, "If-Range": {`"deadbeef"`}}}, status: 200,
			headers: map[string]string{"Content-Length": "10", "Content-Range": ""}},
		{name: "HEAD If-Range match", req: webRequest{method: http.MethodHead, url: "https://www.example.com/big.bin",
			header: http.Header{"Range": {"bytes=2-5"}, "If-Range": {etag}}}, status: 206,
			headers: map[string]string{"Content-Length": "4", "Content-Range": "bytes 2-5/10"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := fx.do(t, &d, tc.req)
			assert.Equal(t, tc.status, resp.Status, body)
			if tc.body != "" {
				assert.Equal(t, tc.body, body)
			}
			assert.Equal(t, tc.location, resp.Header.Get("Location"))
			for k, v := range tc.headers {
				assert.Equal(t, v, resp.Header.Get(k), k)
			}
		})
	}
}

// TestWebsiteS3ModeAccess is TDS-04/TDS-05 in the s3 mode: the admission
// before the authentication, then the access of the default behavior.
func TestWebsiteS3ModeAccess(t *testing.T) {
	ctx := context.Background()
	fx := newEngine(t, nil)
	req := func(d *distribution.Distribution, id *domain.Identity) distribution.WebRequest {
		return distribution.WebRequest{Route: distribution.Route{Kind: distribution.RouteDistribution, Distribution: d}, Method: http.MethodPut,
			Path: "/internal/secret.txt", Header: http.Header{}, ClientIP: "192.0.2.1", Identity: id}
	}
	alice := &domain.Identity{AccessKey: "alice", AuthType: domain.AuthTypeHeader}
	public := distribution.Distribution{ID: "s3p", Enabled: true, Mode: distribution.ModeS3, Origin: distribution.Origin{Bucket: "web"}}
	assert.Nil(t, fx.engine.Admit(ctx, req(&public, nil)))
	assert.Nil(t, fx.engine.AuthorizeS3(ctx, req(&public, nil)), "IAM and the policies decide")

	authn := public
	authn.ID, authn.DefaultBehavior.Access = "s3a", distribution.AccessAuthenticated
	assert.Nil(t, fx.engine.Admit(ctx, req(&authn, nil)), "the identity is not known before the authentication")
	resp := fx.engine.AuthorizeS3(ctx, req(&authn, nil))
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.Status)
	assert.Nil(t, fx.engine.AuthorizeS3(ctx, req(&authn, alice)))
	assert.NotNil(t, fx.engine.AuthorizeS3(ctx, req(&authn, domain.Anonymous())))

	deny := public
	deny.ID, deny.DefaultBehavior.Access = "s3d", distribution.AccessDeny
	resp = fx.engine.Admit(ctx, req(&deny, alice))
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.Status)

	site := sample("site", "www.example.com")
	assert.Nil(t, fx.engine.AuthorizeS3(ctx, req(&site, nil)), "not an s3 distribution")
}
