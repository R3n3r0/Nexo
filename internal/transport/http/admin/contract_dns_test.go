package admin_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/internal/testutil/dnstest"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

// TestDNSProvidersContract manages the DNS providers of the DNS-01
// challenge against the OpenAPI contract: credentials write-only, a test
// record published through RFC 2136 on an in-process nameserver, the
// wildcard aliases bound to dns-01 and the providers in use kept.
func TestDNSProvidersContract(t *testing.T) {
	const secret = "c2VjcmV0LXRzaWcta2V5LW9mLXRoZS10ZXN0cy0wMTIzNDU2Nzg5"
	ns := dnstest.Start(t, "site.test", dnstest.WithTSIG("nexo-acme", secret))
	srv := testutil.StartServer(t, testutil.WithConfig(func(c *config.Static) {
		c.ACMEDNSNameservers = ns.Addr
		// No CA: the orders the distributions trigger fail fast, offline.
		c.ACMEDirectory = "http://127.0.0.1:1/directory"
	}))
	c := newConsole(t, srv, srv.RootUser, srv.RootPassword)
	c.call(http.MethodPost, "/buckets", map[string]any{"name": "web"}, http.StatusCreated)

	bind := map[string]any{
		"name": "bind", "type": "rfc2136", "pollingInterval": 1,
		"rfc2136":     map[string]any{"nameserver": ns.Addr, "tsigKeyName": "nexo-acme"},
		"credentials": map[string]any{"tsigSecret": secret},
	}
	created := decodeAs[admin.DNSProviderResponse](t, c.call(http.MethodPost, "/dns-providers", bind, http.StatusCreated))
	assert.True(t, created.HasCredentials)
	assert.Equal(t, "hmac-sha256", created.RFC2136.TSIGAlgorithm)
	assert.Equal(t, "dns_provider_exists", decodeAs[accessProblem](t, c.call(http.MethodPost, "/dns-providers", bind, http.StatusConflict)).Code)
	bad := map[string]any{"name": "cf", "type": "cloudflare"}
	assert.Equal(t, "invalid_dns_provider", decodeAs[accessProblem](t, c.call(http.MethodPost, "/dns-providers", bad, http.StatusBadRequest)).Code)
	for _, path := range []string{"/dns-providers", "/dns-providers/bind"} {
		assert.NotContains(t, string(c.call(http.MethodGet, path, nil, http.StatusOK)), secret, "credentials are never returned")
	}
	assert.Equal(t, "dns_provider_not_found", decodeAs[accessProblem](t, c.call(http.MethodGet, "/dns-providers/nope", nil, http.StatusNotFound)).Code)

	res := decodeAs[map[string]any](t, c.call(http.MethodPost, "/dns-providers/bind/test", map[string]any{"domain": "*.site.test"}, http.StatusOK))
	assert.Equal(t, true, res["ok"], res["error"])
	assert.Equal(t, "_acme-challenge.site.test", res["record"])
	assert.Empty(t, ns.TXT("_acme-challenge.site.test"))

	// An update without credentials keeps them.
	delete(bind, "credentials")
	bind["comment"] = "primary"
	updated := decodeAs[admin.DNSProviderResponse](t, c.call(http.MethodPut, "/dns-providers/bind", bind, http.StatusOK))
	assert.True(t, updated.HasCredentials)
	assert.Equal(t, "primary", updated.Comment)

	// A wildcard alias needs dns-01 with an existing provider.
	site := map[string]any{
		"id": "preview", "enabled": true, "aliases": []string{"*.preview.site.test"}, "origin": map[string]any{"bucket": "web"},
		"tls": map[string]any{"certificate": "auto"},
	}
	p := decodeAs[accessProblem](t, c.call(http.MethodPost, "/distributions", site, http.StatusBadRequest))
	assert.Equal(t, "invalid_distribution", p.Code)
	assert.Contains(t, p.Detail, "dns-01")
	site["tls"] = map[string]any{"certificate": "auto", "challenge": "dns-01", "dnsProvider": "nope"}
	p = decodeAs[accessProblem](t, c.call(http.MethodPost, "/distributions", site, http.StatusBadRequest))
	assert.Contains(t, p.Detail, "does not exist")
	site["tls"] = map[string]any{"certificate": "auto", "challenge": "dns-01", "dnsProvider": "bind"}
	d := decodeAs[distribution.Distribution](t, c.call(http.MethodPost, "/distributions", site, http.StatusCreated))
	assert.Equal(t, distribution.TLS{Certificate: "auto", Challenge: "dns-01", DNSProvider: "bind"}, d.TLS)

	list := decodeAs[admin.DNSProviderListResponse](t, c.call(http.MethodGet, "/dns-providers", nil, http.StatusOK))
	require.Len(t, list.Items, 1)
	assert.Equal(t, []string{"preview"}, list.Items[0].UsedBy)
	certs := decodeAs[admin.CertificateListResponse](t, c.call(http.MethodGet, "/certificates", nil, http.StatusOK))
	require.Len(t, certs.ACME.Managed, 1)
	assert.Equal(t, "dns-01", certs.ACME.Managed[0].Challenge)
	assert.Equal(t, "bind", certs.ACME.Managed[0].DNSProvider)
	assert.Equal(t, "dns_provider_in_use", decodeAs[accessProblem](t, c.call(http.MethodDelete, "/dns-providers/bind", nil, http.StatusConflict)).Code)

	// A user without admin:ManageCertificates.
	c.call(http.MethodPost, "/users", map[string]any{"accessKey": "webdev", "secretKey": "webdev-secret", "policyNames": []string{"readwrite"}}, http.StatusCreated)
	other := newConsole(t, srv, "webdev", "webdev-secret")
	other.call(http.MethodGet, "/dns-providers", nil, http.StatusForbidden)
	other.call(http.MethodPost, "/dns-providers/bind/test", map[string]any{"domain": "site.test"}, http.StatusForbidden)

	c.call(http.MethodDelete, "/distributions/preview", nil, http.StatusNoContent)
	c.call(http.MethodDelete, "/dns-providers/bind", nil, http.StatusNoContent)
	assert.False(t, strings.Contains(string(c.call(http.MethodGet, "/dns-providers", nil, http.StatusOK)), "bind"))
}
