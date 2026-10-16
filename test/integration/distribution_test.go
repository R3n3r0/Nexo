package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyendpoints "github.com/aws/smithy-go/endpoints"
	"github.com/letsencrypt/challtestsrv"
	pebbleca "github.com/letsencrypt/pebble/v2/ca"
	pebbledb "github.com/letsencrypt/pebble/v2/db"
	pebbleva "github.com/letsencrypt/pebble/v2/va"
	pebblewfe "github.com/letsencrypt/pebble/v2/wfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/internal/testutil/dnstest"
)

// hostRequest sends a request for host to the server at base (a node or the
// load balancer): the domains are simulated with the Host header.
func hostRequest(t *testing.T, method, base, host, pathAndQuery string, hdr http.Header) response {
	t.Helper()
	req := newRequest(t, method, base+pathAndQuery, nil)
	req.Host = host
	for k, v := range hdr {
		req.Header[k] = v
	}
	return do(t, req)
}

// putTyped stores an object with its content type.
func (e *env) putTyped(bucket, key, body, contentType string) {
	e.t.Helper()
	_, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key),
		Body: strings.NewReader(body), ContentType: aws.String(contentType)})
	require.NoError(e.t, err)
}

// nodeURLs are the S3 URLs of every node of the deployment.
func (e *env) nodeURLs() []string {
	var out []string
	for _, n := range e.srv.Nodes {
		out = append(out, n.S3URL)
	}
	return out
}

