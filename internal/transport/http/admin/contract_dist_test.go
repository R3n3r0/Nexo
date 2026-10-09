package admin_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/accessrule"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

// fetch sends an anonymous GET to the S3 listener with the Host header
// host ("" keeps the listener's) and returns the status and the body.
func fetch(t *testing.T, s3URL, host, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s3URL+path, nil)
	require.NoError(t, err)
	if host != "" {
		req.Host = host
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(data)
}

// callIfMatch sends a JSON request with an If-Match header.
func (c *console) callIfMatch(method, path, etag string, body any, status int) []byte {
	c.t.Helper()
	data, err := json.Marshal(body)
	require.NoError(c.t, err)
	resp := c.request(method, path, strings.NewReader(string(data)), http.Header{"If-Match": {etag}, "Content-Type": {"application/json"}})
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	require.Equal(c.t, status, resp.StatusCode, "%s %s: %s", method, path, out)
	return out
}

// TestAccessRulesContract exercises the access rules endpoints against a
// real server, every exchange validated against api/openapi.yaml: atomic
// replacement with If-Match, errors located in the pattern, the tester
// agreeing with real anonymous requests, the presets and the block public
// access (TAR-02, TAR-08, TCL-A-01 on the API).
func TestAccessRulesContract(t *testing.T) {
	srv := testutil.StartServer(t)
	c := newConsole(t, srv, srv.RootUser, srv.RootPassword)
	c.call(http.MethodPost, "/buckets", map[string]any{"name": "media"}, http.StatusCreated)
	for _, key := range []string{"img/2026/a.jpg", "private/x.jpg"} {
		c.call(http.MethodPost, "/buckets/media/objects/upload"+q(key), strings.NewReader("data of "+key), http.StatusCreated)
	}

	empty := decodeAs[admin.AccessRulesResponse](t, c.call(http.MethodGet, "/buckets/media/access-rules", nil, http.StatusOK))
	assert.Empty(t, empty.Rules)
	require.NotEmpty(t, empty.ETag)

	rules := []map[string]any{{
		"id": "img-pub", "priority": 10, "effect": "allow", "principals": []string{"*"}, "actions": []string{"read"},
		"match": map[string]any{"type": "regex", "pattern": `^img/[0-9]{4}/.*\.(jpe?g|png)$`, "caseInsensitive": true},
	}}
	set := decodeAs[admin.AccessRulesResponse](t, c.callIfMatch(http.MethodPut, "/buckets/media/access-rules", `"`+empty.ETag+`"`,
		map[string]any{"rules": rules}, http.StatusOK))
	assert.True(t, set.Public)
	assert.NotEqual(t, empty.ETag, set.ETag)
	// A stale ETag: the rules changed meanwhile.
	c.callIfMatch(http.MethodPut, "/buckets/media/access-rules", empty.ETag, map[string]any{"rules": []any{}}, http.StatusPreconditionFailed)

	status, body := fetch(t, srv.S3URL, "", "/media/img/2026/a.jpg")
	assert.Equal(t, http.StatusOK, status, body)
	status, _ = fetch(t, srv.S3URL, "", "/media/private/x.jpg")
	assert.Equal(t, http.StatusForbidden, status)

	// The tester agrees with the real requests.
	res := decodeAs[accessrule.TestResult](t, c.call(http.MethodPost, "/buckets/media/access-rules/test",
		map[string]any{"keys": []string{"img/2026/a.jpg", "private/x.jpg", "img/2026/../../private/x.jpg"}, "principal": "anonymous", "ip": "1.2.3.4"},
		http.StatusOK))
	require.Len(t, res.Results, 3)
	assert.True(t, res.Results[0].Allowed)
	assert.Equal(t, "img-pub", res.Results[0].Determinant)
	assert.Equal(t, []string{"img-pub"}, res.Results[0].Matched)
	assert.False(t, res.Results[1].Allowed)
	assert.False(t, res.Results[2].Allowed, "no bypass with ..")
	assert.Equal(t, "private/x.jpg", res.Results[2].NormalizedKey)

	// An invalid regex is located.
	data := c.call(http.MethodPut, "/buckets/media/access-rules", map[string]any{"rules": []map[string]any{{
		"id": "bad", "priority": 1, "effect": "allow", "principals": []string{"*"}, "actions": []string{"read"},
		"match": map[string]any{"type": "regex", "pattern": "^img/(unclosed"},
	}}}, http.StatusBadRequest)
	p := decodeAs[accessProblem](t, data)
	assert.Equal(t, "invalid_access_rule", p.Code)
	require.Len(t, p.AccessRuleErrors, 1)
	assert.Equal(t, 1, p.AccessRuleErrors[0].Rule)
	assert.Equal(t, "bad", p.AccessRuleErrors[0].ID)
	assert.Equal(t, "match.pattern", p.AccessRuleErrors[0].Field)

	// Presets.
	pub := decodeAs[accessrule.PublicStatus](t, c.call(http.MethodGet, "/buckets/media/access-rules/public", nil, http.StatusOK))
	assert.True(t, pub.Public)
	assert.Equal(t, accessrule.PresetCustom, pub.Preset)
	c.call(http.MethodPut, "/buckets/media/access-rules/public", map[string]any{"preset": "public-write"}, http.StatusBadRequest)
	afterPreset := decodeAs[admin.AccessRulesResponse](t, c.call(http.MethodPut, "/buckets/media/access-rules/public",
		map[string]any{"preset": "private"}, http.StatusOK))
	assert.False(t, afterPreset.Public)
	status, _ = fetch(t, srv.S3URL, "", "/media/img/2026/a.jpg")
	assert.Equal(t, http.StatusForbidden, status, "private removes the rules for everyone")

	// The bucket list shows the public flag of the access rules.
	c.call(http.MethodPut, "/buckets/media/access-rules/public", map[string]any{"preset": "public-read"}, http.StatusOK)
	bucket := decodeAs[admin.BucketResponse](t, c.call(http.MethodGet, "/buckets/media", nil, http.StatusOK))
	assert.True(t, bucket.Public)

	// Block public access of the cluster, reject mode.
	bp := decodeAs[accessrule.BlockPublic](t, c.call(http.MethodPut, "/access/block-public", map[string]any{"enabled": true, "mode": "reject"}, http.StatusOK))
	assert.True(t, bp.Effective)
	status, _ = fetch(t, srv.S3URL, "", "/media/img/2026/a.jpg")
	assert.Equal(t, http.StatusForbidden, status, "the stored rule for everyone is ignored")
	data = c.call(http.MethodPut, "/buckets/media/access-rules", map[string]any{"rules": rules}, http.StatusForbidden)
	assert.Equal(t, "public_access_blocked", decodeAs[accessProblem](t, data).Code)
	bucketBP := decodeAs[accessrule.BlockPublic](t, c.call(http.MethodGet, "/buckets/media/block-public", nil, http.StatusOK))
	assert.True(t, bucketBP.Cluster)
	assert.False(t, bucketBP.Enabled)
	c.call(http.MethodPut, "/buckets/media/block-public", map[string]any{"enabled": true, "mode": "ignore"}, http.StatusBadRequest)
	c.call(http.MethodPut, "/access/block-public", map[string]any{"enabled": false}, http.StatusOK)
	status, _ = fetch(t, srv.S3URL, "", "/media/img/2026/a.jpg")
	assert.Equal(t, http.StatusOK, status)

	// A user without admin:ManageAccessRules.
	c.call(http.MethodPost, "/users", map[string]any{"accessKey": "ruler", "secretKey": "ruler-secret", "policyNames": []string{"readwrite"}}, http.StatusCreated)
	other := newConsole(t, srv, "ruler", "ruler-secret")
	other.call(http.MethodGet, "/buckets/media/access-rules", nil, http.StatusForbidden)
	other.call(http.MethodPost, "/buckets/media/access-rules/test", map[string]any{"keys": []string{"a"}}, http.StatusForbidden)
	other.call(http.MethodPut, "/access/block-public", map[string]any{"enabled": true}, http.StatusForbidden)
}

