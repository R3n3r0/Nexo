package nexoadmin_test

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

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestDistributionKeepsUnknownMembers: a read-modify-write never drops the
// members a newer server stores.
func TestDistributionKeepsUnknownMembers(t *testing.T) {
	in := `{"id":"site","enabled":true,"aliases":["a.test"],"origin":{"bucket":"web"},"cache":{"policy":"standard"},` +
		`"defaultBehavior":{"access":"public","cache":{"cacheControl":"no-store","ttl":{"default":"1h"}},"future":1},` +
		`"behaviors":[{"pathPattern":{"type":"prefix","pattern":"/a/"},"other":true}],"headers":{},"restrictions":{},"tls":{},"logging":{}}`
	var d nexoadmin.Distribution
	require.NoError(t, json.Unmarshal([]byte(in), &d))
	assert.Equal(t, "no-store", d.DefaultBehavior.Cache.CacheControl)
	assert.Contains(t, d.Extra, "cache")
	d.Aliases = append(d.Aliases, "b.test")
	d.DefaultBehavior.Cache.CacheControl = "public"
	out, err := json.Marshal(d)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(out, &m))
	assert.Equal(t, map[string]any{"policy": "standard"}, m["cache"])
	def := m["defaultBehavior"].(map[string]any)
	assert.Equal(t, float64(1), def["future"])
	assert.Equal(t, map[string]any{"cacheControl": "public", "ttl": map[string]any{"default": "1h"}}, def["cache"])
	assert.Equal(t, true, m["behaviors"].([]any)[0].(map[string]any)["other"])
	assert.Equal(t, []any{"a.test", "b.test"}, m["aliases"])
}