// TestDistributionWebsite serves a website distribution on its domains
// from every node (TDS-03, TDS-05, TDS-09, TDS-12, TCL-D-01) and applies
// its changes on every node without restart (TCD-03).
//
// covers: S-DST-01, S-DST-02, S-DST-03
func TestDistributionWebsite(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("web")
	e.putTyped("web", "prod/index.html", "<h1>home</h1>", "text/html")
	e.putTyped("web", "prod/docs/index.html", "<h1>docs</h1>", "text/html")
	e.putTyped("web", "prod/404.html", "<h1>not found</h1>", "text/html")
	e.putTyped("web", "prod/assets/app.js", "console.log(1)", "application/javascript")
	e.putTyped("web", "prod/range.txt", "0123456789", "text/plain")
	e.putTyped("web", "secret.txt", "outside the prefix", "text/plain")

	d := distribution.Distribution{
		ID: "website", Enabled: true, Aliases: []string{"www.site.test", "site.test", "*.preview.site.test"},
		Origin: distribution.Origin{Bucket: "web", PathPrefix: "/prod"}, Mode: distribution.ModeWebsite,
		DefaultBehavior: distribution.Behavior{Access: distribution.AccessPublic, IndexDocument: "index.html", SPAFallback: "/index.html",
			Cache: &distribution.Cache{CacheControl: "public, max-age=300"}},
		Behaviors: []distribution.Behavior{
			{PathPattern: &distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/assets/**"},
				Cache: &distribution.Cache{CacheControl: "public, max-age=31536000, immutable"}, SPAFallback: "/none"},
			{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/internal/"}, Access: distribution.AccessDeny},
		},
		ErrorPages: []distribution.ErrorPage{{Status: 404, Path: "/404.html"}, {Status: 403, Path: "/404.html", ResponseStatus: 404}},
		Redirects: []distribution.Redirect{
			{From: `^/old/(.*)$`, To: "/new/$1", Status: 301},
			{HostFrom: "site.test", ToHost: "www.site.test", Status: 301},
		},
		Headers: distribution.Headers{Response: map[string]string{"X-Content-Type-Options": "nosniff"}},
	}
	// Created on the first node, served by every node.
	_, err := e.srv.Nodes[0].App.Distributions().Create(e.ctx, d)
	require.NoError(t, err)

	tests := []struct {
		name, host, path string
		hdr              http.Header
		status           int
		body             string
		headers          map[string]string
	}{
		{name: "index", host: "www.site.test", path: "/", status: 200, body: "<h1>home</h1>",
			headers: map[string]string{"Content-Type": "text/html", "Cache-Control": "public, max-age=300", "X-Content-Type-Options": "nosniff"}},
		{name: "index with port", host: "www.site.test:9000", path: "/docs/", status: 200, body: "<h1>docs</h1>"},
		{name: "wildcard alias", host: "pr-7.preview.site.test", path: "/", status: 200, body: "<h1>home</h1>"},
		{name: "asset", host: "www.site.test", path: "/assets/app.js", status: 200, body: "console.log(1)",
			headers: map[string]string{"Content-Type": "application/javascript", "Cache-Control": "public, max-age=31536000, immutable"}},
		{name: "SPA fallback", host: "www.site.test", path: "/app/settings", status: 200, body: "<h1>home</h1>"},
		{name: "missing asset", host: "www.site.test", path: "/assets/none.js", status: 404, body: "<h1>not found</h1>"},
		{name: "deny behind 404", host: "www.site.test", path: "/internal/x", status: 404, body: "<h1>not found</h1>"},
		{name: "prefix escape", host: "www.site.test", path: "/../secret.txt", status: 404, body: "<h1>not found</h1>"},
		{name: "prefix escape encoded", host: "www.site.test", path: "/%2e%2e/secret.txt", status: 404},
		{name: "redirect regex", host: "www.site.test", path: "/old/a.html", status: 301, headers: map[string]string{"Location": "/new/a.html"}},
		{name: "redirect host", host: "site.test", path: "/a?b=1", status: 301, headers: map[string]string{"Location": "http://www.site.test/a?b=1"}},
		{name: "range", host: "www.site.test", path: "/range.txt", hdr: http.Header{"Range": {"bytes=2-5"}}, status: 206, body: "2345",
			headers: map[string]string{"Content-Range": "bytes 2-5/10", "Content-Type": "text/plain", "Accept-Ranges": "bytes"}},
		{name: "unsatisfiable range", host: "www.site.test", path: "/range.txt", hdr: http.Header{"Range": {"bytes=50-"}}, status: 416,
			headers: map[string]string{"Content-Range": "bytes */10"}},
		{name: "method not allowed", host: "www.site.test", path: "/range.txt", status: 405},
		// The reserved paths of the S3 listener stay the server's whatever
		// the host (TestDistributionAliasTakeover): the health probe
		// answers, not the site.
		{name: "health path", host: "www.site.test", path: "/nexo/health/live", status: 200},
	}
	for _, base := range e.nodeURLs() {
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				method := http.MethodGet
				if tc.name == "method not allowed" {
					method = http.MethodDelete
				}
				resp := hostRequest(t, method, base, tc.host, tc.path, tc.hdr)
				assert.Equal(t, tc.status, resp.StatusCode, "%s", resp.Body)
				if tc.body != "" {
					assert.Equal(t, tc.body, string(resp.Body))
				}
				if tc.name == "health path" {
					assert.NotContains(t, string(resp.Body), "<h1>home</h1>")
				}
				for k, v := range tc.headers {
					assert.Equal(t, v, resp.Header.Get(k), k)
				}
				assert.NotContains(t, resp.Header.Get("Content-Type"), "xml")
			})
		}
	}

	// Conditional GET and HEAD.
	base := e.srv.S3URL
	first := hostRequest(t, http.MethodGet, base, "www.site.test", "/range.txt", nil)
	etag := first.Header.Get("ETag")
	require.NotEmpty(t, etag)
	resp := hostRequest(t, http.MethodGet, base, "www.site.test", "/range.txt", http.Header{"If-None-Match": {etag}})
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	assert.Empty(t, resp.Body)
	resp = hostRequest(t, http.MethodGet, base, "www.site.test", "/range.txt",
		http.Header{"If-Modified-Since": {first.Header.Get("Last-Modified")}})
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	resp = hostRequest(t, http.MethodHead, base, "www.site.test", "/range.txt", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "10", resp.Header.Get("Content-Length"))
	assert.Equal(t, etag, resp.Header.Get("ETag"))
	assert.Empty(t, resp.Body)

	// A change on the last node applies on every node at once (TCD-03).
	last := e.srv.Nodes[len(e.srv.Nodes)-1].App.Distributions()
	cur, err := last.Get(e.ctx, "website")
	require.NoError(t, err)
	cur.Aliases = append(cur.Aliases, "static.site.test")
	_, err = last.Update(e.ctx, cur, cur.ETag)
	require.NoError(t, err)
	for _, base := range e.nodeURLs() {
		resp := hostRequest(t, http.MethodGet, base, "static.site.test", "/", nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "new alias on %s", base)
	}
	_, err = last.SetEnabled(e.ctx, "website", false)
	require.NoError(t, err)
	for _, base := range e.nodeURLs() {
		resp := hostRequest(t, http.MethodGet, base, "www.site.test", "/", nil)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "disabled on %s", base)
	}
	require.NoError(t, e.srv.Nodes[0].App.Distributions().Delete(e.ctx, "website"))
	for _, base := range e.nodeURLs() {
		resp := hostRequest(t, http.MethodGet, base, "www.site.test", "/", nil)
		assert.NotEqual(t, "<h1>home</h1>", string(resp.Body), "deleted on %s", base)
	}
}

