package certs_test

// The ACME tests run the Let's Encrypt test CA, Pebble
// (github.com/letsencrypt/pebble/v2), in the test process: its web front
// end on an httptest server, its validation authority resolving every
// name to 127.0.0.1 through the mock DNS server of challtestsrv, and
// validating the challenges on the HTTP and TLS listeners of the test,
// which answer with the certificate service. No network, no Docker.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/letsencrypt/challtestsrv"
	pebbleca "github.com/letsencrypt/pebble/v2/ca"
	pebbledb "github.com/letsencrypt/pebble/v2/db"
	pebbleva "github.com/letsencrypt/pebble/v2/va"
	pebblewfe "github.com/letsencrypt/pebble/v2/wfe"
	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/acme"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/certs"
	certsmock "github.com/r3n3r0/nexo/internal/service/certs/mock"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	clustermock "github.com/r3n3r0/nexo/internal/service/cluster/mock"
)

// switchableLeader is a leader mock whose state the test changes.
func switchableLeader(t testing.TB, elected *atomic.Bool) *clustermock.Leader {
	t.Helper()
	m := clustermock.NewLeader(t)
	m.EXPECT().IsLeader().RunAndReturn(elected.Load).Maybe()
	m.EXPECT().Subscribe(tmock.Anything).Return(func() {}).Maybe()
	return m
}

// freeUDPAddr returns a loopback address free for UDP and TCP.
func freeUDPAddr(t testing.TB) string {
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

// challengeListeners answer the validations of the CA with svc: HTTP-01
// on an HTTP listener, TLS-ALPN-01 on a TLS listener.
func challengeListeners(t testing.TB, answer func() certs.Service) (httpPort, tlsPort int) {
	t.Helper()
	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	httpSrv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.URL.Path, "/.well-known/acme-challenge/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		keyAuth, err := answer().HTTPChallenge(r.Context(), token)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, keyAuth)
	})}
	go func() { _ = httpSrv.Serve(httpLn) }()
	t.Cleanup(func() { _ = httpSrv.Close() })

	tlsLn, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return answer().GetCertificate(h) },
		NextProtos:     []string{"http/1.1", acme.ALPNProto},
		MinVersion:     tls.VersionTLS12,
	})
	require.NoError(t, err)
	go func() {
		for {
			conn, err := tlsLn.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = conn.(*tls.Conn).Handshake()
				_ = conn.Close()
			}()
		}
	}()
	t.Cleanup(func() { _ = tlsLn.Close() })
	return httpLn.Addr().(*net.TCPAddr).Port, tlsLn.Addr().(*net.TCPAddr).Port
}

// startPebble starts the test CA; it returns the directory URL, the
// client trusting its HTTPS front end and the pool of its roots.
func startPebble(t testing.TB, httpPort, tlsPort int) (string, *http.Client, *x509.CertPool) {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	dnsAddr := freeUDPAddr(t)
	dns, err := challtestsrv.New(challtestsrv.Config{DNSOneAddrs: []string{dnsAddr}, Log: logger})
	require.NoError(t, err)
	dns.SetDefaultDNSIPv4("127.0.0.1")
	dns.SetDefaultDNSIPv6("")
	dns.Run()
	t.Cleanup(dns.Shutdown)

	db := pebbledb.NewMemoryStore()
	ca := pebbleca.New(logger, db, "", "ecdsa", 0, 1, map[string]pebbleca.Profile{
		"default": {Description: "test", ValidityPeriod: 90 * 24 * 3600},
	})
	va := pebbleva.New(logger, httpPort, tlsPort, false, dnsAddr, db)
	wfe := pebblewfe.New(logger, db, va, ca, false, false, 0, 0)
	srv := httptest.NewTLSServer(wfe.Handler())
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(ca.GetRootCert(0).Cert)
	return srv.URL + pebblewfe.DirectoryPath, srv.Client(), roots
}

// verifyServed checks that node serves for name a certificate of the
// test CA covering it; it returns the serial number.
func verifyServed(t *testing.T, svc certs.Service, name string, roots *x509.CertPool) string {
	t.Helper()
	c, err := svc.GetCertificate(&tls.ClientHelloInfo{ServerName: name})
	require.NoError(t, err)
	require.NotNil(t, c, "no certificate for %s", name)
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	require.NoError(t, err)
	inter := x509.NewCertPool()
	for _, der := range c.Certificate[1:] {
		ic, err := x509.ParseCertificate(der)
		require.NoError(t, err)
		inter.AddCert(ic)
	}
	_, err = leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots, Intermediates: inter})
	require.NoError(t, err)
	return leaf.SerialNumber.String()
}

