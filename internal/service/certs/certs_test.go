package certs_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/certs"
	certsmock "github.com/r3n3r0/nexo/internal/service/certs/mock"
	"github.com/r3n3r0/nexo/internal/service/cluster"
)

func TestUploadValidation(t *testing.T) {
	ctx := context.Background()
	docs := &memDocs{docs: map[string][]byte{}}
	nodes := deployment(t, docs, []cluster.Leader{leader(t, false)}, nil, clock.NewReal())
	svc := nodes[0].svc

	root := newCA(t, "root")
	inter := root.intermediate(t, "intermediate")
	other := newCA(t, "other")
	now := time.Now()
	leafPEM, keyPEM := inter.leaf(t, []string{"www.example.com", "example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	_, otherKey := inter.leaf(t, []string{"www.example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	expiredPEM, expiredKey := inter.leaf(t, []string{"old.example.com"}, now.Add(-48*time.Hour), now.Add(-time.Hour))
	noSANPEM, noSANKey := inter.leaf(t, nil, now.Add(-time.Hour), now.Add(time.Hour))

	tests := []struct {
		name string
		in   certs.UploadInput
		ok   bool
	}{
		{name: "leaf and intermediate", in: certs.UploadInput{ChainPEM: leafPEM + inter.pem, KeyPEM: keyPEM}, ok: true},
		{name: "leaf alone", in: certs.UploadInput{ID: "solo", ChainPEM: leafPEM, KeyPEM: keyPEM}, ok: true},
		{name: "full chain to the root", in: certs.UploadInput{ID: "full", ChainPEM: leafPEM + inter.pem + root.pem, KeyPEM: keyPEM}, ok: true},
		{name: "key of another certificate", in: certs.UploadInput{ChainPEM: leafPEM, KeyPEM: otherKey}},
		{name: "chain in the wrong order", in: certs.UploadInput{ChainPEM: inter.pem + leafPEM, KeyPEM: keyPEM}},
		{name: "foreign intermediate", in: certs.UploadInput{ChainPEM: leafPEM + other.pem, KeyPEM: keyPEM}},
		{name: "expired", in: certs.UploadInput{ChainPEM: expiredPEM, KeyPEM: expiredKey}},
		{name: "no DNS name", in: certs.UploadInput{ChainPEM: noSANPEM, KeyPEM: noSANKey}},
		{name: "garbage", in: certs.UploadInput{ChainPEM: "x", KeyPEM: "y"}},
		{name: "invalid id", in: certs.UploadInput{ID: "Bad/ID", ChainPEM: leafPEM, KeyPEM: keyPEM}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := svc.Upload(ctx, tc.in)
			if !tc.ok {
				require.ErrorIs(t, err, domain.ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, certs.SourceUpload, c.Source)
			assert.Equal(t, []string{"www.example.com", "example.com"}, c.Domains)
			assert.Equal(t, certs.StatusValid, c.Status)
			assert.NotEmpty(t, c.Fingerprint)
		})
	}
	list, err := svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, "full", list[0].ID)
	assert.Equal(t, "solo", list[1].ID)
	assert.Equal(t, "www.example.com", list[2].ID, "id derived from the first name")
	assert.NotContains(t, docs.all(), "PRIVATE KEY", "the keys are sealed in the store")

	require.NoError(t, svc.Delete(ctx, "solo"))
	_, err = svc.Get(ctx, "solo")
	require.ErrorIs(t, err, certs.ErrCertificateNotFound)
}

// TestSNISelection covers the selection by SNI with several certificates
// (TDS-10) and the propagation of an upload to the other nodes.
func TestSNISelection(t *testing.T) {
	ctx := context.Background()
	docs := &memDocs{docs: map[string][]byte{}}
	nodes := deployment(t, docs, []cluster.Leader{leader(t, false), leader(t, false)}, nil, clock.NewReal())
	ca := newCA(t, "ca")
	now := time.Now()
	upload := func(id string, names []string, notAfter time.Time) {
		chain, key := ca.leaf(t, names, now.Add(-time.Hour), notAfter)
		_, err := nodes[0].svc.Upload(ctx, certs.UploadInput{ID: id, ChainPEM: chain, KeyPEM: key})
		require.NoError(t, err)
	}
	upload("site", []string{"www.example.com", "example.com"}, now.Add(30*24*time.Hour))
	upload("site-new", []string{"www.example.com"}, now.Add(60*24*time.Hour))
	upload("wild", []string{"*.preview.example.com"}, now.Add(30*24*time.Hour))
	upload("other", []string{"static.example.it"}, now.Add(30*24*time.Hour))

	tests := []struct {
		name string
		want string
	}{
		{name: "www.example.com", want: "www.example.com"},
		{name: "WWW.EXAMPLE.COM.", want: "www.example.com"},
		{name: "example.com", want: "example.com"},
		{name: "pr-1.preview.example.com", want: "*.preview.example.com"},
		{name: "static.example.it", want: "static.example.it"},
		{name: "a.b.preview.example.com"},
		{name: "unknown.example.net"},
		{name: ""},
	}
	for i, n := range nodes {
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				c, err := n.svc.GetCertificate(&tls.ClientHelloInfo{ServerName: tc.name})
				require.NoError(t, err)
				if tc.want == "" {
					assert.Nil(t, c, "node %d", i+1)
					return
				}
				require.NotNil(t, c, "node %d", i+1)
				leaf, err := x509.ParseCertificate(c.Certificate[0])
				require.NoError(t, err)
				assert.Contains(t, leaf.DNSNames, tc.want)
				if tc.want == "www.example.com" {
					assert.WithinDuration(t, now.Add(60*24*time.Hour), leaf.NotAfter, time.Minute, "the certificate expiring last wins")
				}
			})
		}
	}
	// A deletion reaches the other node too.
	require.NoError(t, nodes[0].svc.Delete(ctx, "other"))
	c, err := nodes[1].svc.GetCertificate(&tls.ClientHelloInfo{ServerName: "static.example.it"})
	require.NoError(t, err)
	assert.Nil(t, c)
}