// fixedEndpoint resolves every request of the SDK to one endpoint: the
// custom domain of an s3 distribution is the bucket itself.
type fixedEndpoint struct{ u url.URL }

func (f fixedEndpoint) ResolveEndpoint(context.Context, s3.EndpointParameters) (smithyendpoints.Endpoint, error) {
	return smithyendpoints.Endpoint{URI: f.u}, nil
}

// TestDistributionS3Mode uses an s3 distribution as the S3 endpoint of its
// bucket with the SDK: SigV4 with the public host, presigned URLs of that
// domain (TDS-04), and the restrictions of the distribution.
//
// covers: S-DST-02
func TestDistributionS3Mode(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("customer")
	e.mkBucket("other")
	e.put("other", "x.txt", []byte("other bucket"))
	_, err := e.srv.App.Distributions().Create(e.ctx, distribution.Distribution{
		ID: "s3-customer", Enabled: true, Aliases: []string{"s3.customer.test"}, Mode: distribution.ModeS3,
		Origin: distribution.Origin{Bucket: "customer"},
	})
	require.NoError(t, err)

	_, port, err := net.SplitHostPort(e.hostPort())
	require.NoError(t, err)
	endpoint := url.URL{Scheme: "http", Host: "s3.customer.test:" + port}
	cfg := e.sdkConfig(e.rootCreds())
	cfg.HTTPClient = e.loopbackClient()
	client := s3.NewFromConfig(cfg, func(o *s3.Options) { o.EndpointResolverV2 = fixedEndpoint{u: endpoint} })

	_, err = client.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("customer"), Key: aws.String("docs/a.txt"), Body: strings.NewReader("hello")})
	require.NoError(t, err)
	got, err := client.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("customer"), Key: aws.String("docs/a.txt")})
	require.NoError(t, err)
	body, _ := io.ReadAll(got.Body)
	_ = got.Body.Close()
	assert.Equal(t, "hello", string(body))
	// The object is in the bucket of the distribution.
	assert.Equal(t, []byte("hello"), e.get("customer", "docs/a.txt"))
	list, err := client.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String("customer")})
	require.NoError(t, err)
	require.Len(t, list.Contents, 1)
	assert.Equal(t, "docs/a.txt", aws.ToString(list.Contents[0].Key))
	// POST /?delete is an S3 request of the bucket (not STS).
	del, err := client.DeleteObjects(e.ctx, &s3.DeleteObjectsInput{Bucket: aws.String("customer"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("missing.txt")}}}})
	require.NoError(t, err)
	assert.Len(t, del.Deleted, 1)

	// Presigned URLs of the domain.
	presigned, err := s3.NewPresignClient(client).PresignGetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("customer"), Key: aws.String("docs/a.txt")})
	require.NoError(t, err)
	assert.Contains(t, presigned.URL, "://s3.customer.test:"+port+"/docs/a.txt?")
	resp := doWith(t, e.loopbackClient(), newRequest(t, http.MethodGet, presigned.URL, nil))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "hello", string(resp.Body))
	tampered := strings.Replace(presigned.URL, "docs/a.txt", "docs/b.txt", 1)
	resp = doWith(t, e.loopbackClient(), newRequest(t, http.MethodGet, tampered, nil))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// The domain addresses its bucket only: another bucket is a key.
	_, err = client.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("customer"), Key: aws.String("other/x.txt")})
	assert.Equal(t, "NoSuchKey", apiCode(err))
	// Anonymous requests follow the policies of the bucket.
	resp = doWith(t, e.loopbackClient(), newRequest(t, http.MethodGet, endpoint.String()+"/docs/a.txt", nil))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// Restrictions apply in the s3 mode too: the origin secret.
	dists := e.srv.App.Distributions()
	secret, _, err := dists.RotateOriginSecret(e.ctx, distribution.RotateSecretInput{ID: "s3-customer"})
	require.NoError(t, err)
	_, err = client.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("customer"), Key: aws.String("docs/a.txt")})
	assert.Equal(t, http.StatusForbidden, httpStatus(err), "no origin secret")
	withSecret := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.EndpointResolverV2 = fixedEndpoint{u: endpoint}
		o.HTTPClient = headerClient{c: e.loopbackClient(), name: distribution.DefaultOriginSecretHeader, value: secret}
	})
	_, err = withSecret.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("customer"), Key: aws.String("docs/a.txt")})
	require.NoError(t, err)
}

