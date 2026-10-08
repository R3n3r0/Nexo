package admin

import (
	"net"
	"net/http"
	"strconv"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// maxJSONBody bounds the JSON request bodies of the Admin API.
const maxJSONBody = 1 << 20

// authorizer evaluates the admin:* action of an endpoint with the policy
// engine, using the identity stored in the context by the authentication
// middleware.
type authorizer struct {
	policy policy.Service
	enc    httpx.Encoder
}

// authorize checks that the caller may perform action; bucket is set for
// the bucket endpoints so that statements may scope them. It writes the
// problem response and returns false when access is denied. With no policy
// engine configured every request is allowed (tests).
func (a authorizer) authorize(w http.ResponseWriter, r *http.Request, action policy.Action, bucket string) bool {
	if a.policy == nil {
		return true
	}
	id, ok := middleware.IdentityFromContext(r.Context())
	if !ok {
		a.enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusUnauthorized, Code: "unauthenticated", Detail: "authentication required"})
		return false
	}
	decision, err := a.policy.Evaluate(r.Context(), policy.Args{
		Identity:   id,
		Action:     action,
		Bucket:     bucket,
		Conditions: requestConditions(r),
	})
	if err != nil {
		a.enc.WriteProblemError(w, r, err)
		return false
	}
	if !decision.Allowed {
		a.enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusForbidden, Code: "access_denied", Detail: decision.Reason})
		return false
	}
	return true
}

// caller returns the authenticated identity, or the anonymous identity
// when no authentication middleware ran.
func caller(r *http.Request) *domain.Identity {
	if id, ok := middleware.IdentityFromContext(r.Context()); ok {
		return id
	}
	return domain.Anonymous()
}

// requestConditions builds the policy condition context of a request.
func requestConditions(r *http.Request) map[string][]string {
	conds := map[string][]string{}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		conds["aws:SourceIp"] = []string{host}
	} else if r.RemoteAddr != "" {
		conds["aws:SourceIp"] = []string{r.RemoteAddr}
	}
	conds["aws:SecureTransport"] = []string{strconv.FormatBool(r.TLS != nil)}
	if ua := r.UserAgent(); ua != "" {
		conds["aws:UserAgent"] = []string{ua}
	}
	if ref := r.Referer(); ref != "" {
		conds["aws:Referer"] = []string{ref}
	}
	return conds
}

// nonNil returns s, or an empty slice when s is nil, so that JSON clients
// always receive an array.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// nonNilMap returns m, or an empty map when m is nil, so that JSON clients
// always receive an object.
func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// queryBool parses a boolean query parameter; absent or empty is false.
func queryBool(r *http.Request, name string) bool {
	v := r.URL.Query().Get(name)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// badRequest writes an invalid_argument problem.
func badRequest(enc httpx.Encoder, w http.ResponseWriter, r *http.Request, detail string) {
	enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusBadRequest, Code: "invalid_argument", Detail: detail})
}
