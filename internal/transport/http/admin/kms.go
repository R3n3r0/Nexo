package admin

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/kms"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// KMSRoutes exposes the key management endpoints (/kms/status,
// /kms/keys): the state of the KMS of the server-side encryption and its
// keys, the creation of a key reserved to the administrators
// (admin:KMSCreateKey).
type KMSRoutes interface{ Routes }

// KMSDeps are the dependencies of the KMS routes.
type KMSDeps struct {
	// KMS is the key management; nil answers 501 not_implemented.
	KMS adminsvc.KMSManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// KMSOption configures the KMS routes.
type KMSOption func(*kmsRoutes)

type kmsRoutes struct {
	deps  KMSDeps
	guard guard
}

var _ KMSRoutes = (*kmsRoutes)(nil)

// NewKMSRoutes returns the KMS routes.
func NewKMSRoutes(deps KMSDeps, opts ...KMSOption) (KMSRoutes, error) {
	if deps.Authz == nil {
		return nil, fmt.Errorf("%w: KMS routes require the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &kmsRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *kmsRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *kmsRoutes) routes() []route {
	status, list, create, reseal := h.status, h.list, h.create, h.reseal
	if h.deps.KMS == nil {
		ni := notImplemented(h.deps.Enc)
		status, list, create, reseal = ni, ni, ni, ni
	}
	return []route{
		{http.MethodGet, "/kms/status", global(policy.ActionAdminKMSKeyStatus), status},
		{http.MethodGet, "/kms/keys", global(policy.ActionAdminKMSKeyStatus), list},
		{http.MethodPost, "/kms/keys", global(policy.ActionAdminKMSCreateKey), create},
		// Re-sealing the data keys of the objects is key administration.
		{http.MethodPost, "/kms/reseal", global(policy.ActionAdminKMSCreateKey), reseal},
	}
}

// KMSStatusResponse is the body of GET /kms/status.
type KMSStatusResponse struct {
	// Configured is false without KMS: SSE-S3 and SSE-KMS are refused.
	Configured bool `json:"configured"`
	// Backend is "builtin" (master key from the environment) or "kes".
	Backend      string `json:"backend,omitempty"`
	DefaultKeyID string `json:"defaultKeyId,omitempty"`
	// Endpoints are the servers of an external KMS.
	Endpoints []KMSEndpointResponse `json:"endpoints,omitempty"`
	// Online reports that the KMS answers; Error why it does not.
	Online  bool   `json:"online"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
}

// KMSEndpointResponse is a server of an external KMS.
type KMSEndpointResponse struct {
	URL    string `json:"url"`
	Online bool   `json:"online"`
}

// KMSKeyResponse is a key of the KMS.
type KMSKeyResponse struct {
	ID        string     `json:"id"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	// Default marks the key of SSE-S3.
	Default bool `json:"default,omitempty"`
}

// KMSKeyListResponse is the body of GET /kms/keys.
type KMSKeyListResponse struct {
	Items []KMSKeyResponse `json:"items"`
}

// KMSResealResponse is the body of POST /kms/reseal.
type KMSResealResponse struct {
	// Buckets counts the buckets walked.
	Buckets int `json:"buckets"`
	// Versions counts the SSE-S3 and SSE-KMS versions met, Uploads the
	// pending multipart uploads of such objects; Resealed counts the
	// versions and uploads re-sealed under the current master key, Failed
	// those that could not be (FirstError tells why).
	Versions   int    `json:"versions"`
	Uploads    int    `json:"uploads"`
	Resealed   int    `json:"resealed"`
	Failed     int    `json:"failed"`
	FirstError string `json:"firstError,omitempty"`
}

// CreateKMSKeyRequest is the body of POST /kms/keys.
type CreateKMSKeyRequest struct {
	ID string `json:"id"`
}

func newKMSStatusResponse(st kms.Status) KMSStatusResponse {
	out := KMSStatusResponse{
		Configured: st.Configured, Backend: st.Backend, DefaultKeyID: st.DefaultKey,
		Online: st.Online, Error: st.Error, Version: st.Version,
	}
	for _, e := range st.Endpoints {
		out.Endpoints = append(out.Endpoints, KMSEndpointResponse{URL: e, Online: st.Online})
	}
	return out
}

func newKMSKeyResponse(k kms.KeyInfo) KMSKeyResponse {
	out := KMSKeyResponse{ID: k.Name, Default: k.Default}
	if !k.CreatedAt.IsZero() {
		created := k.CreatedAt.UTC()
		out.CreatedAt = &created
	}
	return out
}

func (h *kmsRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

func (h *kmsRoutes) status(w http.ResponseWriter, r *http.Request) {
	st, err := h.deps.KMS.Status(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newKMSStatusResponse(st))
}

func (h *kmsRoutes) list(w http.ResponseWriter, r *http.Request) {
	keys, err := h.deps.KMS.ListKeys(r.Context(), r.URL.Query().Get("prefix"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := KMSKeyListResponse{Items: make([]KMSKeyResponse, 0, len(keys))}
	for _, k := range keys {
		out.Items = append(out.Items, newKMSKeyResponse(k))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *kmsRoutes) create(w http.ResponseWriter, r *http.Request) {
	var in CreateKMSKeyRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	key, err := h.deps.KMS.CreateKey(r.Context(), in.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newKMSKeyResponse(key))
}

// reseal re-seals the data keys of the objects of the bucket of the query
// (every bucket without one) under the current master key.
func (h *kmsRoutes) reseal(w http.ResponseWriter, r *http.Request) {
	rep, err := h.deps.KMS.Reseal(r.Context(), r.URL.Query().Get("bucket"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, KMSResealResponse{
		Buckets: rep.Buckets, Versions: rep.Versions, Uploads: rep.Uploads, Resealed: rep.Resealed, Failed: rep.Failed, FirstError: rep.FirstError,
	})
}