// headerClient adds a header to every request (a proxy with its origin
// secret).
type headerClient struct {
	c           *http.Client
	name, value string
}

func (h headerClient) Do(r *http.Request) (*http.Response, error) {
	r.Header.Set(h.name, h.value)
	return h.c.Do(r)
}

// TestDistributionSignedAndRestricted covers the signed URLs and cookies
// with the rotation of the keys (TDS-06), the origin secret with two
// active values (TDS-07) and the address restrictions.
//
// covers: S-DST-04, S-DST-05
func TestDistributionSignedAndRestricted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("rilasci")
	e.putTyped("rilasci", "downloads/v1.zip", "zipdata", "application/zip")
	e.putTyped("rilasci", "index.html", "home", "text/html")
	app := e.srv.App
	dists := app.Distributions()
	_, err := dists.CreateKeyGroup(e.ctx, "rilasci", "downloads")
	require.NoError(t, err)
	k1, err := dists.GenerateKey(e.ctx, distribution.GenerateKeyInput{KeyGroup: "rilasci"})
	require.NoError(t, err)
	_, err = dists.Create(e.ctx, distribution.Distribution{
		ID: "download", Enabled: true, Aliases: []string{"dl.site.test"}, Mode: distribution.ModeWebsite,
		Origin:          distribution.Origin{Bucket: "rilasci"},
		DefaultBehavior: distribution.Behavior{Access: distribution.AccessPublic, IndexDocument: "index.html"},
		Behaviors: []distribution.Behavior{{
			PathPattern: &distribution.PathPattern{Type: distribution.PatternRegex, Pattern: `^/downloads/.*\.(zip|iso)$`},
			Access:      distribution.AccessSigned, Signing: &distribution.Signing{KeyGroup: "rilasci"},
		}},
	})
	require.NoError(t, err)

	signer := app.DistributionSigner()
	sign := func(k distribution.GeneratedKey, in distribution.SignInput) string {
		in.KeyPairID, in.PrivateKeyPEM = k.PublicKey.ID, k.PrivateKeyPEM
		if in.Expires.IsZero() {
			in.Expires = time.Now().Add(time.Hour)
		}
		u, err := signer.SignURL(e.ctx, in)
		require.NoError(t, err)
		parsed, err := url.Parse(u)
		require.NoError(t, err)
		return parsed.RequestURI()
	}
	get := func(path string, hdr http.Header) int {
		return hostRequest(t, http.MethodGet, e.srv.S3URL, "dl.site.test", path, hdr).StatusCode
	}
	signed := sign(k1, distribution.SignInput{URL: "http://dl.site.test/downloads/v1.zip"})
	assert.Equal(t, http.StatusOK, get(signed, nil))
	assert.Equal(t, http.StatusForbidden, get("/downloads/v1.zip", nil), "unsigned")
	assert.Equal(t, http.StatusForbidden, get(strings.Replace(signed, "Signature=", "Signature=AA", 1), nil), "tampered")
	assert.Equal(t, http.StatusOK, get("/", nil), "the public part")

	// Expired: signed by a signer whose clock is two hours behind.
	past := distribution.NewSigner(distribution.SignerDeps{Clock: clock.NewFake(time.Now().Add(-2 * time.Hour))})
	expiredURL, err := past.SignURL(e.ctx, distribution.SignInput{URL: "http://dl.site.test/downloads/v1.zip", KeyPairID: k1.PublicKey.ID,
		PrivateKeyPEM: k1.PrivateKeyPEM, Expires: time.Now().Add(-time.Hour)})
	require.NoError(t, err)
	u, _ := url.Parse(expiredURL)
	assert.Equal(t, http.StatusForbidden, get(u.RequestURI(), nil), "expired")

	// Custom policy: wildcard resource, client address.
	wild := sign(k1, distribution.SignInput{URL: "http://dl.site.test/downloads/v1.zip", Resource: "http://dl.site.test/downloads/*", IPRange: "127.0.0.0/8"})
	assert.Equal(t, http.StatusOK, get(wild, nil))
	other := sign(k1, distribution.SignInput{URL: "http://dl.site.test/downloads/v1.zip", IPRange: "192.0.2.0/24"})
	assert.Equal(t, http.StatusForbidden, get(other, nil), "other address")

	// Signed cookies.
	cookies, err := signer.SignCookies(e.ctx, distribution.SignInput{URL: "http://dl.site.test/downloads/v1.zip",
		Resource: "http://dl.site.test/downloads/*", KeyPairID: k1.PublicKey.ID, PrivateKeyPEM: k1.PrivateKeyPEM, Expires: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	var jar []string
	for _, c := range cookies {
		jar = append(jar, c.Name+"="+c.Value)
	}
	assert.Equal(t, http.StatusOK, get("/downloads/v1.zip", http.Header{"Cookie": {strings.Join(jar, "; ")}}))

	// Key rotation: a second key works at once, the first one until
	// removed.
	k2, err := dists.GenerateKey(e.ctx, distribution.GenerateKeyInput{KeyGroup: "rilasci", Algorithm: distribution.KeyRSA})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, get(sign(k2, distribution.SignInput{URL: "http://dl.site.test/downloads/v1.zip"}), nil))
	require.NoError(t, dists.DeleteKey(e.ctx, "rilasci", k1.PublicKey.ID))
	assert.Equal(t, http.StatusForbidden, get(signed, nil), "removed key")

	// Origin secrets: two active during a rotation.
	first, _, err := dists.RotateOriginSecret(e.ctx, distribution.RotateSecretInput{ID: "download"})
	require.NoError(t, err)
	second, _, err := dists.RotateOriginSecret(e.ctx, distribution.RotateSecretInput{ID: "download"})
	require.NoError(t, err)
	for _, base := range e.nodeURLs() {
		hdr := func(v string) http.Header { return http.Header{distribution.DefaultOriginSecretHeader: {v}} }
		assert.Equal(t, http.StatusOK, hostRequest(t, http.MethodGet, base, "dl.site.test", "/", hdr(first)).StatusCode)
		assert.Equal(t, http.StatusOK, hostRequest(t, http.MethodGet, base, "dl.site.test", "/", hdr(second)).StatusCode)
		assert.Equal(t, http.StatusForbidden, hostRequest(t, http.MethodGet, base, "dl.site.test", "/", hdr("wrong")).StatusCode)
		assert.Equal(t, http.StatusForbidden, hostRequest(t, http.MethodGet, base, "dl.site.test", "/", nil).StatusCode)
	}

	// Address restrictions: the loopback clients of the test are denied.
	d, err := dists.Get(e.ctx, "download")
	require.NoError(t, err)
	d.Restrictions.OriginSecretHeader = nil
	d.Restrictions.SourceIP = &distribution.IPRestriction{Deny: []string{"127.0.0.0/8", "::1"}}
	_, err = dists.Update(e.ctx, d, "")
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, get("/", nil))
}