// TestACME obtains, serves and renews certificates from Pebble with both
// challenges, in a deployment of two nodes where only the leader orders
// and both serve (TDS-10).
func TestACME(t *testing.T) {
	if testing.Short() {
		t.Skip("ACME against Pebble skipped with -short")
	}
	t.Setenv("PEBBLE_VA_NOSLEEP", "1")
	t.Setenv("PEBBLE_WFE_NONCEREJECT", "0")
	t.Setenv("PEBBLE_AUTHZREUSE", "0")
	for _, challenge := range []string{certs.ChallengeHTTP01, certs.ChallengeTLSALPN01} {
		t.Run(challenge, func(t *testing.T) {
			ctx := context.Background()
			var nodes []*testNode
			// The follower answers the validations: the challenges are in
			// the shared store.
			httpPort, tlsPort := challengeListeners(t, func() certs.Service { return nodes[1].svc })
			directory, client, roots := startPebble(t, httpPort, tlsPort)

			domains := certsmock.NewDomainSource(t)
			wanted := []certs.ManagedDomains{{ID: "acme-site", Domains: []string{"www.site.test", "site.test", "*.preview.site.test"}}}
			domains.EXPECT().ACMEDomains(tmock.Anything).RunAndReturn(func(context.Context) ([]certs.ManagedDomains, error) {
				return wanted, nil
			}).Maybe()
			fake := clock.NewFake(time.Now())
			var first, second atomic.Bool
			first.Store(true)
			docs := &memDocs{docs: map[string][]byte{}}
			nodes = deployment(t, docs, []cluster.Leader{switchableLeader(t, &first), switchableLeader(t, &second)}, domains, fake,
				certs.WithACMEDirectory(directory), certs.WithHTTPClient(client), certs.WithACMEEmail("ops@site.test"), certs.WithChallenge(challenge),
				certs.WithCheckInterval(time.Hour))

			// The follower does nothing; the leader orders.
			require.NoError(t, nodes[1].svc.Reconcile(ctx))
			list, err := nodes[1].svc.List(ctx)
			require.NoError(t, err)
			require.Empty(t, list)
			require.NoError(t, nodes[0].svc.Reconcile(ctx))

			serial := ""
			for i, n := range nodes {
				for _, name := range []string{"www.site.test", "site.test"} {
					s := verifyServed(t, n.svc, name, roots)
					if serial == "" {
						serial = s
					}
					assert.Equal(t, serial, s, "node %d", i+1)
				}
			}
			c, err := nodes[1].svc.Get(ctx, "acme-site")
			require.NoError(t, err)
			assert.Equal(t, certs.SourceACME, c.Source)
			assert.Equal(t, []string{"site.test", "www.site.test"}, c.Domains, "the wildcard needs DNS-01")
			assert.NotContains(t, docs.all(), "PRIVATE KEY", "account and certificate keys are sealed")

			// Nothing to do while the certificate is fresh.
			require.NoError(t, nodes[0].svc.Reconcile(ctx))
			assert.Equal(t, serial, verifyServed(t, nodes[1].svc, "www.site.test", roots))

			// 61 days later the certificate is within the renewal window:
			// the follower still does nothing, the new leader renews and
			// every node serves the new certificate.
			fake.Advance(61 * 24 * time.Hour)
			require.NoError(t, nodes[1].svc.Reconcile(ctx))
			assert.Equal(t, serial, verifyServed(t, nodes[1].svc, "www.site.test", roots))
			first.Store(false)
			second.Store(true)
			require.NoError(t, nodes[0].svc.Reconcile(ctx), "no longer the leader: no-op")
			require.NoError(t, nodes[1].svc.Reconcile(ctx))
			renewed := verifyServed(t, nodes[0].svc, "www.site.test", roots)
			assert.NotEqual(t, serial, renewed, "renewed")
			assert.Equal(t, renewed, verifyServed(t, nodes[1].svc, "site.test", roots))

			// A distribution that no longer wants the certificate drops it.
			wanted = nil
			require.NoError(t, nodes[1].svc.Reconcile(ctx))
			_, err = nodes[0].svc.Get(ctx, "acme-site")
			require.ErrorIs(t, err, certs.ErrCertificateNotFound)
		})
	}
}

// TestACMEFailure checks that an order the CA cannot validate is reported
// and retried later, not in a loop.
func TestACMEFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("ACME against Pebble skipped with -short")
	}
	t.Setenv("PEBBLE_VA_NOSLEEP", "1")
	t.Setenv("PEBBLE_WFE_NONCEREJECT", "0")
	ctx := context.Background()
	// Nobody answers the challenges.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	directory, client, _ := startPebble(t, port, port)
	domains := certsmock.NewDomainSource(t)
	domains.EXPECT().ACMEDomains(tmock.Anything).Return([]certs.ManagedDomains{{ID: "acme-x", Domains: []string{"x.site.test"}}}, nil).Maybe()
	var elected atomic.Bool
	elected.Store(true)
	nodes := deployment(t, &memDocs{docs: map[string][]byte{}}, []cluster.Leader{switchableLeader(t, &elected)}, domains, clock.NewReal(),
		certs.WithACMEDirectory(directory), certs.WithHTTPClient(client), certs.WithRetryInterval(time.Hour))
	err = nodes[0].svc.Reconcile(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "acme-x")
	assert.Contains(t, err.Error(), "validation", "the order failed on the challenge")
	// Within the retry interval the order is not attempted again.
	require.NoError(t, nodes[0].svc.Reconcile(ctx))
}
