package admin

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/distribution"
)

// CertificateManagerDeps are the dependencies of the certificate manager.
type CertificateManagerDeps struct {
	// Certs stores the certificates (required).
	Certs certs.Service
	// Distributions tell which distributions use a certificate (optional:
	// without it no certificate is reported in use).
	Distributions distribution.Service
	// Domains lists the ACME certificates of the distributions (optional:
	// without it no ACME certificate is reported).
	Domains certs.DomainSource
	Logger  *slog.Logger
}

// CertificateManagerOption configures the certificate manager.
type CertificateManagerOption func(*certificateManager)

// WithACME reports the ACME directory and challenge type of the
// deployment in CertificateList.ACME (empty values keep the defaults of
// the certificate service: Let's Encrypt, http-01).
func WithACME(directory, challenge string) CertificateManagerOption {
	return func(m *certificateManager) {
		if directory != "" {
			m.acmeDirectory = directory
		}
		if challenge != "" {
			m.acmeChallenge = challenge
		}
	}
}

type certificateManager struct {
	deps          CertificateManagerDeps
	acmeDirectory string
	acmeChallenge string
}

var _ CertificateManager = (*certificateManager)(nil)

// NewCertificateManager returns the certificate manager of the Admin API.
// A missing certificate service is a wiring error
// (domain.ErrInvalidArgument).
func NewCertificateManager(deps CertificateManagerDeps, opts ...CertificateManagerOption) (CertificateManager, error) {
	if deps.Certs == nil {
		return nil, fmt.Errorf("%w: certificate manager: the certificate service is required", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &certificateManager{deps: deps, acmeDirectory: certs.LetsEncryptDirectory, acmeChallenge: certs.ChallengeHTTP01}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// usage maps the certificates to the distributions using them.
type usage struct {
	uploads map[string][]string
	// managed are the ACME certificates with their distributions.
	managed []ManagedCertificate
}

// usage computes the distributions using each certificate.
func (m *certificateManager) usage(ctx context.Context) (usage, error) {
	u := usage{uploads: map[string][]string{}}
	var dists []distribution.Distribution
	if m.deps.Distributions != nil {
		var err error
		if dists, err = m.deps.Distributions.List(ctx); err != nil {
			return usage{}, err
		}
	}
	for _, d := range dists {
		if id, ok := strings.CutPrefix(d.TLS.Certificate, distribution.CertificateUploadPrefix); ok {
			u.uploads[id] = append(u.uploads[id], d.ID)
		}
	}
	if m.deps.Domains == nil {
		return u, nil
	}
	managed, err := m.deps.Domains.ACMEDomains(ctx)
	if err != nil {
		return usage{}, err
	}
	for _, md := range managed {
		mc := ManagedCertificate{ID: md.ID, Domains: slices.Clone(md.Domains), Distributions: []string{}, State: ACMEPending}
		for _, d := range dists {
			if d.TLS.Certificate == distribution.CertificateAuto && sameNames(d.Aliases, md.Domains) {
				mc.Distributions = append(mc.Distributions, d.ID)
			}
		}
		u.managed = append(u.managed, mc)
	}
	return u, nil
}

// sameNames reports whether a and b hold the same names, in any order.
func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// usedBy returns the distributions using the certificate c.
func (u usage) usedBy(c certs.Certificate) []string {
	out := slices.Clone(u.uploads[c.ID])
	for _, mc := range u.managed {
		if mc.ID == c.ID {
			out = append(out, mc.Distributions...)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// List implements CertificateManager.
func (m *certificateManager) List(ctx context.Context) (*CertificateList, error) {
	list, err := m.deps.Certs.List(ctx)
	if err != nil {
		return nil, err
	}
	u, err := m.usage(ctx)
	if err != nil {
		return nil, err
	}
	out := &CertificateList{
		Items: make([]CertificateInfo, 0, len(list)),
		ACME:  ACMEStatus{Directory: m.acmeDirectory, Challenge: m.acmeChallenge, Managed: []ManagedCertificate{}},
	}
	byID := make(map[string]certs.Certificate, len(list))
	for _, c := range list {
		byID[c.ID] = c
		out.Items = append(out.Items, CertificateInfo{Certificate: c, UsedBy: u.usedBy(c)})
	}
	for _, mc := range u.managed {
		if c, ok := byID[mc.ID]; ok {
			mc.State, mc.Certificate = ACMEIssued, &c
		}
		out.ACME.Managed = append(out.ACME.Managed, mc)
	}
	return out, nil
}

// Get implements CertificateManager.
func (m *certificateManager) Get(ctx context.Context, id string) (*CertificateInfo, error) {
	c, err := m.deps.Certs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	u, err := m.usage(ctx)
	if err != nil {
		return nil, err
	}
	return &CertificateInfo{Certificate: c, UsedBy: u.usedBy(c)}, nil
}

// Upload implements CertificateManager.
func (m *certificateManager) Upload(ctx context.Context, in certs.UploadInput) (*CertificateInfo, error) {
	if strings.TrimSpace(in.ChainPEM) == "" || strings.TrimSpace(in.KeyPEM) == "" {
		return nil, fmt.Errorf("%w: the certificate chain and its private key are required, in PEM", certs.ErrInvalidCertificate)
	}
	c, err := m.deps.Certs.Upload(ctx, in)
	if err != nil {
		return nil, err
	}
	u, err := m.usage(ctx)
	if err != nil {
		return nil, err
	}
	return &CertificateInfo{Certificate: c, UsedBy: u.usedBy(c)}, nil
}

// Delete implements CertificateManager: a certificate a distribution names
// (upload:<id>) is kept, an ACME certificate in use is renewed again by
// the leader.
func (m *certificateManager) Delete(ctx context.Context, id string) error {
	c, err := m.deps.Certs.Get(ctx, id)
	if err != nil {
		return err
	}
	u, err := m.usage(ctx)
	if err != nil {
		return err
	}
	if users := u.uploads[c.ID]; len(users) > 0 {
		return fmt.Errorf("%w: certificate %s is used by the distributions %s: change their TLS first", domain.ErrConflict, c.ID, strings.Join(users, ", "))
	}
	return m.deps.Certs.Delete(ctx, id)
}