// selfSigned returns a self-signed certificate of names and its key, in
// PEM.
func selfSigned(t *testing.T, names ...string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

// TestDistributionsContract exercises the distribution, key group,
// routing and certificate endpoints against a real server, every exchange
// validated against api/openapi.yaml: two domains served, a URL signed by
// the API opens the object, the test of a URL, alias conflicts, If-Match,
// origin secret rotation and certificates in use.
func TestDistributionsContract(t *testing.T) {
	srv := testutil.StartServer(t)
	c := newConsole(t, srv, srv.RootUser, srv.RootPassword)
	c.call(http.MethodPost, "/buckets", map[string]any{"name": "web"}, http.StatusCreated)
	c.call(http.MethodPost, "/buckets/web/objects/upload"+q("index.html"), strings.NewReader("<h1>home</h1>"), http.StatusCreated)
	c.call(http.MethodPost, "/buckets/web/objects/upload"+q("downloads/v1.zip"), strings.NewReader("zip bytes"), http.StatusCreated)

	created := decodeAs[admin.CreateKeyGroupResponse](t, c.call(http.MethodPost, "/keygroups",
		map[string]any{"id": "rel", "comment": "releases", "generateKey": map[string]any{"algorithm": "ed25519"}}, http.StatusCreated))
	require.NotNil(t, created.Key)
	require.Len(t, created.KeyGroup.Keys, 1)
	privateKey := created.Key.PrivateKeyPEM
	assert.Contains(t, privateKey, "PRIVATE KEY")
	keys := decodeAs[admin.KeyListResponse](t, c.call(http.MethodGet, "/keygroups/rel/keys", nil, http.StatusOK))
	require.Len(t, keys.Items, 1)
	assert.NotContains(t, string(c.call(http.MethodGet, "/keygroups/rel", nil, http.StatusOK)), "PRIVATE", "the private key is never stored")

	site := map[string]any{
		"id": "site", "enabled": true, "aliases": []string{"www.site.test", "site.test"}, "origin": map[string]any{"bucket": "web"},
		"defaultBehavior": map[string]any{"access": "public", "indexDocument": "index.html"},
		"behaviors": []map[string]any{{
			"pathPattern": map[string]any{"type": "regex", "pattern": `^/downloads/.*\.zip$`},
			"access":      "signed", "signing": map[string]any{"keyGroup": "rel"},
		}},
	}
	d := decodeAs[distribution.Distribution](t, c.call(http.MethodPost, "/distributions", site, http.StatusCreated))
	require.NotEmpty(t, d.ETag)
	c.call(http.MethodPost, "/distributions", site, http.StatusConflict)
	conflict := map[string]any{"id": "other", "aliases": []string{"site.test"}, "origin": map[string]any{"bucket": "web"}}
	assert.Equal(t, "alias_conflict", decodeAs[accessProblem](t, c.call(http.MethodPost, "/distributions", conflict, http.StatusConflict)).Code)
	list := decodeAs[admin.DistributionListResponse](t, c.call(http.MethodGet, "/distributions", nil, http.StatusOK))
	require.Len(t, list.Items, 1)

	// Both domains serve the site.
	for _, host := range []string{"www.site.test", "site.test"} {
		status, body := fetch(t, srv.S3URL, host, "/")
		assert.Equal(t, http.StatusOK, status, host)
		assert.Equal(t, "<h1>home</h1>", body, host)
	}
	route := decodeAs[admin.RouteResponse](t, c.call(http.MethodGet, "/routing/resolve?host=WWW.site.test:9000", nil, http.StatusOK))
	assert.Equal(t, distribution.RouteDistribution, route.Kind)
	assert.Equal(t, "site", route.Distribution)

	// The test of a URL.
	tr := decodeAs[admin.DistributionTestResponse](t, c.call(http.MethodPost, "/distributions/site/test", map[string]any{"url": "/downloads/v1.zip"}, http.StatusOK))
	assert.Equal(t, 0, tr.BehaviorIndex)
	assert.Equal(t, distribution.AccessSigned, tr.Behavior.Access)
	assert.Equal(t, "downloads/v1.zip", tr.Key)
	c.call(http.MethodPost, "/distributions/site/test", map[string]any{"url": "https://elsewhere.test/"}, http.StatusBadRequest)

	// A signed URL opens the object; tampered, it does not.
	status, _ := fetch(t, srv.S3URL, "www.site.test", "/downloads/v1.zip")
	assert.Equal(t, http.StatusForbidden, status, "signature required")
	signed := decodeAs[admin.SignDistributionResponse](t, c.call(http.MethodPost, "/distributions/site/sign",
		map[string]any{"url": "/downloads/v1.zip", "scheme": "http", "privateKeyPem": privateKey, "expiresIn": 300}, http.StatusOK))
	assert.Equal(t, "rel", signed.KeyGroup)
	assert.Equal(t, keys.Items[0].ID, signed.KeyPairID)
	pathAndQuery := strings.TrimPrefix(signed.URL, "http://www.site.test")
	status, body := fetch(t, srv.S3URL, "www.site.test", pathAndQuery)
	assert.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "zip bytes", body)
	status, _ = fetch(t, srv.S3URL, "www.site.test", strings.Replace(pathAndQuery, "Signature=", "Signature=AA", 1))
	assert.Equal(t, http.StatusForbidden, status)
	cookies := decodeAs[admin.SignDistributionResponse](t, c.call(http.MethodPost, "/distributions/site/sign",
		map[string]any{"url": "/downloads/v1.zip", "privateKeyPem": privateKey, "expiresIn": 300, "cookies": true,
			"resource": "https://www.site.test/downloads/*"}, http.StatusOK))
	assert.Len(t, cookies.Cookies, 3)
	c.call(http.MethodPost, "/distributions/site/sign", map[string]any{"url": "/index.html", "privateKeyPem": privateKey, "expiresIn": 300}, http.StatusBadRequest)

	// Optimistic concurrency.
	got := c.request(http.MethodGet, "/distributions/site", nil, nil)
	etag := got.Header.Get("ETag")
	_ = got.Body.Close()
	require.Equal(t, `"`+d.ETag+`"`, etag)
	site["comment"] = "the site"
	updated := decodeAs[distribution.Distribution](t, c.callIfMatch(http.MethodPut, "/distributions/site", etag, site, http.StatusOK))
	assert.Equal(t, "the site", updated.Comment)
	c.callIfMatch(http.MethodPut, "/distributions/site", etag, site, http.StatusPreconditionFailed)

	// Origin secret.
	rot := decodeAs[admin.RotateOriginSecretResponse](t, c.call(http.MethodPost, "/distributions/site/origin-secret/rotate", nil, http.StatusOK))
	require.NotEmpty(t, rot.Secret)
	assert.Equal(t, "X-Origin-Secret", rot.Header)
	status, _ = fetch(t, srv.S3URL, "www.site.test", "/")
	assert.Equal(t, http.StatusForbidden, status, "the proxy secret is missing")

	// Certificates.
	chain, key := selfSigned(t, "www.site.test", "site.test")
	cert := decodeAs[admin.CertificateResponse](t, c.call(http.MethodPost, "/certificates",
		map[string]any{"id": "site-cert", "certificatePem": chain, "privateKeyPem": key}, http.StatusCreated))
	assert.ElementsMatch(t, []string{"www.site.test", "site.test"}, cert.Domains)
	c.call(http.MethodPost, "/certificates", map[string]any{"certificatePem": chain, "privateKeyPem": "nope"}, http.StatusBadRequest)
	current := decodeAs[distribution.Distribution](t, c.call(http.MethodGet, "/distributions/site", nil, http.StatusOK))
	current.TLS.Certificate = "upload:site-cert"
	c.call(http.MethodPut, "/distributions/site", current, http.StatusOK)
	certs := decodeAs[admin.CertificateListResponse](t, c.call(http.MethodGet, "/certificates", nil, http.StatusOK))
	require.Len(t, certs.Items, 1)
	assert.Equal(t, []string{"site"}, certs.Items[0].UsedBy)
	c.call(http.MethodDelete, "/certificates/site-cert", nil, http.StatusConflict)
	c.call(http.MethodDelete, "/keygroups/rel", nil, http.StatusConflict)

	// A user without the actions of the distributions.
	c.call(http.MethodPost, "/users", map[string]any{"accessKey": "webdev", "secretKey": "webdev-secret", "policyNames": []string{"readwrite"}}, http.StatusCreated)
	other := newConsole(t, srv, "webdev", "webdev-secret")
	other.call(http.MethodGet, "/distributions", nil, http.StatusForbidden)
	other.call(http.MethodPost, "/distributions/site/sign", map[string]any{"url": "/downloads/v1.zip", "privateKeyPem": privateKey, "expiresIn": 60}, http.StatusForbidden)
	other.call(http.MethodGet, "/certificates", nil, http.StatusForbidden)

	c.call(http.MethodDelete, "/distributions/site", nil, http.StatusNoContent)
	c.call(http.MethodGet, "/distributions/site", nil, http.StatusNotFound)
	c.call(http.MethodDelete, "/certificates/site-cert", nil, http.StatusNoContent)
	c.call(http.MethodDelete, "/keygroups/rel/keys/"+keys.Items[0].ID, nil, http.StatusNoContent)
	c.call(http.MethodDelete, "/keygroups/rel", nil, http.StatusNoContent)
}
