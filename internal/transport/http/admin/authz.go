package admin

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// scope tells which resource the actions of a route apply to.
type scope int

const (
	// scopeGlobal actions name no bucket (admin:*, s3:ListAllMyBuckets).
	scopeGlobal scope = iota
	// scopeBucket actions apply to the {bucket} path parameter.
	scopeBucket
	// scopeObject actions apply to the object named by the {bucket} path
	// parameter and the "key" query parameter.
	scopeObject
)

// requirement declares how a route is authorized. Every route of the
// Admin API declares one: the guard evaluates it with the authorization
// service before the handler runs, so that handlers never authorize.
type requirement struct {
	// actions are evaluated together: the first denied one denies.
	actions []policy.Action
	scope   scope
	// versioned replaces actions when the request names a version
	// (versionId query parameter).
	versioned []policy.Action
	// extra adds actions that depend on the request (force deletion,
	// another user's permissions...); extraActions documents them.
	extra        func(r *http.Request) []policy.Action
	extraActions []policy.Action
	// conditions adds operation-specific condition values (s3:prefix...).
	conditions func(r *http.Request) map[string][]string
	// byService marks routes whose resources travel in the body or depend
	// on ownership: the service authorizes them with the same
	// authorization service; the guard only requires an authenticated
	// identity. actions documents what the service checks.
	byService bool
	// partial authorizes with adminsvc.AuthorizeInput.Partial: the routes
	// that show a resource the requester may use in part (the page of a
	// bucket whose listing a home directory restricts).
	partial bool
	// any allows the request when one of actions is allowed (the metrics
	// of the console: admin:Prometheus or admin:ServerInfo).
	any bool
}

// global requires actions on the whole deployment.
func global(actions ...policy.Action) requirement {
	return requirement{actions: actions, scope: scopeGlobal}
}

// onBucket requires actions on the {bucket} of the path.
func onBucket(actions ...policy.Action) requirement {
	return requirement{actions: actions, scope: scopeBucket}
}

// onObject requires actions on the object named by ?key=.
func onObject(actions ...policy.Action) requirement {
	return requirement{actions: actions, scope: scopeObject}
}

// anyOf requires one of actions on the whole deployment.
func anyOf(actions ...policy.Action) requirement {
	return requirement{actions: actions, scope: scopeGlobal, any: true}
}

// anyOnObject requires one of actions on the object named by ?key=.
func anyOnObject(actions ...policy.Action) requirement {
	return requirement{actions: actions, scope: scopeObject, any: true}
}

// byService documents the actions the service authorizes itself.
func byService(actions ...policy.Action) requirement {
	return requirement{actions: actions, byService: true}
}

// authenticated only requires an authenticated identity: the operation
// discloses nothing beyond the caller's own data.
func authenticated() requirement { return requirement{byService: true} }

// orVersion sets the actions used when the request names a version.
func (q requirement) orVersion(actions ...policy.Action) requirement {
	q.versioned = actions
	return q
}

// plus adds request-dependent actions; documented lists the actions extra
// may return.
func (q requirement) plus(extra func(r *http.Request) []policy.Action, documented ...policy.Action) requirement {
	q.extra, q.extraActions = extra, documented
	return q
}

// inPart authorizes the actions when granted under conditions the request
// does not meet (adminsvc.AuthorizeInput.Partial).
func (q requirement) inPart() requirement {
	q.partial = true
	return q
}

// when adds operation-specific conditions.
func (q requirement) when(conditions func(r *http.Request) map[string][]string) requirement {
	q.conditions = conditions
	return q
}

// declared lists every action the requirement may evaluate (the
// x-nexo-actions of the operation in api/openapi.yaml).
func (q requirement) declared() []policy.Action {
	var out []policy.Action
	for _, a := range append(append(append([]policy.Action(nil), q.actions...), q.versioned...), q.extraActions...) {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// route is one endpoint of a route group.
type route struct {
	method  string
	pattern string
	req     requirement
	handler http.HandlerFunc
}

// routeTable is implemented by every route group: Register installs the
// table and the tests check it against the action catalog and the
// OpenAPI document.
type routeTable interface {
	routes() []route
}

// guard authorizes the routes with the authorization service.
type guard struct {
	authz adminsvc.Authorizer
	enc   httpx.Encoder
}

// register installs the routes behind their authorization. The route
// names the operation of the request ("GET /buckets/{bucket}") for the
// audit log, the live trace and the spans (httpx.SetOperationName).
func (g guard) register(r chi.Router, routes []route) {
	for _, rt := range routes {
		r.With(operationName(rt.method+" "+rt.pattern), g.middleware(rt.req)).Method(rt.method, rt.pattern, rt.handler)
	}
}

// operationName records the operation of the requests of a route.
func operationName(name string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpx.SetOperationName(r.Context(), name)
			next.ServeHTTP(w, r)
		})
	}
}