// testLeaf issues a self-signed certificate for names.
func testLeaf(t *testing.T, names ...string) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), pool
}

// TestDistributionWebListeners serves the distributions on the dedicated
// listeners: plain HTTP (ACME challenges, 421 for the other hosts) and
// TLS with the certificate of every domain selected by SNI on every node
// (TDS-10).
//
// covers: S-DST-06
func TestDistributionWebListeners(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithConfig(func(c *config.Static) {
		c.WebAddress = "127.0.0.1:0"
		c.WebTLSAddress = "127.0.0.1:0"
	}))
	e.mkBucket("web")
	e.putTyped("web", "index.html", "home", "text/html")
	app := e.srv.Nodes[0].App
	for _, id := range []string{"a", "b"} {
		_, err := app.Distributions().Create(e.ctx, distribution.Distribution{
			ID: "site-" + id, Enabled: true, Aliases: []string{id + ".site.test"}, Origin: distribution.Origin{Bucket: "web"},
			DefaultBehavior: distribution.Behavior{Access: distribution.AccessPublic, IndexDocument: "index.html"},
			TLS:             distribution.TLS{Certificate: distribution.CertificateUploadPrefix + "cert-" + id},
		})
		require.NoError(t, err)
	}
	pools := map[string]*x509.CertPool{}
	for _, id := range []string{"a", "b"} {
		chain, key, pool := testLeaf(t, id+".site.test")
		_, err := app.Certificates().Upload(e.ctx, certs.UploadInput{ID: "cert-" + id, ChainPEM: chain, KeyPEM: key})
		require.NoError(t, err)
		pools[id] = pool
	}

	for i, n := range e.srv.Nodes {
		webAddr, tlsAddr := n.App.WebAddr(), n.App.WebTLSAddr()
		require.NotEmpty(t, webAddr, "node %d", i+1)
		require.NotEmpty(t, tlsAddr, "node %d", i+1)
		base := "http://" + webAddr
		resp := hostRequest(t, http.MethodGet, base, "a.site.test", "/", nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "home", string(resp.Body))
		resp = hostRequest(t, http.MethodGet, base, "127.0.0.1", "/web/index.html", nil)
		assert.Equal(t, http.StatusMisdirectedRequest, resp.StatusCode, "the web listener serves no S3 API")
		resp = hostRequest(t, http.MethodGet, base, "a.site.test", "/.well-known/acme-challenge/unknown", nil)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)

		for _, id := range []string{"a", "b"} {
			host := id + ".site.test"
			client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
				TLSClientConfig: &tls.Config{ServerName: host, RootCAs: pools[id], MinVersion: tls.VersionTLS12},
			}}
			req := newRequest(t, http.MethodGet, "https://"+tlsAddr+"/", nil)
			req.Host = host
			resp := doWith(t, client, req)
			assert.Equal(t, http.StatusOK, resp.StatusCode, "node %d %s", i+1, host)
			require.NotNil(t, resp.TLS)
			assert.Equal(t, host, resp.TLS.PeerCertificates[0].Subject.CommonName)
		}
		// A name without certificate fails the handshake.
		conn, err := tls.Dial("tcp", tlsAddr, &tls.Config{ServerName: "c.site.test", InsecureSkipVerify: true}) //nolint:gosec // the handshake must fail
		if err == nil {
			_ = conn.Close()
		}
		assert.Error(t, err)
	}
}

