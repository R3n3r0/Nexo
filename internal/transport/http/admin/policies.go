package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// PolicyDeps are the dependencies of the policy routes.
type PolicyDeps struct {
	// Policy is the store of the named policies (list, delete, attach,
	// entities) (required).
	Policy policy.Service
	// Manager saves, validates and simulates policies (required).
	Manager adminsvc.PolicyManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// PolicyOption configures the policy routes (none yet: the type keeps the
// constructor shape of every route group, docs/12 §12.3).
type PolicyOption func(*policyRoutes)

type policyRoutes struct {
	deps  PolicyDeps
	guard guard
}

var _ PolicyRoutes = (*policyRoutes)(nil)

// NewPolicyRoutes returns the IAM policy routes.
func NewPolicyRoutes(deps PolicyDeps, opts ...PolicyOption) (PolicyRoutes, error) {
	if deps.Policy == nil || deps.Manager == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: policy routes require the policy service, the policy manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &policyRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *policyRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *policyRoutes) routes() []route {
	return []route{
		{http.MethodGet, "/policies", global(policy.ActionAdminListUserPolicies), h.list},
		{http.MethodPost, "/policies", global(policy.ActionAdminCreatePolicy), h.create},
		{http.MethodPost, "/policies/attach", global(policy.ActionAdminAttachUserOrGroupPolicy), h.attach},
		{http.MethodPost, "/policies/detach", global(policy.ActionAdminAttachUserOrGroupPolicy), h.detach},
		{http.MethodPost, "/policies/simulate", global(policy.ActionAdminSimulatePolicy), h.simulate},
		// Validation stores nothing and discloses nothing.
		{http.MethodPost, "/policies/validate", authenticated(), h.validate},
		{http.MethodGet, "/policies/{policy}", global(policy.ActionAdminGetPolicy), h.get},
		{http.MethodPut, "/policies/{policy}", global(policy.ActionAdminCreatePolicy), h.put},
		{http.MethodDelete, "/policies/{policy}", global(policy.ActionAdminDeletePolicy), h.remove},
		{http.MethodGet, "/policies/{policy}/entities", global(policy.ActionAdminGetPolicy), h.entities},
	}
}

func (h *policyRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
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

// PolicyListResponse is a page of policies (built-in ones included).
type PolicyListResponse struct {
	Items      []PolicyResponse `json:"items"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// SavePolicyRequest is the JSON body of PUT /policies/{policy}. The
// document is a policy object or its JSON text (a string): errors of a
// text are located by line and column in that text.
type SavePolicyRequest struct {
	Document json.RawMessage `json:"document"`
}

// CreatePolicyRequest is the JSON body of POST /policies.
type CreatePolicyRequest struct {
	Name     string          `json:"name"`
	Document json.RawMessage `json:"document"`
}

// AttachRequest is the JSON body of POST /policies/attach and /detach.
// Exactly one of User and Group is set.
type AttachRequest struct {
	PolicyNames []string `json:"policyNames"`
	User        string   `json:"user,omitempty"`
	Group       string   `json:"group,omitempty"`
}

// PolicyEntitiesResponse lists the users and groups of a policy.
type PolicyEntitiesResponse struct {
	Policy string   `json:"policy"`
	Users  []string `json:"users"`
	Groups []string `json:"groups"`
}

// ValidatePolicyRequest is the JSON body of POST /policies/validate.
type ValidatePolicyRequest struct {
	Document json.RawMessage `json:"document"`
	// Bucket validates a bucket policy of that bucket.
	Bucket string `json:"bucket,omitempty"`
}

// ValidationResponse is the result of POST /policies/validate.
type ValidationResponse struct {
	Valid  bool                  `json:"valid"`
	Errors []PolicyErrorResponse `json:"errors"`
}

// SimulateRequest is the JSON body of POST /policies/simulate.
type SimulateRequest struct {
	// User is the access key to simulate (user or service account); the
	// caller by default.
	User   string `json:"user,omitempty"`
	Action string `json:"action"`
	Bucket string `json:"bucket,omitempty"`
	Object string `json:"object,omitempty"`
	// SourceIP, SecureTransport and Time set aws:SourceIp,
	// aws:SecureTransport and the evaluation time.
	SourceIP        string     `json:"sourceIp,omitempty"`
	SecureTransport *bool      `json:"secureTransport,omitempty"`
	Time            *time.Time `json:"time,omitempty"`
	// Conditions are other condition values (aws:Referer, s3:prefix...).
	Conditions map[string][]string `json:"conditions,omitempty"`
}

// EvaluatedStatementResponse is one statement considered by the
// simulator.
type EvaluatedStatementResponse struct {
	// Source is iam, group, inline or bucket.
	Source     string `json:"source"`
	PolicyName string `json:"policyName,omitempty"`
	Group      string `json:"group,omitempty"`
	// Index is the 0-based position of the statement in its document.
	Index  int    `json:"index"`
	Sid    string `json:"sid,omitempty"`
	Effect string `json:"effect"`
	// Matched reports that the statement applies to the request.
	Matched bool `json:"matched"`
	// Mismatch is the first element that did not match: principal,
	// action, resource or condition.
	Mismatch  string            `json:"mismatch,omitempty"`
	Statement *domain.Statement `json:"statement,omitempty"`
}

// SimulationResponse is the result of POST /policies/simulate.
type SimulationResponse struct {
	User    string `json:"user"`
	Action  string `json:"action"`
	Bucket  string `json:"bucket,omitempty"`
	Object  string `json:"object,omitempty"`
	Allowed bool   `json:"allowed"`
	// Decision is "allow" or "deny".
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	// Source is the origin of the deciding statement: root, iam, group,
	// inline or bucket; empty for an implicit deny.
	Source     string `json:"source,omitempty"`
	PolicyName string `json:"policyName,omitempty"`
	Group      string `json:"group,omitempty"`
	// Statement is the deciding statement (absent for root and implicit
	// denies).
	Statement *EvaluatedStatementResponse `json:"statement,omitempty"`
	// Evaluated lists every statement considered, in evaluation order.
	Evaluated []EvaluatedStatementResponse `json:"evaluated"`
}

// documentText returns the JSON text of a document given as an object or
// as a string holding its text.
func documentText(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("%w: policy document is required", domain.ErrInvalidArgument)
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, fmt.Errorf("%w: policy document: %v", domain.ErrInvalidArgument, err)
		}
		return []byte(text), nil
	}
	return raw, nil
}

func (h *policyRoutes) list(w http.ResponseWriter, r *http.Request) {
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	named, err := h.deps.Manager.List(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := PolicyListResponse{Items: make([]PolicyResponse, 0, len(named.Items)), NextCursor: named.NextCursor}
	for i := range named.Items {
		out.Items = append(out.Items, newPolicyResponse(&named.Items[i]))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *policyRoutes) get(w http.ResponseWriter, r *http.Request) {
	name, err := pathParam(r, "policy")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	n, err := h.deps.Manager.Get(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newPolicyResponse(n))
}

func (h *policyRoutes) create(w http.ResponseWriter, r *http.Request) {
	var in CreatePolicyRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	h.save(w, r, in.Name, in.Document, true, http.StatusCreated)
}

func (h *policyRoutes) put(w http.ResponseWriter, r *http.Request) {
	name, err := pathParam(r, "policy")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in SavePolicyRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	h.save(w, r, name, in.Document, false, http.StatusOK)
}

func (h *policyRoutes) save(w http.ResponseWriter, r *http.Request, name string, raw json.RawMessage, create bool, status int) {
	text, err := documentText(raw)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	n, err := h.deps.Manager.Save(r.Context(), adminsvc.SavePolicyInput{Name: name, Document: text, Create: create})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, status, newPolicyResponse(n))
}

func (h *policyRoutes) remove(w http.ResponseWriter, r *http.Request) {
	name, err := pathParam(r, "policy")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Policy.DeletePolicy(r.Context(), name); err != nil {
		h.fail(w, r, err)
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
	var in AttachRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := op(r.Context(), policy.AttachInput{PolicyNames: in.PolicyNames, User: in.User, Group: in.Group}); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *policyRoutes) entities(w http.ResponseWriter, r *http.Request) {
	name, err := pathParam(r, "policy")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	e, err := h.deps.Policy.Entities(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, PolicyEntitiesResponse{Policy: e.Policy, Users: nonNil(e.Users), Groups: nonNil(e.Groups)})
}

func (h *policyRoutes) validate(w http.ResponseWriter, r *http.Request) {
	var in ValidatePolicyRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	text, err := documentText(in.Document)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rep, err := h.deps.Manager.Validate(r.Context(), adminsvc.ValidatePolicyInput{Document: text, Bucket: in.Bucket})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := ValidationResponse{Valid: rep.Valid, Errors: make([]PolicyErrorResponse, 0, len(rep.Problems))}
	for _, p := range rep.Problems {
		out.Errors = append(out.Errors, newPolicyErrorResponse(p))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func newEvaluatedStatementResponse(e policy.EvaluatedStatement) EvaluatedStatementResponse {
	return EvaluatedStatementResponse{
		Source: string(e.Source), PolicyName: e.PolicyName, Group: e.Group, Index: e.Index, Sid: e.Sid,
		Effect: string(e.Effect), Matched: e.Matched, Mismatch: string(e.Mismatch), Statement: e.Statement,
	}
}

func (h *policyRoutes) simulate(w http.ResponseWriter, r *http.Request) {
	var in SimulateRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	sim := adminsvc.SimulateInput{
		Requester: requesterOf(r), User: in.User, Action: policy.Action(in.Action), Bucket: in.Bucket, Object: in.Object,
		SourceIP: in.SourceIP, SecureTransport: in.SecureTransport, Conditions: in.Conditions,
	}
	if in.Time != nil {
		sim.Now = *in.Time
	}
	res, err := h.deps.Manager.Simulate(r.Context(), sim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	d := res.Decision
	out := SimulationResponse{
		User: res.User, Action: in.Action, Bucket: in.Bucket, Object: in.Object, Allowed: d.Allowed, Decision: "deny",
		Reason: d.Reason, Source: string(d.Source), PolicyName: d.PolicyName, Group: d.Group,
		Evaluated: make([]EvaluatedStatementResponse, 0, len(d.Evaluated)),
	}
	if d.Allowed {
		out.Decision = "allow"
	}
	if d.MatchedStatement != nil {
		out.Statement = &EvaluatedStatementResponse{
			Source: string(d.Source), PolicyName: d.PolicyName, Group: d.Group, Index: d.Index, Sid: d.Sid,
			Effect: string(d.MatchedStatement.Effect), Matched: true, Statement: d.MatchedStatement,
		}
	}
	for _, e := range d.Evaluated {
		out.Evaluated = append(out.Evaluated, newEvaluatedStatementResponse(e))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}
