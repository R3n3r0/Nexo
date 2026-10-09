package admin

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// AccessRuleRoutes exposes the access rules of the buckets (docs/10 §10.1,
// §10.5): /buckets/{bucket}/access-rules (atomic replacement with If-Match
// on the ETag of the rules), the tester, the public presets, and the block
// public access of the buckets and of the cluster.
type AccessRuleRoutes interface{ Routes }

// AccessRuleDeps are the dependencies of the access rules routes.
type AccessRuleDeps struct {
	// Rules manages the access rules (required).
	Rules adminsvc.AccessRuleManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// AccessRuleOption configures the access rules routes.
type AccessRuleOption func(*accessRuleRoutes)

type accessRuleRoutes struct {
	deps  AccessRuleDeps
	guard guard
}

var _ AccessRuleRoutes = (*accessRuleRoutes)(nil)

// NewAccessRuleRoutes returns the access rules routes.
func NewAccessRuleRoutes(deps AccessRuleDeps, opts ...AccessRuleOption) (AccessRuleRoutes, error) {
	if deps.Rules == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: access rules routes require the access rules manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &accessRuleRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *accessRuleRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *accessRuleRoutes) routes() []route {
	manage := policy.ActionAdminManageAccessRules
	return []route{
		{http.MethodGet, "/buckets/{bucket}/access-rules", onBucket(manage), h.get},
		{http.MethodPut, "/buckets/{bucket}/access-rules", onBucket(manage), h.put},
		// The tester authorizes admin:ManageAccessRules on the bucket and,
		// for the requests of another identity, admin:SimulatePolicy: the
		// principal travels in the body.
		{http.MethodPost, "/buckets/{bucket}/access-rules/test", byService(manage, policy.ActionAdminSimulatePolicy), h.test},
		{http.MethodGet, "/buckets/{bucket}/access-rules/public", onBucket(manage), h.getPublic},
		{http.MethodPut, "/buckets/{bucket}/access-rules/public", onBucket(manage), h.putPublic},
		{http.MethodGet, "/buckets/{bucket}/block-public", onBucket(manage), h.getBlockPublic},
		{http.MethodPut, "/buckets/{bucket}/block-public", onBucket(manage), h.putBlockPublic},
		{http.MethodGet, "/access/block-public", global(manage), h.getBlockPublic},
		{http.MethodPut, "/access/block-public", global(manage), h.putBlockPublic},
	}
}

// AccessRulesResponse is the access rules of a bucket (GET and PUT
// /buckets/{bucket}/access-rules, PUT .../access-rules/public); the ETag
// header carries the ETag of the rules, quoted.
type AccessRulesResponse struct {
	Bucket string `json:"bucket"`
	// Rules are the rules as stored, in the order of the document.
	Rules []accessrule.Rule `json:"rules"`
	// ETag identifies the version of the rules (If-Match of PUT).
	ETag      string     `json:"etag"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy string     `json:"updatedBy,omitempty"`
	// Public reports an allow rule for everyone in effect.
	Public bool `json:"public"`
	// BlockPublic is the block public access in effect on the bucket.
	BlockPublic bool `json:"blockPublic"`
	// Warnings describe the rules granting everyone write or delete
	// access and those the block public access ignores.
	Warnings []string `json:"warnings"`
}

func newAccessRulesResponse(c *accessrule.Config) AccessRulesResponse {
	return AccessRulesResponse{
		Bucket: c.Bucket, Rules: nonNil(c.Rules), ETag: c.ETag, UpdatedAt: optTime(c.UpdatedAt), UpdatedBy: c.UpdatedBy,
		Public: c.Public, BlockPublic: c.BlockPublic, Warnings: nonNil(c.Warnings),
	}
}

// PutAccessRulesRequest is the body of PUT /buckets/{bucket}/access-rules:
// every rule of the bucket (none removes them).
type PutAccessRulesRequest struct {
	Rules []accessrule.Rule `json:"rules"`
}

// TestAccessRulesRequest is the body of POST
// /buckets/{bucket}/access-rules/test.
type TestAccessRulesRequest struct {
	// Keys are the keys to test (listing prefixes for a listing action).
	Keys []string `json:"keys,omitempty"`
	// Action is an S3 action or a class (read, list, write, delete, tag).
	Action string `json:"action,omitempty"`
	// Principal is anonymous (default), user:<name>, accesskey:<key> or
	// distribution:<id>.
	Principal    string            `json:"principal,omitempty"`
	IP           string            `json:"ip,omitempty"`
	Host         string            `json:"host,omitempty"`
	Method       string            `json:"method,omitempty"`
	Secure       *bool             `json:"secure,omitempty"`
	Referer      string            `json:"referer,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Distribution string            `json:"distribution,omitempty"`
	// Time is the time of the requests (default: now).
	Time *time.Time `json:"time,omitempty"`
}

// AccessPresetRequest is the body of PUT
// /buckets/{bucket}/access-rules/public.
type AccessPresetRequest struct {
	Preset accessrule.Preset `json:"preset"`
	// Match is the pattern of public-prefix (and optionally public-write).
	Match *match.Pattern `json:"match,omitempty"`
	// Confirm acknowledges public-write.
	Confirm bool `json:"confirm,omitempty"`
}

// BlockPublicRequest is the body of PUT /access/block-public and PUT
// /buckets/{bucket}/block-public.
type BlockPublicRequest struct {
	Enabled bool `json:"enabled"`
	// Mode (cluster level only) is reject or ignore; "" keeps it.
	Mode accessrule.BlockPublicMode `json:"mode,omitempty"`
}

// ifMatch returns the ETag of the If-Match header, unquoted; "" without
// one or for "*".
func ifMatch(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	v = strings.TrimPrefix(v, "W/")
	v = strings.Trim(v, `"`)
	if v == "*" {
		return ""
	}
	return v
}

// setETag sets the ETag header of a document.
func setETag(w http.ResponseWriter, etag string) {
	if etag != "" {
		w.Header().Set("ETag", `"`+etag+`"`)
	}
}

func (h *accessRuleRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

func (h *accessRuleRoutes) writeConfig(w http.ResponseWriter, c *accessrule.Config) {
	setETag(w, c.ETag)
	h.deps.Enc.WriteJSON(w, http.StatusOK, newAccessRulesResponse(c))
}

func (h *accessRuleRoutes) get(w http.ResponseWriter, r *http.Request) {
	c, err := h.deps.Rules.Get(r.Context(), chi.URLParam(r, "bucket"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeConfig(w, c)
}

func (h *accessRuleRoutes) put(w http.ResponseWriter, r *http.Request) {
	var in PutAccessRulesRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	c, err := h.deps.Rules.Put(r.Context(), adminsvc.PutAccessRulesInput{
		Requester: requesterOf(r), Bucket: chi.URLParam(r, "bucket"), Rules: in.Rules, IfMatch: ifMatch(r),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeConfig(w, c)
}

func (h *accessRuleRoutes) test(w http.ResponseWriter, r *http.Request) {
	var in TestAccessRulesRequest
	if err := decodeJSON(h.deps.Enc, r, &in, true); err != nil {
		h.fail(w, r, err)
		return
	}
	tin := adminsvc.TestAccessRulesInput{
		Requester: requesterOf(r), Bucket: chi.URLParam(r, "bucket"), Keys: in.Keys, Action: in.Action, Principal: in.Principal,
		IP: in.IP, Host: in.Host, Method: in.Method, Secure: in.Secure, Referer: in.Referer, Headers: in.Headers, Distribution: in.Distribution,
	}
	if in.Time != nil {
		tin.Time = *in.Time
	}
	res, err := h.deps.Rules.Test(r.Context(), tin)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, res)
}

func (h *accessRuleRoutes) getPublic(w http.ResponseWriter, r *http.Request) {
	st, err := h.deps.Rules.Public(r.Context(), chi.URLParam(r, "bucket"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, st)
}

func (h *accessRuleRoutes) putPublic(w http.ResponseWriter, r *http.Request) {
	var in AccessPresetRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	c, err := h.deps.Rules.ApplyPreset(r.Context(), adminsvc.AccessPresetInput{
		Requester: requesterOf(r), Bucket: chi.URLParam(r, "bucket"), Preset: in.Preset, Match: in.Match, Confirm: in.Confirm, IfMatch: ifMatch(r),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeConfig(w, c)
}

// getBlockPublic answers the block public access of the bucket of the
// path, or of the cluster (/access/block-public).
func (h *accessRuleRoutes) getBlockPublic(w http.ResponseWriter, r *http.Request) {
	bp, err := h.deps.Rules.BlockPublic(r.Context(), chi.URLParam(r, "bucket"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, bp)
}

func (h *accessRuleRoutes) putBlockPublic(w http.ResponseWriter, r *http.Request) {
	var in BlockPublicRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	bp, err := h.deps.Rules.SetBlockPublic(r.Context(), adminsvc.SetBlockPublicInput{
		Requester: requesterOf(r), Bucket: chi.URLParam(r, "bucket"), Enabled: in.Enabled, Mode: in.Mode,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, bp)
}
