package admin_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/certs"
	certsmock "github.com/r3n3r0/nexo/internal/service/certs/mock"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	distmock "github.com/r3n3r0/nexo/internal/service/distribution/mock"
)

type certEnv struct {
	certs   *certsmock.Service
	dists   *distmock.Service
	domains *certsmock.DomainSource
	mgr     admin.CertificateManager
}

func newCertEnv(t *testing.T) *certEnv {
	e := &certEnv{certs: certsmock.NewService(t), dists: distmock.NewService(t), domains: certsmock.NewDomainSource(t)}
	m, err := admin.NewCertificateManager(admin.CertificateManagerDeps{Certs: e.certs, Distributions: e.dists, Domains: e.domains},
		admin.WithACME("https://acme.test/dir", certs.ChallengeTLSALPN01))
	require.NoError(t, err)
	e.mgr = m
	// Three distributions: one on an uploaded certificate, one with ACME,
	// one without TLS.
	e.dists.EXPECT().List(mock.Anything).Return([]distribution.Distribution{
		{ID: "a", Enabled: true, Aliases: []string{"a.example.com"}, TLS: distribution.TLS{Certificate: "upload:shop"}},
		{ID: "b", Enabled: true, Aliases: []string{"b.example.com", "www.b.example.com"}, TLS: distribution.TLS{Certificate: distribution.CertificateAuto}},
		{ID: "c", Enabled: true, Aliases: []string{"c.example.com"}, TLS: distribution.TLS{Certificate: distribution.CertificateAuto}},
		{ID: "d", Enabled: true, Aliases: []string{"d.example.com"}},
	}, nil).Maybe()
	e.domains.EXPECT().ACMEDomains(mock.Anything).Return([]certs.ManagedDomains{
		{ID: "acme-b", Domains: []string{"www.b.example.com", "b.example.com"}},
		{ID: "acme-c", Domains: []string{"c.example.com"}},
	}, nil).Maybe()
	return e
}

func TestNewCertificateManagerRequiresCerts(t *testing.T) {
	_, err := admin.NewCertificateManager(admin.CertificateManagerDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestCertificateManagerList(t *testing.T) {
	e := newCertEnv(t)
	notAfter := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	e.certs.EXPECT().List(mock.Anything).Return([]certs.Certificate{
		{ID: "acme-b", Source: certs.SourceACME, Domains: []string{"b.example.com"}, NotAfter: notAfter, Status: certs.StatusValid},
		{ID: "old", Source: certs.SourceUpload, Status: certs.StatusExpired},
		{ID: "shop", Source: certs.SourceUpload, Status: certs.StatusExpiring},
	}, nil)
	got, err := e.mgr.List(context.Background())
	require.NoError(t, err)
	require.Len(t, got.Items, 3)
	assert.Equal(t, []string{"b"}, got.Items[0].UsedBy)
	assert.Equal(t, []string{}, got.Items[1].UsedBy)
	assert.Equal(t, []string{"a"}, got.Items[2].UsedBy)
	assert.Equal(t, "https://acme.test/dir", got.ACME.Directory)
	assert.Equal(t, certs.ChallengeTLSALPN01, got.ACME.Challenge)
	require.Len(t, got.ACME.Managed, 2)
	assert.Equal(t, admin.ACMEIssued, got.ACME.Managed[0].State)
	require.NotNil(t, got.ACME.Managed[0].Certificate)
	assert.Equal(t, notAfter, got.ACME.Managed[0].Certificate.NotAfter)
	assert.Equal(t, []string{"b"}, got.ACME.Managed[0].Distributions)
	assert.Equal(t, admin.ACMEPending, got.ACME.Managed[1].State)
	assert.Nil(t, got.ACME.Managed[1].Certificate)
	assert.Equal(t, []string{"c"}, got.ACME.Managed[1].Distributions)
}

func TestCertificateManagerDelete(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		id      string
		deleted bool
		wantErr error
	}{
		{name: "used by a distribution", id: "shop", wantErr: domain.ErrConflict},
		{name: "unused", id: "old", deleted: true},
		{name: "ACME certificate in use: renewed again", id: "acme-b", deleted: true},
		{name: "unknown", id: "nope", wantErr: domain.ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newCertEnv(t)
			if tc.id == "nope" {
				e.certs.EXPECT().Get(mock.Anything, tc.id).Return(certs.Certificate{}, certs.ErrCertificateNotFound)
			} else {
				e.certs.EXPECT().Get(mock.Anything, tc.id).Return(certs.Certificate{ID: tc.id}, nil)
			}
			if tc.deleted {
				e.certs.EXPECT().Delete(mock.Anything, tc.id).Return(nil)
			}
			err := e.mgr.Delete(ctx, tc.id)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestCertificateManagerUpload(t *testing.T) {
	ctx := context.Background()
	e := newCertEnv(t)
	_, err := e.mgr.Upload(ctx, certs.UploadInput{ID: "shop", ChainPEM: " "})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	in := certs.UploadInput{ID: "shop", ChainPEM: "CHAIN", KeyPEM: "KEY"}
	e.certs.EXPECT().Upload(mock.Anything, in).Return(certs.Certificate{ID: "shop"}, nil)
	got, err := e.mgr.Upload(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, got.UsedBy)
	e.certs.EXPECT().Get(mock.Anything, "shop").Return(certs.Certificate{ID: "shop"}, nil)
	got, err = e.mgr.Get(ctx, "shop")
	require.NoError(t, err)
	assert.Equal(t, "shop", got.ID)
}

func TestCertificateManagerWithoutDistributions(t *testing.T) {
	c := certsmock.NewService(t)
	m, err := admin.NewCertificateManager(admin.CertificateManagerDeps{Certs: c})
	require.NoError(t, err)
	c.EXPECT().List(mock.Anything).Return([]certs.Certificate{{ID: "x"}}, nil)
	got, err := m.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{}, got.Items[0].UsedBy)
	assert.Equal(t, certs.LetsEncryptDirectory, got.ACME.Directory)
	assert.Empty(t, got.ACME.Managed)
}
