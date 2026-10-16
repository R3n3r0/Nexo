package certs_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/letsencrypt/challtestsrv"
	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/certs/dns01"
	certsmock "github.com/r3n3r0/nexo/internal/service/certs/mock"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/testutil/dnstest"
)

const (
	tsigKey    = "nexo-acme."
	tsigSecret = "c2VjcmV0LXRzaWcta2V5LW9mLXRoZS10ZXN0cy0wMTIzNDU2Nzg5"
)

// fakeDNS is a DNS provider publishing the records in the mock DNS server
// of the test CA; it records what it was asked.
type fakeDNS struct {
	srv *challtestsrv.ChallSrv
	mu  sync.Mutex
	// live are the values published and not removed yet.
	live    map[string][]string
	present []string
}

var _ dns01.Provider = (*fakeDNS)(nil)

func (f *fakeDNS) Present(_ context.Context, fqdn, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[fqdn] = append(f.live[fqdn], value)
	f.present = append(f.present, fqdn)
	f.srv.AddDNSOneChallenge(fqdn, value)
	return nil
}

func (f *fakeDNS) CleanUp(_ context.Context, fqdn, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[fqdn] = slices.DeleteFunc(f.live[fqdn], func(v string) bool { return v == value })
	f.srv.DeleteDNSOneChallenge(fqdn)
	for _, v := range f.live[fqdn] {
		f.srv.AddDNSOneChallenge(fqdn, v)
	}
	if len(f.live[fqdn]) == 0 {
		delete(f.live, fqdn)
	}
	return nil
}

func (f *fakeDNS) Propagation() (time.Duration, time.Duration) {
	return 10 * time.Second, 20 * time.Millisecond
}

// TestACMEDNS01 obtains from Pebble a certificate of an apex and its
// wildcard through the DNS-01 challenge, the records published by a DNS
// provider and checked on the nameserver before the validation (TDS-10).
//
// covers: S-DST-06
func TestACMEDNS01(t *testing.T) {
	if testing.Short() {
		t.Skip("ACME against Pebble skipped with -short")
	}
	t.Setenv("PEBBLE_VA_NOSLEEP", "1")
	t.Setenv("PEBBLE_WFE_NONCEREJECT", "0")
	t.Setenv("PEBBLE_AUTHZREUSE", "0")
	ctx := context.Background()
	// Nobody answers HTTP-01 nor TLS-ALPN-01: only the DNS can validate.
	p := startPebbleDNS(t, 1, 1)
	fake := &fakeDNS{srv: p.dns, live: map[string][]string{}}

	providers := certsmock.NewDNSProviderSource(t)
	providers.EXPECT().Provider(tmock.Anything, "fake").Return(fake, nil).Maybe()
	providers.EXPECT().Provider(tmock.Anything, "gone").Return(nil, certs.ErrDNSProviderNotFound).Maybe()
	wanted := []certs.ManagedDomains{{ID: "acme-wild", Domains: []string{"site.test", "*.site.test"}, Challenge: certs.ChallengeDNS01, DNSProvider: "fake"}}
	domains := certsmock.NewDomainSource(t)
	domains.EXPECT().ACMEDomains(tmock.Anything).RunAndReturn(func(context.Context) ([]certs.ManagedDomains, error) { return wanted, nil }).Maybe()

	sealer, err := certs.NewSealer(certs.DeriveKey("root", "root-secret", "deployment"))
	require.NoError(t, err)
	docs := &memDocs{docs: map[string][]byte{}}
	var elected atomic.Bool
	elected.Store(true)
	svc, err := certs.New(certs.Deps{
		Store: newStore(t, docs), Locks: lock.NewLocal(lock.Deps{}), Notifier: cluster.NewLocalNotifier(cluster.NotifierDeps{}),
		Leader: switchableLeader(t, &elected), Sealer: sealer, Domains: domains, DNSProviders: providers,
		Propagation: dns01.NewChecker(dns01.WithNameservers(p.dnsAddr)), Clock: clock.NewReal(), Logger: slog.New(slog.DiscardHandler),
	}, certs.WithACMEDirectory(p.directory), certs.WithHTTPClient(p.client), certs.WithRetryInterval(time.Hour))
	require.NoError(t, err)

	require.NoError(t, svc.Reconcile(ctx))
	c, err := svc.Get(ctx, "acme-wild")
	require.NoError(t, err)
	assert.Equal(t, []string{"*.site.test", "site.test"}, c.Domains, "the wildcard is in the certificate")
	for _, name := range []string{"site.test", "www.site.test", "preview.site.test"} {
		verifyServed(t, svc, name, p.roots)
	}
	fake.mu.Lock()
	assert.Equal(t, []string{"_acme-challenge.site.test.", "_acme-challenge.site.test."}, fake.present, "one record name for the apex and the wildcard")
	assert.Empty(t, fake.live, "the records are removed after the validation")
	fake.mu.Unlock()

	// A distribution naming a provider that no longer exists: the order
	// fails with the reason, and is retried later only.
	wanted = append(wanted, certs.ManagedDomains{ID: "acme-gone", Domains: []string{"*.other.test"}, Challenge: certs.ChallengeDNS01, DNSProvider: "gone"})
	err = svc.Reconcile(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "acme-gone")
	assert.Contains(t, err.Error(), "DNS provider gone")
	require.NoError(t, svc.Reconcile(ctx), "within the retry interval")
}