// lateHandler serves with a handler installed after the server started.
type lateHandler struct{ h atomic.Pointer[http.Handler] }

func (l *lateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := l.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	http.Error(w, "not ready", http.StatusServiceUnavailable)
}

// TestDistributionACME obtains the certificate of a distribution with
// tls.certificate "auto" from Pebble (the ACME test CA, in the test
// process) through the HTTP-01 challenge answered by the web listener of
// another node, and serves it by SNI on every node (TDS-10).
//
// covers: S-DST-06
func TestDistributionACME(t *testing.T) {
	t.Parallel()
	// Read by Pebble when it starts: no random validation delay, no
	// rejected nonces (the process environment: t.Setenv is not
	// available to parallel tests).
	_ = os.Setenv("PEBBLE_VA_NOSLEEP", "1")
	_ = os.Setenv("PEBBLE_WFE_NONCEREJECT", "0")
	front := &lateHandler{}
	wfeSrv := httptest.NewTLSServer(front)
	t.Cleanup(wfeSrv.Close)
	caFile := filepath.Join(t.TempDir(), "acme-ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: wfeSrv.Certificate().Raw}), 0o600))

	e := newEnv(t, testutil.WithConfig(func(c *config.Static) {
		c.WebAddress = "127.0.0.1:0"
		c.WebTLSAddress = "127.0.0.1:0"
		c.ACMEDirectory = wfeSrv.URL + pebblewfe.DirectoryPath
		c.ACMECACert = caFile
		c.ACMEEmail = "ops@site.test"
	}))
	// The CA validates on the web listener of the last node.
	last := e.srv.Nodes[len(e.srv.Nodes)-1].App
	_, webPort, err := net.SplitHostPort(last.WebAddr())
	require.NoError(t, err)
	httpPort, err := strconv.Atoi(webPort)
	require.NoError(t, err)

	logger := log.New(io.Discard, "", 0)
	dnsAddr := freeUDPAddr(t)
	dns, err := challtestsrv.New(challtestsrv.Config{DNSOneAddrs: []string{dnsAddr}, Log: logger})
	require.NoError(t, err)
	dns.SetDefaultDNSIPv4("127.0.0.1")
	dns.SetDefaultDNSIPv6("")
	dns.Run()
	t.Cleanup(dns.Shutdown)
	db := pebbledb.NewMemoryStore()
	ca := pebbleca.New(logger, db, "", "ecdsa", 0, 1, map[string]pebbleca.Profile{"default": {ValidityPeriod: 90 * 24 * 3600}})
	va := pebbleva.New(logger, httpPort, 0, false, dnsAddr, db)
	wfe := pebblewfe.New(logger, db, va, ca, false, false, 0, 0)
	handler := wfe.Handler()
	front.h.Store(&handler)
	roots := x509.NewCertPool()
	roots.AddCert(ca.GetRootCert(0).Cert)

	e.mkBucket("web")
	e.putTyped("web", "index.html", "secure home", "text/html")
	_, err = e.srv.Nodes[0].App.Distributions().Create(e.ctx, distribution.Distribution{
		ID: "secure", Enabled: true, Aliases: []string{"secure.site.test", "www.secure.site.test"}, Origin: distribution.Origin{Bucket: "web"},
		DefaultBehavior: distribution.Behavior{Access: distribution.AccessPublic, IndexDocument: "index.html"},
		TLS:             distribution.TLS{Certificate: distribution.CertificateAuto},
	})
	require.NoError(t, err)

	// The leader orders the certificate (asked by the change, on this
	// node or through the invalidation of the distribution).
	require.Eventually(t, func() bool {
		c, err := last.Certificates().Get(e.ctx, "acme-secure")
		return err == nil && c.Source == certs.SourceACME
	}, 2*time.Minute, 200*time.Millisecond, "the ACME certificate is obtained")

	for i, n := range e.srv.Nodes {
		for _, host := range []string{"secure.site.test", "www.secure.site.test"} {
			client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
				TLSClientConfig: &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12},
			}}
			var resp response
			require.Eventually(t, func() bool {
				req := newRequest(t, http.MethodGet, "https://"+n.App.WebTLSAddr()+"/", nil)
				req.Host = host
				r, err := client.Do(req)
				if err != nil {
					return false
				}
				defer r.Body.Close()
				body, _ := io.ReadAll(r.Body)
				resp = response{Response: r, Body: body}
				return true
			}, 10*time.Second, 100*time.Millisecond, "node %d serves the certificate of %s", i+1, host)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "secure home", string(resp.Body))
		}
	}
}

