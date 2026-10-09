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
// them and the ACME certificates pending.
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
			ID: mc.ID, Domains: nonNil(mc.Domains), Distributions: nonNil(mc.Distributions), State: mc.State, Certificate: mc.Certificate,
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