// TestACMEWildcardNeedsDNS01 leaves the wildcard out of an order with
// another challenge (the distributions refuse that configuration; an
// older one is still served for its other names).
func TestACMEWildcardNeedsDNS01(t *testing.T) {
	providers := certsmock.NewDNSProviderSource(t)
	domains := certsmock.NewDomainSource(t)
	domains.EXPECT().ACMEDomains(tmock.Anything).Return([]certs.ManagedDomains{{ID: "acme-x", Domains: []string{"*.site.test"}, Challenge: certs.ChallengeHTTP01}}, nil)
	nodes := deploymentWithDNS(t, domains, providers)
	require.NoError(t, nodes.Reconcile(context.Background()), "nothing to order: the provider is never asked")
}

func deploymentWithDNS(t *testing.T, domains certs.DomainSource, providers certs.DNSProviderSource) certs.Service {
	t.Helper()
	sealer, err := certs.NewSealer(certs.DeriveKey("root", "root-secret", "deployment"))
	require.NoError(t, err)
	svc, err := certs.New(certs.Deps{
		Store: newStore(t, &memDocs{docs: map[string][]byte{}}), Locks: lock.NewLocal(lock.Deps{}),
		Notifier: cluster.NewLocalNotifier(cluster.NotifierDeps{}), Leader: leader(t, true), Sealer: sealer, Domains: domains,
		DNSProviders: providers, Logger: slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	return svc
}

// newDNSProviders returns a registry on docs, its test checker querying
// the nameserver addr.
func newDNSProviders(t *testing.T, docs *memDocs, addr string, logs *bytes.Buffer) certs.DNSProviders {
	t.Helper()
	sealer, err := certs.NewSealer(certs.DeriveKey("root", "root-secret", "deployment"))
	require.NoError(t, err)
	logger := slog.New(slog.DiscardHandler)
	if logs != nil {
		logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	r, err := certs.NewDNSProviders(certs.DNSProvidersDeps{
		Store: newStore(t, docs), Locks: lock.NewLocal(lock.Deps{}), Sealer: sealer,
		Propagation: dns01.NewChecker(dns01.WithNameservers(addr)), Logger: logger,
	})
	require.NoError(t, err)
	return r
}

// TestDNSProviders stores the providers with their credentials sealed,
// returns them without the credentials and keeps the stored credentials
// an update leaves empty.
func TestDNSProviders(t *testing.T) {
	ctx := context.Background()
	docs := &memDocs{docs: map[string][]byte{}}
	var logs bytes.Buffer
	r := newDNSProviders(t, docs, "127.0.0.1:1", &logs)

	cf, err := r.Create(ctx, certs.DNSProviderConfig{Name: "cf", Type: "Cloudflare", Cloudflare: &certs.CloudflareSettings{ZoneID: "z1"},
		Route53: &certs.Route53Settings{Region: "dropped"}}, certs.DNSCredentials{APIToken: "cf-token-secret", TSIGSecret: "dropped-secret"})
	require.NoError(t, err)
	assert.Equal(t, certs.DNSProviderCloudflare, cf.Type)
	assert.True(t, cf.HasCredentials)
	assert.Nil(t, cf.Route53, "only the settings of the type are kept")
	_, err = r.Create(ctx, certs.DNSProviderConfig{Name: "cf", Type: "cloudflare"}, certs.DNSCredentials{APIToken: "x"})
	require.ErrorIs(t, err, certs.ErrDNSProviderExists)

	_, err = r.Create(ctx, certs.DNSProviderConfig{Name: "r53", Type: "route53", Route53: &certs.Route53Settings{AccessKeyID: "AKID"}},
		certs.DNSCredentials{SecretAccessKey: "r53-secret"})
	require.NoError(t, err)
	_, err = r.Create(ctx, certs.DNSProviderConfig{Name: "bind", Type: "rfc2136", RFC2136: &certs.RFC2136Settings{Nameserver: "ns1.site.test", TSIGKeyName: "k"}},
		certs.DNSCredentials{TSIGSecret: tsigSecret})
	require.NoError(t, err)

	stored := docs.all()
	for _, secret := range []string{"cf-token-secret", "r53-secret", tsigSecret, "dropped-secret"} {
		assert.NotContains(t, stored, secret, "credentials are sealed")
		assert.NotContains(t, logs.String(), secret, "credentials are never logged")
	}

	list, err := r.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{"bind", "cf", "r53"}, []string{list[0].Name, list[1].Name, list[2].Name})
	assert.Equal(t, dns01.TSIGHmacSHA256, list[0].RFC2136.TSIGAlgorithm, "default algorithm")
	for _, p := range list {
		assert.True(t, p.HasCredentials, p.Name)
	}

	// An update without credentials keeps them; the settings change.
	up, err := r.Update(ctx, certs.DNSProviderConfig{Name: "cf", Type: "cloudflare", Comment: "main zone"}, certs.DNSCredentials{})
	require.NoError(t, err)
	assert.True(t, up.HasCredentials)
	assert.Empty(t, up.Cloudflare.ZoneID)
	assert.Equal(t, "main zone", up.Comment)
	assert.Equal(t, cf.CreatedAt, up.CreatedAt)
	// Changing the type needs the credentials of the new type.
	_, err = r.Update(ctx, certs.DNSProviderConfig{Name: "cf", Type: "rfc2136", RFC2136: &certs.RFC2136Settings{Nameserver: "ns", TSIGKeyName: "k"}}, certs.DNSCredentials{})
	require.ErrorIs(t, err, certs.ErrInvalidDNSProvider)
	// Route 53 without its access key: the ambient credentials, the
	// stored secret is dropped.
	amb, err := r.Update(ctx, certs.DNSProviderConfig{Name: "r53", Type: "route53"}, certs.DNSCredentials{})
	require.NoError(t, err)
	assert.False(t, amb.HasCredentials)
	_, err = r.Update(ctx, certs.DNSProviderConfig{Name: "missing", Type: "route53"}, certs.DNSCredentials{})
	require.ErrorIs(t, err, certs.ErrDNSProviderNotFound)

	p, err := r.Provider(ctx, "bind")
	require.NoError(t, err)
	assert.NotNil(t, p)

	require.NoError(t, r.Delete(ctx, "r53"))
	_, err = r.Get(ctx, "r53")
	require.ErrorIs(t, err, certs.ErrDNSProviderNotFound)
	require.ErrorIs(t, r.Delete(ctx, "r53"), certs.ErrDNSProviderNotFound)
	_, err = r.Get(ctx, "../etc")
	require.ErrorIs(t, err, certs.ErrDNSProviderNotFound)
}

func TestDNSProvidersValidation(t *testing.T) {
	ctx := context.Background()
	r := newDNSProviders(t, &memDocs{docs: map[string][]byte{}}, "127.0.0.1:1", nil)
	for name, in := range map[string]struct {
		cfg   certs.DNSProviderConfig
		creds certs.DNSCredentials
	}{
		"bad name":             {certs.DNSProviderConfig{Name: "Bad Name", Type: "cloudflare"}, certs.DNSCredentials{APIToken: "t"}},
		"unknown type":         {certs.DNSProviderConfig{Name: "x", Type: "gandi"}, certs.DNSCredentials{}},
		"cloudflare no token":  {certs.DNSProviderConfig{Name: "x", Type: "cloudflare"}, certs.DNSCredentials{}},
		"route53 key alone":    {certs.DNSProviderConfig{Name: "x", Type: "route53", Route53: &certs.Route53Settings{AccessKeyID: "A"}}, certs.DNSCredentials{}},
		"route53 secret alone": {certs.DNSProviderConfig{Name: "x", Type: "route53"}, certs.DNSCredentials{SecretAccessKey: "s"}},
		"rfc2136 no server":    {certs.DNSProviderConfig{Name: "x", Type: "rfc2136"}, certs.DNSCredentials{}},
		"rfc2136 bad secret": {certs.DNSProviderConfig{Name: "x", Type: "rfc2136", RFC2136: &certs.RFC2136Settings{Nameserver: "ns", TSIGKeyName: "k"}},
			certs.DNSCredentials{TSIGSecret: "not base64!"}},
		"ttl":                 {certs.DNSProviderConfig{Name: "x", Type: "cloudflare", TTL: -1}, certs.DNSCredentials{APIToken: "t"}},
		"propagation timeout": {certs.DNSProviderConfig{Name: "x", Type: "cloudflare", PropagationTimeout: 100000}, certs.DNSCredentials{APIToken: "t"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.Create(ctx, in.cfg, in.creds)
			require.ErrorIs(t, err, certs.ErrInvalidDNSProvider)
			assert.NotContains(t, err.Error(), "not base64!")
		})
	}
}

// TestDNSProvidersTest publishes a test record through an RFC 2136
// provider on an in-process nameserver: success, then a wrong key.
func TestDNSProvidersTest(t *testing.T) {
	ctx := context.Background()
	srv := dnstest.Start(t, "site.test", dnstest.WithTSIG(tsigKey, tsigSecret))
	r := newDNSProviders(t, &memDocs{docs: map[string][]byte{}}, srv.Addr, nil)
	_, err := r.Create(ctx, certs.DNSProviderConfig{Name: "bind", Type: "rfc2136", PollingInterval: 1,
		RFC2136: &certs.RFC2136Settings{Nameserver: srv.Addr, TSIGKeyName: tsigKey}}, certs.DNSCredentials{TSIGSecret: tsigSecret})
	require.NoError(t, err)

	res, err := r.Test(ctx, "bind", "*.site.test")
	require.NoError(t, err)
	assert.True(t, res.OK, res.Error)
	assert.Equal(t, "_acme-challenge.site.test", res.Record)
	assert.Empty(t, srv.TXT("_acme-challenge.site.test."), "removed")
	applied, _ := srv.Updates()
	assert.Equal(t, 2, applied)

	_, err = r.Create(ctx, certs.DNSProviderConfig{Name: "wrong", Type: "rfc2136",
		RFC2136: &certs.RFC2136Settings{Nameserver: srv.Addr, TSIGKeyName: tsigKey}}, certs.DNSCredentials{TSIGSecret: "d3Jvbmcta2V5"})
	require.NoError(t, err)
	res, err = r.Test(ctx, "wrong", "site.test")
	require.NoError(t, err, "a failure of the provider is a result")
	assert.False(t, res.OK)
	assert.Equal(t, "present", res.Step)
	assert.Contains(t, res.Error, "NOTAUTH")
	assert.NotContains(t, res.Error, "d3Jvbmcta2V5")

	_, err = r.Test(ctx, "bind", "not a name")
	require.ErrorIs(t, err, certs.ErrInvalidDNSProvider)
	_, err = r.Test(ctx, "nope", "site.test")
	require.ErrorIs(t, err, certs.ErrDNSProviderNotFound)
}

// TestDNSCredentialsRedacted keeps the credentials out of the formatted
// values and the structured logs.
func TestDNSCredentialsRedacted(t *testing.T) {
	c := certs.DNSCredentials{APIToken: "tok-1", SecretAccessKey: "sak-2", SessionToken: "st-3", TSIGSecret: "ts-4"}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "creds", c)
	out := fmt.Sprintf("%v %+v %#v %s", c, c, c, c) + logs.String()
	for _, s := range []string{"tok-1", "sak-2", "st-3", "ts-4"} {
		assert.NotContains(t, out, s)
	}
	assert.Contains(t, out, "[REDACTED]")
}