// freeUDPAddr returns a loopback address free for UDP and TCP (the mock
// DNS server of the ACME test).
func freeUDPAddr(t *testing.T) string {
	t.Helper()
	for range 20 {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := pc.LocalAddr().String()
		ln, err := net.Listen("tcp", addr)
		_ = pc.Close()
		if err == nil {
			_ = ln.Close()
			return addr
		}
	}
	t.Fatal("no free port")
	return ""
}

// TestDistributionACMEDNS01 obtains the certificate of a distribution
// with an apex and its wildcard from Pebble through the DNS-01 challenge:
// the leader publishes the TXT records with RFC 2136 updates signed with
// TSIG on a nameserver in the test process (the one Pebble queries), waits
// for them on it (NEXO_ACME_DNS_NAMESERVERS) and removes them; every node
// serves the certificate for any name under the wildcard (TDS-10).
//
// covers: S-DST-06
func TestDistributionACMEDNS01(t *testing.T) {
	t.Parallel()
	_ = os.Setenv("PEBBLE_VA_NOSLEEP", "1")
	_ = os.Setenv("PEBBLE_WFE_NONCEREJECT", "0")
	const tsigSecret = "aW50ZWdyYXRpb24tdHNpZy1zZWNyZXQtb2YtbmV4bw=="
	ns := dnstest.Start(t, "wild.test", dnstest.WithTSIG("nexo-acme", tsigSecret))
	front := &lateHandler{}
	wfeSrv := httptest.NewTLSServer(front)
	t.Cleanup(wfeSrv.Close)
	caFile := filepath.Join(t.TempDir(), "acme-ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: wfeSrv.Certificate().Raw}), 0o600))

	e := newEnv(t, testutil.WithConfig(func(c *config.Static) {
		c.WebTLSAddress = "127.0.0.1:0"
		c.ACMEDirectory = wfeSrv.URL + pebblewfe.DirectoryPath
		c.ACMECACert = caFile
		c.ACMEDNSNameservers = ns.Addr
	}))
	logger := log.New(io.Discard, "", 0)
	db := pebbledb.NewMemoryStore()
	ca := pebbleca.New(logger, db, "", "ecdsa", 0, 1, map[string]pebbleca.Profile{"default": {ValidityPeriod: 90 * 24 * 3600}})
	// No HTTP-01 nor TLS-ALPN-01 listener: only the DNS validates.
	va := pebbleva.New(logger, 1, 1, false, ns.Addr, db)
	wfe := pebblewfe.New(logger, db, va, ca, false, false, 0, 0)
	handler := wfe.Handler()
	front.h.Store(&handler)
	roots := x509.NewCertPool()
	roots.AddCert(ca.GetRootCert(0).Cert)

	first := e.srv.Nodes[0].App
	_, err := first.DNSProviders().Create(e.ctx, certs.DNSProviderConfig{
		Name: "bind", Type: certs.DNSProviderRFC2136, PollingInterval: 1,
		RFC2136: &certs.RFC2136Settings{Nameserver: ns.Addr, TSIGKeyName: "nexo-acme"},
	}, certs.DNSCredentials{TSIGSecret: tsigSecret})
	require.NoError(t, err)
	e.mkBucket("previews")
	e.putTyped("previews", "index.html", "preview home", "text/html")
	_, err = first.Distributions().Create(e.ctx, distribution.Distribution{
		ID: "previews", Enabled: true, Aliases: []string{"wild.test", "*.wild.test"}, Origin: distribution.Origin{Bucket: "previews"},
		DefaultBehavior: distribution.Behavior{Access: distribution.AccessPublic, IndexDocument: "index.html"},
		TLS:             distribution.TLS{Certificate: distribution.CertificateAuto, Challenge: distribution.ChallengeDNS01, DNSProvider: "bind"},
	})
	require.NoError(t, err)

	last := e.srv.Nodes[len(e.srv.Nodes)-1].App
	require.Eventually(t, func() bool {
		c, err := last.Certificates().Get(e.ctx, "acme-previews")
		return err == nil && c.Source == certs.SourceACME
	}, 2*time.Minute, 200*time.Millisecond, "the ACME certificate is obtained through DNS-01")
	c, err := last.Certificates().Get(e.ctx, "acme-previews")
	require.NoError(t, err)
	assert.Equal(t, []string{"*.wild.test", "wild.test"}, c.Domains)
	assert.Empty(t, ns.TXT("_acme-challenge.wild.test."), "the challenge records are removed")
	applied, refused := ns.Updates()
	assert.GreaterOrEqual(t, applied, 4, "two values published and removed")
	assert.Zero(t, refused)

	for i, n := range e.srv.Nodes {
		for _, host := range []string{"wild.test", "pr-42.wild.test"} {
			client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
				TLSClientConfig: &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12},
			}}
			var resp response
			require.Eventually(t, func() bool {
				req := newRequest(t, http.MethodGet, "https://"+n.App.WebTLSAddr()+"/", nil)
				req.Host = host
				r, err := client.Do(req)
				if err != nil {
					return false
				}
				defer r.Body.Close()
				body, _ := io.ReadAll(r.Body)
				resp = response{Response: r, Body: body}
				return true
			}, 10*time.Second, 100*time.Millisecond, "node %d serves the wildcard certificate for %s", i+1, host)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "preview home", string(resp.Body))
		}
	}
}