// TestExpiryStatus checks the warning window of 21 days.
func TestExpiryStatus(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	fake := clock.NewFake(start)
	docs := &memDocs{docs: map[string][]byte{}}
	nodes := deployment(t, docs, []cluster.Leader{leader(t, false)}, nil, fake)
	ca := newCA(t, "ca")
	chain, key := ca.leaf(t, []string{"www.example.com"}, start.Add(-time.Hour), start.Add(30*24*time.Hour))
	c, err := nodes[0].svc.Upload(ctx, certs.UploadInput{ChainPEM: chain, KeyPEM: key})
	require.NoError(t, err)
	assert.Equal(t, certs.StatusValid, c.Status)
	fake.Advance(10 * 24 * time.Hour)
	c, err = nodes[0].svc.Get(ctx, c.ID)
	require.NoError(t, err)
	assert.Equal(t, certs.StatusExpiring, c.Status, "20 days left")
	fake.Advance(21 * 24 * time.Hour)
	c, err = nodes[0].svc.Get(ctx, c.ID)
	require.NoError(t, err)
	assert.Equal(t, certs.StatusExpired, c.Status)
}

// TestReconcileOnlyOnLeader checks that the ACME job never runs on a node
// that is not the leader.
func TestReconcileOnlyOnLeader(t *testing.T) {
	domains := certsmock.NewDomainSource(t) // no call expected
	docs := &memDocs{docs: map[string][]byte{}}
	nodes := deployment(t, docs, []cluster.Leader{leader(t, false)}, domains, clock.NewReal())
	require.NoError(t, nodes[0].svc.Reconcile(context.Background()))
	domains.AssertNotCalled(t, "ACMEDomains", tmock.Anything)

	_, err := nodes[0].svc.HTTPChallenge(context.Background(), "unknown-token")
	require.ErrorIs(t, err, certs.ErrChallengeNotFound)
	_, err = nodes[0].svc.HTTPChallenge(context.Background(), "../etc/passwd")
	require.ErrorIs(t, err, certs.ErrChallengeNotFound)
}

func TestSealer(t *testing.T) {
	ctx := context.Background()
	s, err := certs.NewSealer(certs.DeriveKey("root", "secret", "dep"))
	require.NoError(t, err)
	sealed, err := s.Seal(ctx, []byte("private"), []byte("aad"))
	require.NoError(t, err)
	assert.NotContains(t, string(sealed), "private")
	plain, err := s.Open(ctx, sealed, []byte("aad"))
	require.NoError(t, err)
	assert.Equal(t, "private", string(plain))
	_, err = s.Open(ctx, sealed, []byte("other"))
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	other, err := certs.NewSealer(certs.DeriveKey("root", "secret", "other-deployment"))
	require.NoError(t, err)
	_, err = other.Open(ctx, sealed, []byte("aad"))
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = certs.NewSealer([]byte("short"))
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestLeaderJob runs the reconciliation while the node leads: at the
// election and when triggered, never after the leadership is lost.
func TestLeaderJob(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	domains := certsmock.NewDomainSource(t)
	domains.EXPECT().ACMEDomains(tmock.Anything).RunAndReturn(func(context.Context) ([]certs.ManagedDomains, error) {
		calls.Add(1)
		return nil, nil
	}).Maybe()
	leader := cluster.NewLocalLeader(cluster.LeaderDeps{Logger: slog.New(slog.DiscardHandler)})
	nodes := deployment(t, &memDocs{docs: map[string][]byte{}}, []cluster.Leader{leader}, domains, clock.NewReal())
	assert.Zero(t, calls.Load(), "not the leader yet")
	require.NoError(t, leader.Start(ctx))
	require.Eventually(t, func() bool { return calls.Load() == 1 }, 5*time.Second, 10*time.Millisecond, "reconciled at the election")
	nodes[0].svc.Trigger(ctx)
	require.Eventually(t, func() bool { return calls.Load() == 2 }, 5*time.Second, 10*time.Millisecond, "reconciled when triggered")
	require.NoError(t, leader.Stop(ctx))
	nodes[0].svc.Trigger(ctx)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(2), calls.Load(), "no job after the leadership")
}
