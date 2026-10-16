package admin

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// CertificateRoutes exposes the TLS certificates of the domains of the
// distributions (/certificates, docs/10 §10.2): the uploaded and ACME
// certificates with their status and expiration, the distributions using
// them and the ACME certificates pending; and the DNS providers of the
// DNS-01 challenge (/dns-providers), their credentials write-only.
type CertificateRoutes interface{ Routes }

// CertificateDeps are the dependencies of the certificate routes.
type CertificateDeps struct {
	// Certificates manages the certificates (required).
	Certificates adminsvc.CertificateManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// CertificateOption configures the certificate routes.
type CertificateOption func(*certificateRoutes)

type certificateRoutes struct {
	deps  CertificateDeps
	guard guard
}

var _ CertificateRoutes = (*certificateRoutes)(nil)

// NewCertificateRoutes returns the certificate routes.
func NewCertificateRoutes(deps CertificateDeps, opts ...CertificateOption) (CertificateRoutes, error) {
	if deps.Certificates == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: certificate routes require the certificate manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &certificateRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *certificateRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *certificateRoutes) routes() []route {
	manage := global(policy.ActionAdminManageCertificates)
	return []route{
		{http.MethodGet, "/certificates", manage, h.list},
		{http.MethodPost, "/certificates", manage, h.upload},
		{http.MethodGet, "/certificates/{id}", manage, h.get},
		{http.MethodDelete, "/certificates/{id}", manage, h.remove},
		{http.MethodGet, "/dns-providers", manage, h.listDNS},
		{http.MethodPost, "/dns-providers", manage, h.createDNS},
		{http.MethodGet, "/dns-providers/{name}", manage, h.getDNS},
		{http.MethodPut, "/dns-providers/{name}", manage, h.updateDNS},
		{http.MethodDelete, "/dns-providers/{name}", manage, h.removeDNS},
		{http.MethodPost, "/dns-providers/{name}/test", manage, h.testDNS},
	}
}

// maxCertificateBody bounds the body of an upload (a chain of a few
// certificates and a key).
const maxCertificateBody = 256 << 10

// CertificateResponse is a certificate (never its key) with the
// distributions using it.
type CertificateResponse struct {
	certs.Certificate
	UsedBy []string `json:"usedBy"`
}

func newCertificateResponse(c adminsvc.CertificateInfo) CertificateResponse {
	c.Domains = nonNil(c.Domains)
	return CertificateResponse{Certificate: c.Certificate, UsedBy: nonNil(c.UsedBy)}
}

// ManagedCertificateResponse is the ACME certificate of distributions.
type ManagedCertificateResponse struct {
	ID            string   `json:"id"`
	Domains       []string `json:"domains"`
	Distributions []string `json:"distributions"`
	// Challenge is the ACME challenge of the certificate ("" for the one
	// of the deployment), DNSProvider the DNS provider of dns-01.
	Challenge   string `json:"challenge,omitempty"`
	DNSProvider string `json:"dnsProvider,omitempty"`
	// State is issued or pending.
	State string `json:"state"`
	// Certificate is the certificate obtained (state issued).
	Certificate *certs.Certificate `json:"certificate,omitempty"`
}

// ACMEStatusResponse is the state of the ACME certificates.
type ACMEStatusResponse struct {
	Directory string                       `json:"directory"`
	Challenge string                       `json:"challenge"`
	Managed   []ManagedCertificateResponse `json:"managed"`
}

// CertificateListResponse is the body of GET /certificates.
type CertificateListResponse struct {
	Items []CertificateResponse `json:"items"`
	ACME  ACMEStatusResponse    `json:"acme"`
}

// UploadCertificateRequest is the body of POST /certificates.
type UploadCertificateRequest struct {
	// ID names the certificate (default: derived from its first DNS
	// name); an existing certificate of the same id is replaced.
	ID string `json:"id,omitempty"`
	// CertificatePEM is the leaf certificate followed by its
	// intermediates; PrivateKeyPEM its private key.
	CertificatePEM string `json:"certificatePem"`
	PrivateKeyPEM  string `json:"privateKeyPem"`
}

func (h *certificateRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

func (h *certificateRoutes) list(w http.ResponseWriter, r *http.Request) {
	list, err := h.deps.Certificates.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := CertificateListResponse{
		Items: make([]CertificateResponse, 0, len(list.Items)),
		ACME:  ACMEStatusResponse{Directory: list.ACME.Directory, Challenge: list.ACME.Challenge, Managed: make([]ManagedCertificateResponse, 0, len(list.ACME.Managed))},
	}
	for _, c := range list.Items {
		out.Items = append(out.Items, newCertificateResponse(c))
	}
	for _, mc := range list.ACME.Managed {
		out.ACME.Managed = append(out.ACME.Managed, ManagedCertificateResponse{
			ID: mc.ID, Domains: nonNil(mc.Domains), Distributions: nonNil(mc.Distributions), Challenge: mc.Challenge, DNSProvider: mc.DNSProvider,
			State: mc.State, Certificate: mc.Certificate,
		})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *certificateRoutes) upload(w http.ResponseWriter, r *http.Request) {
	var in UploadCertificateRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxCertificateBody, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	c, err := h.deps.Certificates.Upload(r.Context(), certs.UploadInput{ID: in.ID, ChainPEM: in.CertificatePEM, KeyPEM: in.PrivateKeyPEM})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newCertificateResponse(*c))
}

func (h *certificateRoutes) get(w http.ResponseWriter, r *http.Request) {
	c, err := h.deps.Certificates.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newCertificateResponse(*c))
}

func (h *certificateRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Certificates.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- DNS providers of the DNS-01 challenge --------------------------------------

// maxDNSProviderBody bounds the body of a DNS provider.
const maxDNSProviderBody = 64 << 10

// DNSProviderResponse is a DNS provider (never its credentials) with the
// distributions using it.
type DNSProviderResponse struct {
	certs.DNSProviderConfig
	UsedBy []string `json:"usedBy"`
}

// DNSProviderListResponse is the body of GET /dns-providers.
type DNSProviderListResponse struct {
	Items []DNSProviderResponse `json:"items"`
}

// DNSProviderRequest is the body of POST /dns-providers and PUT
// /dns-providers/{name}: the settings and the credentials (write-only).
type DNSProviderRequest struct {
	Name               string                    `json:"name,omitempty"`
	Type               string                    `json:"type"`
	Comment            string                    `json:"comment,omitempty"`
	Cloudflare         *certs.CloudflareSettings `json:"cloudflare,omitempty"`
	Route53            *certs.Route53Settings    `json:"route53,omitempty"`
	RFC2136            *certs.RFC2136Settings    `json:"rfc2136,omitempty"`
	TTL                int                       `json:"ttl,omitempty"`
	PropagationTimeout int                       `json:"propagationTimeout,omitempty"`
	PollingInterval    int                       `json:"pollingInterval,omitempty"`
	// Credentials are sealed in the store and never returned; on an
	// update the empty ones keep the stored values.
	Credentials certs.DNSCredentials `json:"credentials"`
}

// DNSProviderTestRequest is the body of POST /dns-providers/{name}/test.
type DNSProviderTestRequest struct {
	// Domain is the domain of the test record
	// (_acme-challenge.<domain>).
	Domain string `json:"domain"`
}

func (in DNSProviderRequest) config(name string) certs.DNSProviderConfig {
	return certs.DNSProviderConfig{Name: name, Type: in.Type, Comment: in.Comment, Cloudflare: in.Cloudflare, Route53: in.Route53, RFC2136: in.RFC2136,
		TTL: in.TTL, PropagationTimeout: in.PropagationTimeout, PollingInterval: in.PollingInterval}
}

func newDNSProviderResponse(p adminsvc.DNSProviderInfo) DNSProviderResponse {
	return DNSProviderResponse{DNSProviderConfig: p.DNSProviderConfig, UsedBy: nonNil(p.UsedBy)}
}

func (h *certificateRoutes) listDNS(w http.ResponseWriter, r *http.Request) {
	list, err := h.deps.Certificates.ListDNSProviders(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := DNSProviderListResponse{Items: make([]DNSProviderResponse, 0, len(list))}
	for _, p := range list {
		out.Items = append(out.Items, newDNSProviderResponse(p))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *certificateRoutes) createDNS(w http.ResponseWriter, r *http.Request) {
	var in DNSProviderRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxDNSProviderBody, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	p, err := h.deps.Certificates.CreateDNSProvider(r.Context(), in.config(in.Name), in.Credentials)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newDNSProviderResponse(*p))
}

func (h *certificateRoutes) getDNS(w http.ResponseWriter, r *http.Request) {
	p, err := h.deps.Certificates.GetDNSProvider(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newDNSProviderResponse(*p))
}

func (h *certificateRoutes) updateDNS(w http.ResponseWriter, r *http.Request) {
	var in DNSProviderRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxDNSProviderBody, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	name := chi.URLParam(r, "name")
	if in.Name != "" && in.Name != name {
		h.fail(w, r, fmt.Errorf("%w: the name of the body (%s) is not the one of the path (%s)", certs.ErrInvalidDNSProvider, in.Name, name))
		return
	}
	p, err := h.deps.Certificates.UpdateDNSProvider(r.Context(), in.config(name), in.Credentials)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newDNSProviderResponse(*p))
}

func (h *certificateRoutes) removeDNS(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Certificates.DeleteDNSProvider(r.Context(), chi.URLParam(r, "name")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *certificateRoutes) testDNS(w http.ResponseWriter, r *http.Request) {
	var in DNSProviderTestRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxDNSProviderBody, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Certificates.TestDNSProvider(r.Context(), chi.URLParam(r, "name"), in.Domain)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, res)
}
