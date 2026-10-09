package admin

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// CORSRoutes exposes the CORS configuration of the buckets
// (/buckets/{bucket}/cors).
type CORSRoutes interface{ Routes }

// CORSDeps are the dependencies of the CORS routes.
type CORSDeps struct {
	// CORS converts, validates and stores the configurations (required).
	CORS adminsvc.CORSManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// CORSOption configures the CORS routes.
type CORSOption func(*corsRoutes)

type corsRoutes struct {
	deps  CORSDeps
	guard guard
}

var _ CORSRoutes = (*corsRoutes)(nil)

// NewCORSRoutes returns the CORS routes.
func NewCORSRoutes(deps CORSDeps, opts ...CORSOption) (CORSRoutes, error) {
	if deps.CORS == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: CORS routes require the CORS manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &corsRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *corsRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *corsRoutes) routes() []route {
	return []route{
		{http.MethodGet, "/buckets/{bucket}/cors", onBucket(policy.ActionGetBucketCORS), h.get},
		{http.MethodPut, "/buckets/{bucket}/cors", onBucket(policy.ActionPutBucketCORS), h.put},
		// S3 authorizes DeleteBucketCors as s3:PutBucketCORS.
		{http.MethodDelete, "/buckets/{bucket}/cors", onBucket(policy.ActionPutBucketCORS), h.remove},
	}
}

// CORSResponse is the CORS configuration of a bucket, the body of GET and
// PUT /buckets/{bucket}/cors: the rules, evaluated in order.
type CORSResponse struct {
	Rules []CORSRuleResponse `json:"rules"`
}

// CORSRuleResponse is one CORS rule.
type CORSRuleResponse struct {
	ID string `json:"id,omitempty"`
	// AllowedOrigins have at most one "*" each ("*" alone: any origin).
	AllowedOrigins []string `json:"allowedOrigins"`
	// AllowedMethods are among GET, PUT, POST, DELETE and HEAD.
	AllowedMethods []string `json:"allowedMethods"`
	AllowedHeaders []string `json:"allowedHeaders,omitempty"`
	ExposeHeaders  []string `json:"exposeHeaders,omitempty"`
	MaxAgeSeconds  *int     `json:"maxAgeSeconds,omitempty"`
}

func newCORSResponse(cfg *adminsvc.CORSConfig) CORSResponse {
	out := CORSResponse{Rules: make([]CORSRuleResponse, 0, len(cfg.Rules))}
	for _, r := range cfg.Rules {
		out.Rules = append(out.Rules, CORSRuleResponse{
			ID: r.ID, AllowedOrigins: nonNil(r.AllowedOrigins), AllowedMethods: nonNil(r.AllowedMethods),
			AllowedHeaders: r.AllowedHeaders, ExposeHeaders: r.ExposeHeaders, MaxAgeSeconds: r.MaxAgeSeconds,
		})
	}
	return out
}

func (h *corsRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

func (h *corsRoutes) get(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.deps.CORS.Get(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newCORSResponse(cfg))
}

func (h *corsRoutes) put(w http.ResponseWriter, r *http.Request) {
	var in CORSResponse
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	cfg := adminsvc.CORSConfig{Rules: make([]domain.CORSRule, 0, len(in.Rules))}
	for _, rule := range in.Rules {
		cfg.Rules = append(cfg.Rules, domain.CORSRule{
			ID: rule.ID, AllowedOrigins: rule.AllowedOrigins, AllowedMethods: rule.AllowedMethods,
			AllowedHeaders: rule.AllowedHeaders, ExposeHeaders: rule.ExposeHeaders, MaxAgeSeconds: rule.MaxAgeSeconds,
		})
	}
	out, err := h.deps.CORS.Set(r.Context(), bucketName(r), cfg)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newCORSResponse(out))
}

func (h *corsRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.CORS.Delete(r.Context(), bucketName(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
