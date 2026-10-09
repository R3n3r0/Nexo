package certs_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	clustermock "github.com/r3n3r0/nexo/internal/service/cluster/mock"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	sysmock "github.com/r3n3r0/nexo/internal/service/sysstore/mock"
)

// memDocs is the shared state of the store mocks of a test deployment.
type memDocs struct {
	mu   sync.Mutex
	docs map[string][]byte
}

func (m *memDocs) all() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	for k, v := range m.docs {
		b.WriteString(k)
		b.Write(v)
	}
	return b.String()
}

func newStore(t testing.TB, docs *memDocs) *sysmock.Store {
	t.Helper()
	m := sysmock.NewStore(t)
	m.EXPECT().Get(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, p string) ([]byte, error) {
		docs.mu.Lock()
		defer docs.mu.Unlock()
		d, ok := docs.docs[p]
		if !ok {
			return nil, sysstore.ErrNotFound
		}
		return append([]byte(nil), d...), nil
	}).Maybe()
	m.EXPECT().Put(tmock.Anything, tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, p string, data []byte) error {
		docs.mu.Lock()
		defer docs.mu.Unlock()
		docs.docs[p] = append([]byte(nil), data...)
		return nil
	}).Maybe()
	m.EXPECT().Delete(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, p string) error {
		docs.mu.Lock()
		defer docs.mu.Unlock()
		delete(docs.docs, p)
		return nil
	}).Maybe()
	m.EXPECT().List(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, dir string) (map[string][]byte, error) {
		docs.mu.Lock()
		defer docs.mu.Unlock()
		out := map[string][]byte{}
		for p, d := range docs.docs {
			if rest, ok := strings.CutPrefix(p, dir+"/"); ok && !strings.Contains(rest, "/") {
				out[rest] = append([]byte(nil), d...)
			}
		}
		return out, nil
	}).Maybe()
	return m
}

// leader is a leader mock with a settable state.
func leader(t testing.TB, elected bool) *clustermock.Leader {
	t.Helper()
	m := clustermock.NewLeader(t)
	m.EXPECT().IsLeader().Return(elected).Maybe()
	m.EXPECT().Subscribe(tmock.Anything).Return(func() {}).Maybe()
	return m
}

// testNode is a certificate service of a test deployment.
type testNode struct {
	svc      certs.Service
	notifier cluster.Notifier
}

// deployment builds n certificate services on one store; the broadcasts
// of a node reach the others. leaders gives the leader election of each.
func deployment(t testing.TB, docs *memDocs, leaders []cluster.Leader, domains certs.DomainSource, fake clock.Clock, opts ...certs.Option) []*testNode {
	t.Helper()
	sealer, err := certs.NewSealer(certs.DeriveKey("root", "root-secret", "deployment"))
	require.NoError(t, err)
	locks := lock.NewLocal(lock.Deps{})
	nodes := make([]*testNode, len(leaders))
	for i := range nodes {
		nodes[i] = &testNode{notifier: cluster.NewLocalNotifier(cluster.NotifierDeps{Logger: slog.New(slog.DiscardHandler)})}
	}
	for i := range nodes {
		self := i
		n := clustermock.NewNotifier(t)
		n.EXPECT().Subscribe(tmock.Anything, tmock.Anything).RunAndReturn(nodes[i].notifier.Subscribe).Maybe()
		n.EXPECT().Broadcast(tmock.Anything, tmock.Anything).RunAndReturn(func(ctx context.Context, e cluster.Event) error {
			for j, other := range nodes {
				if j != self {
					_ = other.notifier.Deliver(ctx, e)
				}
			}
			return nil
		}).Maybe()
		svc, err := certs.New(certs.Deps{
			Store: newStore(t, docs), Locks: locks, Notifier: n, Leader: leaders[i], Sealer: sealer, Domains: domains,
			Clock: fake, Logger: slog.New(slog.DiscardHandler),
		}, opts...)
		require.NoError(t, err)
		require.NoError(t, svc.Start(context.Background()))
		t.Cleanup(func() { _ = svc.Stop(context.Background()) })
		nodes[i].svc = svc
	}
	return nodes
}

// testCA issues certificates for the tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newCA(t testing.TB, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &testCA{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// intermediate issues an intermediate CA.
func (ca *testCA) intermediate(t testing.TB, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(5 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &testCA{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// leaf issues a leaf certificate; it returns the leaf PEM and its key PEM.
func (ca *testCA) leaf(t testing.TB, names []string, notBefore, notAfter time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: notBefore, NotAfter: notAfter, DNSNames: names,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}