// middleware enforces a requirement.
func (g guard) middleware(req requirement) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := caller(r)
			if id.IsAnonymous || (id.AccessKey == "" && !id.IsRoot) {
				problem(g.enc, w, r, http.StatusUnauthorized, codeUnauthenticated, "authentication required")
				return
			}
			var actions []policy.Action
			if !req.byService {
				actions = req.actions
				if len(req.versioned) > 0 && r.URL.Query().Get("versionId") != "" {
					actions = req.versioned
				}
			}
			if req.extra != nil {
				actions = append(append([]policy.Action(nil), actions...), req.extra(r)...)
			}
			if len(actions) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			in := adminsvc.AuthorizeInput{Requester: requesterOf(r), Actions: actions, Partial: req.partial}
			if req.conditions != nil {
				in.Conditions = req.conditions(r)
			}
			switch req.scope {
			case scopeBucket:
				in.Bucket = chi.URLParam(r, "bucket")
			case scopeObject:
				in.Bucket, in.Object = chi.URLParam(r, "bucket"), r.URL.Query().Get("key")
				if in.Object == "" {
					writeError(g.enc, w, r, fmt.Errorf("%w: the key query parameter is required", domain.ErrInvalidArgument))
					return
				}
				if err := domain.ValidateObjectKey(in.Object); err != nil {
					writeError(g.enc, w, r, err)
					return
				}
				// The condition values of the equivalent S3 request
				// (s3:versionid, the forwarded x-amz-* headers): a policy
				// decides alike through the console and through S3.
				in.Conditions = mergeConditions(adminsvc.ObjectConditions(objectRequestOf(r)), in.Conditions)
			}
			if req.any {
				g.authorizeAny(w, r, next, in)
				return
			}
			d, err := g.authz.Authorize(r.Context(), in)
			if err != nil {
				writeError(g.enc, w, r, err)
				return
			}
			if !d.Allowed {
				problem(g.enc, w, r, http.StatusForbidden, codeAccessDenied, deniedDetail(d))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authorizeAny serves the request when one of the actions of in is
// allowed on its resource, and answers the denial of the first one
// otherwise.
func (g guard) authorizeAny(w http.ResponseWriter, r *http.Request, next http.Handler, in adminsvc.AuthorizeInput) {
	var first adminsvc.Decision
	for i, a := range in.Actions {
		one := in
		one.Actions = []policy.Action{a}
		d, err := g.authz.Authorize(r.Context(), one)
		if err != nil {
			writeError(g.enc, w, r, err)
			return
		}
		if d.Allowed {
			next.ServeHTTP(w, r)
			return
		}
		if i == 0 {
			first = d
		}
	}
	problem(g.enc, w, r, http.StatusForbidden, codeAccessDenied, deniedDetail(first))
}

// mergeConditions returns base overlaid with extra (extra wins).
func mergeConditions(base, extra map[string][]string) map[string][]string {
	out := make(map[string][]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// deniedDetail explains a denial.
func deniedDetail(d adminsvc.Decision) string {
	reason := strings.TrimSpace(d.Reason)
	if reason == "" {
		reason = "not allowed"
	}
	return fmt.Sprintf("%s: %s", d.DeniedAction, reason)
}

// prefixConditions are the listing conditions of a bucket route
// (adminsvc.ListingConditions); an invalid limit is left out, the handler
// rejects it.
func prefixConditions(param string) func(r *http.Request) map[string][]string {
	return func(r *http.Request) map[string][]string {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		return adminsvc.ListingConditions(q.Get(param), q.Get("delimiter"), limit)
	}
}