func selfSignedCert(t *testing.T, names ...string) (string, string) {
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

// get sends an anonymous GET to the S3 listener with a Host header.
func (h *harness) get(host, pathAndQuery string) (int, string) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.S3URL+pathAndQuery, nil)
	require.NoError(h.t, err)
	if host != "" {
		req.Host = host
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(h.t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	return resp.StatusCode, string(data)
}

// TestAccessRulesAgainstServer drives the access rules methods against a
// real server, every exchange validated against api/openapi.yaml.
func TestAccessRulesAgainstServer(t *testing.T) {
	h := newHarness(t)
	ctx, c := h.ctx, h.root
	_, err := c.CreateBucket(ctx, nexoadmin.CreateBucketRequest{Name: "media"})
	require.NoError(t, err)
	h.put("media", "img/2026/a.jpg", "jpeg")

	cur, err := c.GetAccessRules(ctx, "media")
	require.NoError(t, err)
	assert.Empty(t, cur.Rules)
	rules := []nexoadmin.BucketAccessRule{{
		ID: "img-pub", Priority: 10, Effect: nexoadmin.EffectAllow, Principals: []string{"*"}, Actions: []string{"read"},
		Match: &nexoadmin.KeyPattern{Type: nexoadmin.PatternRegex, Pattern: `^img/[0-9]{4}/.*\.jpe?g$`},
	}}
	set, err := c.SetAccessRules(ctx, "media", rules, cur.ETag)
	require.NoError(t, err)
	assert.Equal(t, rules, set.Rules)
	_, err = c.SetAccessRules(ctx, "media", nil, cur.ETag)
	assert.True(t, nexoadmin.IsPreconditionFailed(err), "%v", err)

	status, _ := h.get("", "/media/img/2026/a.jpg")
	assert.Equal(t, http.StatusOK, status)

	res, err := c.TestAccessRules(ctx, "media", nexoadmin.TestAccessRulesRequest{Keys: []string{"img/2026/a.jpg", "img/a.jpg"}})
	require.NoError(t, err)
	require.Len(t, res.Results, 2)
	assert.True(t, res.Results[0].Allowed)
	assert.Equal(t, "img-pub", res.Results[0].Determinant)
	assert.False(t, res.Results[1].Allowed)

	_, err = c.SetAccessRules(ctx, "media", []nexoadmin.BucketAccessRule{{ID: "bad", Effect: "allow", Principals: []string{"*"}, Actions: []string{"read"},
		Match: &nexoadmin.KeyPattern{Type: nexoadmin.PatternRegex, Pattern: "a(b"}}}, "")
	e := apiError(t, err)
	assert.Equal(t, nexoadmin.CodeInvalidAccessRule, e.Code)
	require.Len(t, e.AccessRuleErrors, 1)
	assert.Equal(t, "match.pattern", e.AccessRuleErrors[0].Field)

	pub, err := c.GetAccessRulesPublic(ctx, "media")
	require.NoError(t, err)
	assert.True(t, pub.Public)
	after, err := c.ApplyAccessPreset(ctx, "media", nexoadmin.AccessPresetRequest{Preset: nexoadmin.PresetPrivate}, "")
	require.NoError(t, err)
	assert.False(t, after.Public)

	bp, err := c.SetBlockPublic(ctx, "", nexoadmin.BlockPublicRequest{Enabled: true, Mode: nexoadmin.BlockModeIgnore})
	require.NoError(t, err)
	assert.Equal(t, nexoadmin.BlockModeIgnore, bp.Mode)
	bucketBP, err := c.SetBlockPublic(ctx, "media", nexoadmin.BlockPublicRequest{Enabled: true})
	require.NoError(t, err)
	assert.True(t, bucketBP.Enabled)
	got, err := c.GetBlockPublic(ctx, "")
	require.NoError(t, err)
	assert.True(t, got.Effective)
	_, err = c.SetBlockPublic(ctx, "", nexoadmin.BlockPublicRequest{})
	require.NoError(t, err)
	t.Logf("operations exercised: %v", h.contract.operations())
}

// TestDistributionsAgainstServer drives the distribution, key group,
// routing and certificate methods against a real server, every exchange
// validated against api/openapi.yaml.
func TestDistributionsAgainstServer(t *testing.T) {
	h := newHarness(t)
	ctx, c := h.ctx, h.root
	_, err := c.CreateBucket(ctx, nexoadmin.CreateBucketRequest{Name: "web"})
	require.NoError(t, err)
	h.put("web", "index.html", "home")
	h.put("web", "dl/v1.zip", "zip")

	kg, err := c.CreateKeyGroup(ctx, nexoadmin.CreateKeyGroupRequest{ID: "rel", GenerateKey: &nexoadmin.GenerateKeyRequest{}})
	require.NoError(t, err)
	require.NotNil(t, kg.Key)
	added, err := c.AddKeyGroupKey(ctx, "rel", nexoadmin.AddKeyRequest{Algorithm: nexoadmin.KeyRSA, Bits: 2048, Comment: "rotation"})
	require.NoError(t, err)
	assert.Contains(t, added.PrivateKeyPEM, "PRIVATE KEY")
	groups, err := c.ListKeyGroups(ctx)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Len(t, groups[0].Keys, 2)

	d, err := c.CreateDistribution(ctx, nexoadmin.Distribution{
		ID: "site", Enabled: true, Aliases: []string{"www.site.test", "site.test"}, Origin: nexoadmin.Origin{Bucket: "web"},
		DefaultBehavior: nexoadmin.Behavior{Access: nexoadmin.AccessPublic, IndexDocument: "index.html"},
		Behaviors: []nexoadmin.Behavior{{PathPattern: &nexoadmin.KeyPattern{Type: nexoadmin.PatternPrefix, Pattern: "/dl/"},
			Access: nexoadmin.AccessSigned, Signing: &nexoadmin.Signing{KeyGroup: "rel"}}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, d.ETag)
	assert.Equal(t, nexoadmin.ModeWebsite, d.Mode)
	for _, host := range d.Aliases {
		status, body := h.get(host, "/")
		assert.Equal(t, http.StatusOK, status, host)
		assert.Equal(t, "home", body)
	}

	_, err = c.CreateDistribution(ctx, nexoadmin.Distribution{ID: "dup", Aliases: []string{"site.test"}, Origin: nexoadmin.Origin{Bucket: "web"}})
	assert.Equal(t, nexoadmin.CodeAliasConflict, apiError(t, err).Code)

	d.Aliases = append(d.Aliases, "*.preview.site.test")
	updated, err := c.UpdateDistribution(ctx, *d, d.ETag)
	require.NoError(t, err)
	assert.Len(t, updated.Aliases, 3)
	_, err = c.UpdateDistribution(ctx, *d, d.ETag)
	assert.True(t, nexoadmin.IsPreconditionFailed(err))
	route, err := c.ResolveHost(ctx, "pr-1.preview.site.test")
	require.NoError(t, err)
	assert.Equal(t, "site", route.Distribution)
	assert.Equal(t, "*.preview.site.test", route.Alias)

	tr, err := c.TestDistribution(ctx, "site", nexoadmin.DistributionTestRequest{URL: "/dl/v1.zip"})
	require.NoError(t, err)
	assert.Equal(t, nexoadmin.AccessSigned, tr.Behavior.Access)

	// Both keys of the group sign; the second one is found from its key.
	for _, key := range []string{kg.Key.PrivateKeyPEM, added.PrivateKeyPEM} {
		signed, err := c.SignDistributionURL(ctx, "site", nexoadmin.SignRequest{URL: "/dl/v1.zip", Scheme: "http", PrivateKeyPEM: key, ExpiresIn: 120})
		require.NoError(t, err)
		status, body := h.get("www.site.test", strings.TrimPrefix(signed.URL, "http://www.site.test"))
		assert.Equal(t, http.StatusOK, status, body)
	}
	expired := time.Now().Add(-time.Minute)
	_, err = c.SignDistributionURL(ctx, "site", nexoadmin.SignRequest{URL: "/dl/v1.zip", PrivateKeyPEM: kg.Key.PrivateKeyPEM, ExpiresAt: &expired})
	assert.Equal(t, http.StatusBadRequest, apiError(t, err).Status)

	rot, err := c.RotateOriginSecret(ctx, "site", nexoadmin.RotateOriginSecretRequest{Header: "X-Proxy-Secret"})
	require.NoError(t, err)
	assert.Equal(t, "X-Proxy-Secret", rot.Header)
	req, err := http.NewRequest(http.MethodGet, h.srv.S3URL+"/", nil)
	require.NoError(t, err)
	req.Host = "site.test"
	req.Header.Set("X-Proxy-Secret", rot.Secret)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the proxy secret admits the request")

	chain, key := selfSignedCert(t, "www.site.test")
	cert, err := c.UploadCertificate(ctx, nexoadmin.UploadCertificateRequest{ID: "www", CertificatePEM: chain, PrivateKeyPEM: key})
	require.NoError(t, err)
	assert.Equal(t, nexoadmin.CertificateValid, cert.Status)
	list, err := c.ListCertificates(ctx)
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	_, err = c.GetCertificate(ctx, "nope")
	assert.Equal(t, nexoadmin.CodeCertificateNotFound, apiError(t, err).Code)
	require.NoError(t, c.DeleteCertificate(ctx, "www"))

	require.NoError(t, c.DeleteDistribution(ctx, "site"))
	_, err = c.GetDistribution(ctx, "site")
	assert.True(t, nexoadmin.IsNotFound(err))
	require.NoError(t, c.DeleteKeyGroupKey(ctx, "rel", added.PublicKey.ID))
	g, err := c.GetKeyGroup(ctx, "rel")
	require.NoError(t, err)
	assert.Len(t, g.Keys, 1)
	require.NoError(t, c.DeleteKeyGroup(ctx, "rel"))
	t.Logf("operations exercised: %v", h.contract.operations())
}
