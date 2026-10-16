package nexoadmin_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/internal/testutil/dnstest"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestDNSProviders drives the DNS providers of the DNS-01 challenge
// through the client, every exchange checked against the OpenAPI
// document: an RFC 2136 provider on an in-process nameserver, tested, used
// by a wildcard distribution and kept while in use.
func TestDNSProviders(t *testing.T) {
	const secret = "c2VjcmV0LXRzaWcta2V5LW9mLXRoZS10ZXN0cy0wMTIzNDU2Nzg5"
	ctx := context.Background()
	ns := dnstest.Start(t, "site.test", dnstest.WithTSIG("nexo-acme", secret))
	srv := testutil.StartServer(t, testutil.WithConfig(func(c *config.Static) {
		c.ACMEDNSNameservers = ns.Addr
		c.ACMEDirectory = "http://127.0.0.1:1/directory"
	}))
	contract := newContractTransport(t, srv.ConsoleURL)
	c, err := nexoadmin.New(nexoadmin.Config{Endpoint: srv.ConsoleURL, AccessKey: srv.RootUser, SecretKey: srv.RootPassword,
		Region: srv.Config.Region, HTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: contract}})
	require.NoError(t, err)

	req := nexoadmin.DNSProviderRequest{Name: "bind", Type: nexoadmin.DNSProviderRFC2136, PollingInterval: 1,
		RFC2136: &nexoadmin.RFC2136Settings{Nameserver: ns.Addr, TSIGKeyName: "nexo-acme"}, Credentials: nexoadmin.DNSCredentials{TSIGSecret: secret}}
	assert.NotContains(t, fmt.Sprintf("%v %+v %#v", req.Credentials, req, req), secret, "the credentials are not formatted")
	p, err := c.CreateDNSProvider(ctx, req)
	require.NoError(t, err)
	assert.True(t, p.HasCredentials)
	_, err = c.CreateDNSProvider(ctx, req)
	assert.Equal(t, nexoadmin.CodeDNSProviderExists, apiError(t, err).Code)
	_, err = c.CreateDNSProvider(ctx, nexoadmin.DNSProviderRequest{Name: "cf", Type: nexoadmin.DNSProviderCloudflare})
	assert.Equal(t, nexoadmin.CodeInvalidDNSProvider, apiError(t, err).Code)

	res, err := c.TestDNSProvider(ctx, "bind", "site.test")
	require.NoError(t, err)
	assert.True(t, res.OK, res.Error)

	req.Comment, req.Credentials = "primary", nexoadmin.DNSCredentials{}
	p, err = c.UpdateDNSProvider(ctx, "bind", req)
	require.NoError(t, err)
	assert.Equal(t, "primary", p.Comment)
	assert.True(t, p.HasCredentials, "kept")

	_, err = c.CreateBucket(ctx, nexoadmin.CreateBucketRequest{Name: "web"})
	require.NoError(t, err)
	d, err := c.CreateDistribution(ctx, nexoadmin.Distribution{ID: "preview", Enabled: true, Aliases: []string{"*.preview.site.test"},
		Origin: nexoadmin.Origin{Bucket: "web"},
		TLS:    nexoadmin.DistributionTLS{Certificate: nexoadmin.CertificateAuto, Challenge: nexoadmin.ChallengeDNS01, DNSProvider: "bind"}})
	require.NoError(t, err)
	assert.Equal(t, "bind", d.TLS.DNSProvider)
	list, err := c.ListDNSProviders(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, []string{"preview"}, list[0].UsedBy)
	certs, err := c.ListCertificates(ctx)
	require.NoError(t, err)
	require.Len(t, certs.ACME.Managed, 1)
	assert.Equal(t, nexoadmin.ChallengeDNS01, certs.ACME.Managed[0].Challenge)
	err = c.DeleteDNSProvider(ctx, "bind")
	assert.Equal(t, nexoadmin.CodeDNSProviderInUse, apiError(t, err).Code)

	require.NoError(t, c.DeleteDistribution(ctx, "preview"))
	require.NoError(t, c.DeleteDNSProvider(ctx, "bind"))
	_, err = c.GetDNSProvider(ctx, "bind")
	assert.Equal(t, nexoadmin.CodeDNSProviderNotFound, apiError(t, err).Code)
	t.Logf("operations exercised: %v", contract.operations())
}
