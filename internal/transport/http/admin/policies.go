package admin

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// PolicyDeps are the dependencies of the policy routes.
type PolicyDeps struct {
	// Policy is both the store of the named policies and the engine that
	// authorizes the admin:* actions of these routes.
	Policy policy.Service
	Enc    httpx.Codec
}

type policyRoutes struct {
	deps  PolicyDeps
	authz authorizer
}

var _ PolicyRoutes = (*policyRoutes)(nil)

// NewPolicyRoutes returns the IAM policy routes.
func NewPolicyRoutes(deps PolicyDeps) (PolicyRoutes, error) {
	if deps.Policy == nil {
		return nil, fmt.Errorf("%w: policy routes require the policy service", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &policyRoutes{deps: deps, authz: authorizer{policy: deps.Policy, enc: deps.Enc}}, nil
}

// Register implements Routes.
func (h *policyRoutes) Register(r chi.Router) {
	r.Get("/policies", h.list)
	r.Post("/policies", h.create)
	r.Post("/policies/attach", h.attach)
	r.Post("/policies/detach", h.detach)
	r.Get("/policies/{policy}", h.get)
	r.Put("/policies/{policy}", h.put)
	r.Delete("/policies/{policy}", h.remove)
}

// PolicyResponse is a named IAM policy. It mirrors the Policy DTO of the
// web console.
type PolicyResponse struct {
	Name      string         `json:"name"`
	Document  *domain.Policy `json:"document"`
	BuiltIn   bool           `json:"builtIn"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

func newPolicyResponse(n *policy.Named) PolicyResponse {
	return PolicyResponse{Name: n.Name, Document: n.Policy, BuiltIn: n.BuiltIn, UpdatedAt: n.UpdatedAt}
}

// SavePolicyRequest is the JSON body of PUT /policies/{policy}.
type SavePolicyRequest struct {
	Document *domain.Policy `json:"document"`
}

// CreatePolicyRequest is the JSON body of POST /policies.
type CreatePolicyRequest struct {
	Name     string         `json:"name"`
	Document *domain.Policy `json:"document"`
}

// AttachRequest is the JSON body of POST /policies/attach and /detach.
// Exactly one of User and Group is set.
type AttachRequest struct {
	PolicyNames []string `json:"policyNames"`
	User        string   `json:"user,omitempty"`
	Group       string   `json:"group,omitempty"`
}

func (h *policyRoutes) list(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminListPolicies, "") {
		return
	}
	named, err := h.deps.Policy.ListPolicies(r.Context())
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	out := make([]PolicyResponse, 0, len(named))
	for i := range named {
		out = append(out, newPolicyResponse(&named[i]))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *policyRoutes) get(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminGetPolicy, "") {
		return
	}
	n, err := h.named(r, chi.URLParam(r, "policy"))
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newPolicyResponse(n))
}

func (h *policyRoutes) create(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminCreatePolicy, "") {
		return
	}
	var in CreatePolicyRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.save(w, r, in.Name, in.Document, http.StatusCreated)
}

func (h *policyRoutes) put(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminCreatePolicy, "") {
		return
	}
	var in SavePolicyRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.save(w, r, chi.URLParam(r, "policy"), in.Document, http.StatusOK)
}

// save validates and stores a named policy, then answers with its record.
func (h *policyRoutes) save(w http.ResponseWriter, r *http.Request, name string, doc *domain.Policy, status int) {
	if name == "" {
		badRequest(h.deps.Enc, w, r, "policy name is required")
		return
	}
	if doc == nil {
		badRequest(h.deps.Enc, w, r, "policy document is required")
		return
	}
	ctx := r.Context()
	if err := h.deps.Policy.Validate(ctx, doc); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	if err := h.deps.Policy.SetPolicy(ctx, name, doc); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	n, err := h.named(r, name)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, status, newPolicyResponse(n))
}

func (h *policyRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminDeletePolicy, "") {
		return
	}
	if err := h.deps.Policy.DeletePolicy(r.Context(), chi.URLParam(r, "policy")); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *policyRoutes) attach(w http.ResponseWriter, r *http.Request) {
	h.bind(w, r, h.deps.Policy.Attach)
}

func (h *policyRoutes) detach(w http.ResponseWriter, r *http.Request) {
	h.bind(w, r, h.deps.Policy.Detach)
}

func (h *policyRoutes) bind(w http.ResponseWriter, r *http.Request, op func(ctx context.Context, in policy.AttachInput) error) {
	if !h.authz.authorize(w, r, policy.ActionAdminAttachPolicy, "") {
		return
	}
	var in AttachRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	if (in.User == "") == (in.Group == "") {
		badRequest(h.deps.Enc, w, r, "exactly one of user and group is required")
		return
	}
	if len(in.PolicyNames) == 0 {
		badRequest(h.deps.Enc, w, r, "policyNames is required")
		return
	}
	if err := op(r.Context(), policy.AttachInput{PolicyNames: in.PolicyNames, User: in.User, Group: in.Group}); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// named returns the stored record of a policy (document, built-in flag and
// update time), or domain.ErrPolicyNotFound.
func (h *policyRoutes) named(r *http.Request, name string) (*policy.Named, error) {
	all, err := h.deps.Policy.ListPolicies(r.Context())
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", domain.ErrPolicyNotFound, name)
}
