package admin

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/tier"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// TierRoutes exposes the remote tiers of the lifecycle (S-STO-12): /tiers
// and /tiers/{name}.
type TierRoutes interface{ Routes }

// TierDeps are the dependencies of the tier routes.
type TierDeps struct {
	// Tiers manages the tiers (required).
	Tiers tier.Service
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

type tierRoutes struct {
	deps  TierDeps
	guard guard
}

var _ TierRoutes = (*tierRoutes)(nil)

// NewTierRoutes returns the tier routes.
func NewTierRoutes(deps TierDeps) (TierRoutes, error) {
	if deps.Tiers == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: tier routes require the tier service and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &tierRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}, nil
}

// Register implements Routes.
func (h *tierRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *tierRoutes) routes() []route {
	set := global(policy.ActionAdminSetTier)
	return []route{
		{http.MethodGet, "/tiers", global(policy.ActionAdminListTier), h.list},
		{http.MethodPost, "/tiers", set, h.add},
		{http.MethodPut, "/tiers/{name}", set, h.edit},
		{http.MethodDelete, "/tiers/{name}", set, h.remove},
	}
}

// TierInputBody is the body of POST /tiers and of PUT /tiers/{name} (the
// credentials, the region and the storage class only).
type TierInputBody struct {
	Name         string `json:"name,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	Bucket       string `json:"bucket,omitempty"`
	Prefix       string `json:"prefix,omitempty"`
	Region       string `json:"region,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
	AccessKey    string `json:"accessKey"`
	SecretKey    string `json:"secretKey"`
}

// TiersBody is the answer of GET /tiers.
type TiersBody struct {
	Tiers []tier.Tier `json:"tiers"`
}

func (in TierInputBody) input() tier.Input {
	return tier.Input{Name: in.Name, Endpoint: in.Endpoint, Bucket: in.Bucket, Prefix: in.Prefix, Region: in.Region,
		StorageClass: in.StorageClass, AccessKey: in.AccessKey, SecretKey: in.SecretKey}
}

func (h *tierRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

func (h *tierRoutes) list(w http.ResponseWriter, r *http.Request) {
	tiers, err := h.deps.Tiers.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, TiersBody{Tiers: tiers})
}

func (h *tierRoutes) add(w http.ResponseWriter, r *http.Request) {
	var in TierInputBody
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	t, err := h.deps.Tiers.Add(r.Context(), in.input())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, t)
}

func (h *tierRoutes) edit(w http.ResponseWriter, r *http.Request) {
	var in TierInputBody
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	name, err := pathParam(r, "name")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	t, err := h.deps.Tiers.Edit(r.Context(), name, in.input())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, t)
}

func (h *tierRoutes) remove(w http.ResponseWriter, r *http.Request) {
	name, err := pathParam(r, "name")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Tiers.Remove(r.Context(), name); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
