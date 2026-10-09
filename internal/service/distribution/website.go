package distribution

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// Outcomes of the website answers (WebResponse.Outcome).
const (
	OutcomeObject      = "object"
	OutcomeRedirect    = "redirect"
	OutcomeNotModified = "not-modified"
	OutcomeError       = "error"
	OutcomeErrorPage   = "error-page"
	OutcomeSPA         = "spa"
	OutcomePreflight   = "preflight"
	OutcomeDenied      = "denied"
	OutcomeUpload      = "upload"
)

// maxUploadSize bounds an upload through a distribution (single PUT).
const maxUploadSize = 5 << 30

// --- compiled distribution -------------------------------------------------------

type compiledBehavior struct {
	pattern CompiledPattern
	eff     Behavior
}

type compiledRedirect struct {
	r  Redirect
	re *regexp.Regexp
}

// compiledDistribution is a distribution ready to serve requests.
type compiledDistribution struct {
	d          *Distribution
	def        Behavior
	behaviors  []compiledBehavior
	redirects  []compiledRedirect
	allow      []netip.Prefix
	deny       []netip.Prefix
	errorPages map[int]ErrorPage
	// keyPrefix is the key prefix of the origin ("prod/" or "").
	keyPrefix string
	// secrets are the SHA-256 of the origin secrets.
	secretHeader string
	secrets      [][]byte
}

// compileDistribution compiles the patterns, expressions and address
// lists of a (validated) distribution; secrets resolves the origin
// secrets given by name (nil: none resolves).
func compileDistribution(ctx context.Context, m Matcher, d *Distribution, secrets ...SecretSource) (*compiledDistribution, error) {
	c := &compiledDistribution{d: d, def: effectiveBehavior(Behavior{}, d.DefaultBehavior), errorPages: map[int]ErrorPage{}}
	for i, b := range d.Behaviors {
		if b.PathPattern == nil {
			return nil, fmt.Errorf("%w: behaviors[%d] has no path pattern", ErrInvalid, i)
		}
		p, err := m.Compile(ctx, *b.PathPattern)
		if err != nil {
			return nil, fmt.Errorf("behaviors[%d]: %w", i, err)
		}
		c.behaviors = append(c.behaviors, compiledBehavior{pattern: p, eff: effectiveBehavior(d.DefaultBehavior, b)})
	}
	for i, r := range d.Redirects {
		cr := compiledRedirect{r: r}
		if r.From != "" {
			re, err := regexp.Compile(r.From)
			if err != nil {
				return nil, fmt.Errorf("%w: redirects[%d]: %v", ErrInvalid, i, err)
			}
			cr.re = re
		}
		c.redirects = append(c.redirects, cr)
	}
	if ip := d.Restrictions.SourceIP; ip != nil {
		var err error
		if c.allow, err = parseIPEntries(ip.Allow); err != nil {
			return nil, err
		}
		if c.deny, err = parseIPEntries(ip.Deny); err != nil {
			return nil, err
		}
	}
	for _, p := range d.ErrorPages {
		c.errorPages[p.Status] = p
	}
	if p := strings.Trim(d.Origin.PathPrefix, "/"); p != "" {
		c.keyPrefix = p + "/"
	}
	if sec := d.Restrictions.OriginSecretHeader; sec != nil {
		c.secretHeader = sec.Name
		for _, v := range sec.Values {
			if name, ok := secretRefName(v); ok {
				// A secret that cannot be resolved matches no request: the
				// distribution fails closed.
				var value string
				for _, src := range secrets {
					if src != nil {
						if value, ok = src.LookupSecret(ctx, name); ok {
							break
						}
					}
				}
				if value == "" {
					continue
				}
				v = value
			}
			if !isHashedSecret(v) {
				v = hashSecret(v)
			}
			sum, _ := hexDecode(strings.TrimPrefix(v, secretHashPrefix))
			c.secrets = append(c.secrets, sum)
		}
	}
	return c, nil
}

// behaviorFor returns the first behavior matching path (-1: the default).
func (c *compiledDistribution) behaviorFor(path string) (int, Behavior) {
	for i, b := range c.behaviors {
		if b.pattern.Match(path) {
			return i, b.eff
		}
	}
	return -1, c.def
}

// responseHeaders are the headers the distribution adds to its answers.
func (c *compiledDistribution) responseHeaders(b Behavior) map[string]string {
	out := make(map[string]string, len(c.d.Headers.Response)+len(b.ResponseHeaders))
	for k, v := range c.d.Headers.Response {
		out[http.CanonicalHeaderKey(k)] = v
	}
	for k, v := range b.ResponseHeaders {
		out[http.CanonicalHeaderKey(k)] = v
	}
	return out
}

// planRequest is the part of a request the plan depends on.
type planRequest struct {
	host, method, path, escapedPath, rawQuery string
	secure                                    bool
	clientIP                                  string
	header                                    http.Header
}

// plan is how a request is served, decided before any I/O.
type plan struct {
	behaviorIndex int
	behavior      Behavior
	// key is the object key of the path (without index document);
	// directory reports a path ending with "/".
	key       string
	directory bool

	redirectStatus   int
	redirectLocation string

	refusalStatus int
	refusal       string
	// admission reports a refusal of the admission checks (disabled,
	// address, origin secret): never answered with an error page.
	admission bool
}

// admit applies the checks of every mode: enabled, source address, origin
// secret. It returns the refusal status and reason (0 when admitted).
func (c *compiledDistribution) admit(req planRequest) (int, string) {
	if !c.d.Enabled {
		return http.StatusForbidden, "the distribution is disabled"
	}
	if len(c.allow) > 0 || len(c.deny) > 0 {
		addr, err := netip.ParseAddr(req.clientIP)
		if err != nil {
			return http.StatusForbidden, "the client address is not allowed"
		}
		addr = addr.Unmap()
		for _, p := range c.deny {
			if p.Contains(addr) {
				return http.StatusForbidden, "the client address is not allowed"
			}
		}
		if len(c.allow) > 0 && !slices.ContainsFunc(c.allow, func(p netip.Prefix) bool { return p.Contains(addr) }) {
			return http.StatusForbidden, "the client address is not allowed"
		}
	}
	if c.secretHeader != "" {
		ok := false
		for _, v := range req.header.Values(c.secretHeader) {
			sum := sha256.Sum256([]byte(v))
			for _, s := range c.secrets {
				if subtle.ConstantTimeCompare(sum[:], s) == 1 {
					ok = true
				}
			}
		}
		if !ok {
			return http.StatusForbidden, "the origin secret is missing or wrong"
		}
	}
	return 0, ""
}

// plan decides how a request to a website distribution is served.
func (c *compiledDistribution) plan(req planRequest) plan {
	p := plan{behaviorIndex: -1, behavior: c.def}
	if status, reason := c.admit(req); status != 0 {
		p.refusalStatus, p.refusal, p.admission = status, reason, true
		return p
	}
	if status, loc := c.redirect(req); status != 0 {
		p.redirectStatus, p.redirectLocation = status, loc
		return p
	}
	path := req.path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	p.behaviorIndex, p.behavior = c.behaviorFor(path)
	if !slices.Contains(p.behavior.AllowedMethods, req.method) {
		p.refusalStatus, p.refusal = http.StatusMethodNotAllowed, "method "+req.method+" is not allowed"
		return p
	}
	rel := path[1:]
	if !safeRelativePath(rel) {
		p.refusalStatus, p.refusal = http.StatusNotFound, "invalid path"
		return p
	}
	if p.behavior.Access == AccessDeny {
		p.refusalStatus, p.refusal = http.StatusForbidden, "access denied by the behavior"
		return p
	}
	p.directory = rel == "" || strings.HasSuffix(rel, "/")
	p.key = c.keyPrefix + rel
	return p
}

// safeRelativePath reports a path that maps to a key below the origin
// prefix: no "." or ".." segment, no NUL, no backslash (a separator on
// some clients).
func safeRelativePath(rel string) bool {
	if strings.ContainsAny(rel, "\x00\\") {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// redirect returns the first redirection that applies to req.
func (c *compiledDistribution) redirect(req planRequest) (int, string) {
	if c.d.Mode != ModeWebsite {
		return 0, ""
	}
	host := normalizeHost(req.host)
	requestURI := req.escapedPath
	if requestURI == "" {
		requestURI = "/"
	}
	if req.rawQuery != "" {
		requestURI += "?" + req.rawQuery
	}
	scheme := "http"
	if req.secure {
		scheme = "https"
	}
	for _, r := range c.redirects {
		if r.r.Status == 0 {
			r.r.Status = http.StatusMovedPermanently
		}
		switch {
		case r.r.HTTPSOnly:
			if !req.secure {
				return r.r.Status, "https://" + host + requestURI
			}
		case r.r.HostFrom != "":
			if host == r.r.HostFrom {
				return r.r.Status, scheme + "://" + r.r.ToHost + requestURI
			}
		case r.re != nil:
			m := r.re.FindStringSubmatchIndex(req.path)
			if m == nil {
				continue
			}
			target := string(r.re.ExpandString(nil, r.r.To, req.path, m))
			if strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") {
				if req.rawQuery != "" && !strings.Contains(target, "?") {
					target += "?" + req.rawQuery
				}
				return r.r.Status, target
			}
			if u, err := url.Parse(target); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
				return r.r.Status, target
			}
			// A target that is neither a path nor an absolute URL is not
			// followed: a protocol-relative "//evil" would leave the site.
		}
	}
	return 0, ""
}

// hexDecode decodes a hex string (an invalid one gives nil).
func hexDecode(s string) ([]byte, error) {
	out := make([]byte, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		hi, lo := unhex(s[i]), unhex(s[i+1])
		if hi < 0 || lo < 0 {
			return nil, errors.New("invalid hex")
		}
		out[i/2] = byte(hi<<4 | lo)
	}
	return out, nil
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// parseTestURL parses the URL of Service.Test.
func parseTestURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: %q is not an http(s) URL", domain.ErrInvalidArgument, raw)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u, nil
}

// --- website engine ----------------------------------------------------------------

// website implements Website.
type website struct {
	deps WebsiteDeps
	opts websiteOptions

	mu sync.Mutex
	// compiled caches the compiled distributions by id, for their ETag.
	compiled map[string]*compiledDistribution
}

var _ Website = (*website)(nil)

// NewWebsite returns the website engine. The object layer, the policy
// engine and the signature verifier are required
// (domain.ErrInvalidArgument).
func NewWebsite(deps WebsiteDeps, opts ...WebsiteOption) (Website, error) {
	switch {
	case deps.Objects == nil:
		return nil, fmt.Errorf("%w: website: the object layer is required", domain.ErrInvalidArgument)
	case deps.Policy == nil:
		return nil, fmt.Errorf("%w: website: the policy engine is required", domain.ErrInvalidArgument)
	case deps.Signatures == nil:
		return nil, fmt.Errorf("%w: website: the signature verifier is required", domain.ErrInvalidArgument)
	}
	if deps.Matcher == nil {
		deps.Matcher = NewMatcher()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	var o websiteOptions
	for _, opt := range opts {
		opt(&o)
	}
	return &website{deps: deps, opts: o, compiled: make(map[string]*compiledDistribution)}, nil
}

// compile returns the compiled form of d, cached by ETag.
func (w *website) compile(ctx context.Context, d *Distribution) (*compiledDistribution, error) {
	if d.ETag == "" {
		return compileDistribution(ctx, w.deps.Matcher, d, w.deps.Secrets)
	}
	w.mu.Lock()
	c, ok := w.compiled[d.ID]
	w.mu.Unlock()
	if ok && c.d.ETag == d.ETag {
		return c, nil
	}
	c, err := compileDistribution(ctx, w.deps.Matcher, d, w.deps.Secrets)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.compiled[d.ID] = c
	w.mu.Unlock()
	return c, nil
}

func toPlanRequest(req WebRequest) planRequest {
	header := req.Header
	if header == nil {
		header = http.Header{}
	}
	return planRequest{
		host: req.Host, method: req.Method, path: req.Path, escapedPath: req.EscapedPath, rawQuery: req.RawQuery,
		secure: req.Secure, clientIP: req.ClientIP, header: header,
	}
}

// Admit implements Website.
func (w *website) Admit(ctx context.Context, req WebRequest) *WebResponse {
	d := req.Route.Distribution
	if d == nil {
		return w.plain(http.StatusMisdirectedRequest, "no distribution serves this host", OutcomeError)
	}
	c, err := w.compile(ctx, d)
	if err != nil {
		w.deps.Logger.ErrorContext(ctx, "distribution: cannot compile", "id", d.ID, "err", err)
		return w.plain(http.StatusInternalServerError, "the distribution configuration is invalid", OutcomeError)
	}
	if status, reason := c.admit(toPlanRequest(req)); status != 0 {
		return w.plain(status, reason, OutcomeDenied)
	}
	if c.def.Access == AccessDeny {
		return w.plain(http.StatusForbidden, "access denied by the distribution", OutcomeDenied)
	}
	return nil
}

// Serve implements Website.
func (w *website) Serve(ctx context.Context, req WebRequest) (*WebResponse, error) {
	d := req.Route.Distribution
	if d == nil {
		return nil, fmt.Errorf("%w: website: the request has no distribution", domain.ErrInvalidArgument)
	}
	if d.Mode != ModeWebsite {
		return nil, fmt.Errorf("%w: website: distribution %s is in the %s mode", domain.ErrInvalidArgument, d.ID, d.Mode)
	}
	c, err := w.compile(ctx, d)
	if err != nil {
		w.deps.Logger.ErrorContext(ctx, "distribution: cannot compile", "id", d.ID, "err", err)
		return w.plain(http.StatusInternalServerError, "the distribution configuration is invalid", OutcomeError), nil
	}
	resp := w.serve(ctx, c, req)
	w.finish(c, req, resp)
	return resp, nil
}

// finish adds the headers of the distribution and of the engine.
func (w *website) finish(c *compiledDistribution, req WebRequest, resp *WebResponse) {
	if resp.Header == nil {
		resp.Header = http.Header{}
	}
	_, b := c.behaviorFor(cleanPath(req.Path))
	for k, v := range c.responseHeaders(b) {
		resp.Header.Set(k, v)
	}
	if w.opts.serverHeader != "" {
		resp.Header.Set("Server", w.opts.serverHeader)
	}
	if resp.Bucket == "" {
		resp.Bucket = c.d.Origin.Bucket
	}
}

func cleanPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// serve answers a request of a compiled website distribution.
func (w *website) serve(ctx context.Context, c *compiledDistribution, req WebRequest) *WebResponse {
	p := c.plan(toPlanRequest(req))
	switch {
	case p.redirectStatus != 0:
		return w.redirectTo(p.redirectStatus, p.redirectLocation)
	case p.refusalStatus == http.StatusMethodNotAllowed:
		resp := w.plain(http.StatusMethodNotAllowed, p.refusal, OutcomeDenied)
		resp.Header.Set("Allow", strings.Join(p.behavior.AllowedMethods, ", "))
		return resp
	case p.admission:
		// Admission refusals (disabled, address, origin secret) never
		// reveal anything through the error pages.
		return w.plain(p.refusalStatus, p.refusal, OutcomeDenied)
	case p.refusalStatus != 0:
		return w.errorPage(ctx, c, req, p.refusalStatus, p.refusal)
	}
	b := p.behavior
	cors := corsRule(c.d.Headers.CORS, b.CORS)
	origin := req.Header.Get("Origin")
	if req.Method == http.MethodOptions {
		return w.options(cors, origin, req, b)
	}
	corsHeaders := simpleCORS(cors, origin)
	resp := w.serveAllowed(ctx, c, req, p)
	for k, v := range corsHeaders {
		resp.Header[k] = v
	}
	return resp
}

// serveAllowed checks the access of the viewer and serves the object.
func (w *website) serveAllowed(ctx context.Context, c *compiledDistribution, req WebRequest, p plan) *WebResponse {
	b := p.behavior
	if b.Access == AccessSigned {
		err := w.deps.Signatures.VerifySignedRequest(ctx, SignedRequest{
			KeyGroup: signingGroup(b), Scheme: schemeOf(req.Secure), Host: req.Host, EscapedPath: req.EscapedPath,
			RawQuery: req.RawQuery, Cookies: cookiesOf(req.Header), ClientIP: req.ClientIP,
		})
		if err != nil {
			w.deps.Logger.DebugContext(ctx, "distribution: signature refused", "id", c.d.ID, "path", req.Path, "err", err)
			return w.errorPage(ctx, c, req, http.StatusForbidden, "a valid signed URL or signed cookie is required")
		}
	}
	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		return w.upload(ctx, c, req, p)
	}
	key := p.key
	if p.directory {
		if b.IndexDocument == "" {
			return w.notFound(ctx, c, req, b)
		}
		key += b.IndexDocument
	}
	if status, reason := w.authorize(ctx, c, req, b, policy.ActionGetObject, key); status != 0 {
		return w.errorPage(ctx, c, req, status, reason)
	}
	resp, notFound := w.readObject(ctx, c, req, b, key, OutcomeObject)
	if !notFound {
		return resp
	}
	// "dir" without its slash: S3 websites redirect to "dir/" when it has
	// an index document.
	if !p.directory && b.IndexDocument != "" {
		if _, err := w.deps.Objects.GetObjectInfo(ctx, c.d.Origin.Bucket, key+"/"+b.IndexDocument, object.GetObjectOptions{}); err == nil {
			loc := req.EscapedPath + "/"
			if req.RawQuery != "" {
				loc += "?" + req.RawQuery
			}
			return w.redirectTo(http.StatusFound, loc)
		}
	}
	return w.notFound(ctx, c, req, b)
}

// notFound answers a missing object: the SPA fallback, or the 404 page.
func (w *website) notFound(ctx context.Context, c *compiledDistribution, req WebRequest, b Behavior) *WebResponse {
	if b.SPAFallback != "" && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		key := c.keyPrefix + strings.TrimPrefix(b.SPAFallback, "/")
		if status, _ := w.authorize(ctx, c, req, b, policy.ActionGetObject, key); status == 0 {
			plainReq := req
			// The fallback is a whole document: the conditions and the
			// range of the request were about another one.
			plainReq.Header = stripConditional(req.Header)
			if resp, missing := w.readObject(ctx, c, plainReq, b, key, OutcomeSPA); !missing {
				return resp
			}
		}
	}
	return w.errorPage(ctx, c, req, http.StatusNotFound, "not found")
}

// stripConditional returns h without the conditional and range headers.
func stripConditional(h http.Header) http.Header {
	out := h.Clone()
	for _, k := range []string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "Range", "If-Range"} {
		out.Del(k)
	}
	return out
}

// authorize decides whether the viewer may perform action on key: an
// explicit deny refuses (access rules, bucket policy), a public or signed
// behavior allows otherwise (the public ones unless the public access of
// the bucket is blocked), an authenticated behavior needs an allow.
func (w *website) authorize(ctx context.Context, c *compiledDistribution, req WebRequest, b Behavior, action policy.Action, key string) (int, string) {
	id := domain.Anonymous()
	authenticated := b.Access == AccessAuthenticated
	if authenticated {
		if req.AuthErr != nil {
			return http.StatusForbidden, "the request signature does not verify"
		}
		if req.Identity == nil || req.Identity.IsAnonymous {
			return http.StatusForbidden, "credentials are required"
		}
		id = req.Identity
	}
	now := w.deps.Clock.Now()
	client := &policy.RequestClient{IP: req.ClientIP, Secure: req.Secure, UserAgent: req.Header.Get("User-Agent"), Referer: req.Header.Get("Referer")}
	conds := policy.RequestConditions(policy.Request{
		Client: client, Now: now, Headers: req.Header, Host: req.Host, Method: req.Method, Distribution: c.d.ID,
		AuthType: authTypeOf(id),
	})
	dec, err := w.deps.Policy.Evaluate(ctx, policy.Args{
		Identity: id, Action: action, Bucket: c.d.Origin.Bucket, Object: key, Conditions: conds, Now: now,
	})
	if err != nil {
		w.deps.Logger.WarnContext(ctx, "distribution: policy evaluation failed", "id", c.d.ID, "key", key, "err", err)
		return http.StatusInternalServerError, "the access cannot be decided"
	}
	if dec.Allowed {
		return 0, ""
	}
	explicit := dec.MatchedStatement != nil || dec.Rule != nil || (dec.Source != "" && dec.Source != policy.SourceInline)
	if authenticated || explicit {
		return http.StatusForbidden, "access denied"
	}
	if b.Access == AccessPublic && w.deps.PublicAccess != nil {
		blocked, err := w.deps.PublicAccess.PublicAccessBlocked(ctx, c.d.Origin.Bucket)
		if err != nil {
			w.deps.Logger.WarnContext(ctx, "distribution: block public access unknown, refusing", "bucket", c.d.Origin.Bucket, "err", err)
			return http.StatusForbidden, "access denied"
		}
		if blocked {
			return http.StatusForbidden, "public access is blocked"
		}
	}
	return 0, ""
}

// authTypeOf is the s3:authType of the viewer identity.
func authTypeOf(id *domain.Identity) domain.AuthType {
	if id == nil || id.IsAnonymous {
		return domain.AuthTypeAnonymous
	}
	return id.AuthType
}

// readObject reads key and builds the answer; notFound reports a missing
// object (no answer then).
func (w *website) readObject(ctx context.Context, c *compiledDistribution, req WebRequest, b Behavior, key, outcome string) (*WebResponse, bool) {
	bucket := c.d.Origin.Bucket
	conds := conditionsOf(req.Header)
	rng := rangeOf(req.Header)
	opts := object.GetObjectOptions{Conditions: conds, Range: rng}
	if req.Method == http.MethodHead {
		info, err := w.deps.Objects.GetObjectInfo(ctx, bucket, key, object.GetObjectOptions{Conditions: conds})
		if err != nil {
			return w.readError(ctx, c, req, b, key, err)
		}
		resp := &WebResponse{Status: http.StatusOK, Header: http.Header{}, Outcome: outcome, Bucket: bucket, Key: key}
		objectHeaders(resp.Header, info, b)
		length := info.Size
		if rng != nil {
			offset, n, err := rng.Resolve(info.Size)
			if err != nil {
				return w.rangeError(info, b), false
			}
			length = n
			resp.Status = http.StatusPartialContent
			resp.Header.Set("Content-Range", domain.ContentRange(offset, n, info.Size))
		}
		resp.Header.Set("Content-Length", strconv.FormatInt(length, 10))
		return resp, false
	}
	rd, err := w.deps.Objects.GetObject(ctx, bucket, key, opts)
	if err != nil {
		return w.readError(ctx, c, req, b, key, err)
	}
	resp := &WebResponse{Status: http.StatusOK, Header: http.Header{}, Body: rd, Outcome: outcome, Bucket: bucket, Key: key}
	objectHeaders(resp.Header, rd.Info, b)
	resp.Header.Set("Content-Length", strconv.FormatInt(rd.Length, 10))
	if rng != nil && rd.Length > 0 {
		resp.Status = http.StatusPartialContent
		resp.Header.Set("Content-Range", domain.ContentRange(rd.Offset, rd.Length, rd.Info.Size))
	}
	return resp, false
}

// readError maps a read error to an answer; notFound for a missing
// object.
func (w *website) readError(ctx context.Context, c *compiledDistribution, req WebRequest, b Behavior, key string, err error) (*WebResponse, bool) {
	bucket := c.d.Origin.Bucket
	var marker *domain.DeleteMarkerError
	switch {
	case errors.Is(err, domain.ErrNotModified):
		resp := &WebResponse{Status: http.StatusNotModified, Header: http.Header{}, Outcome: OutcomeNotModified, Bucket: bucket, Key: key}
		if info, ierr := w.deps.Objects.GetObjectInfo(ctx, bucket, key, object.GetObjectOptions{}); ierr == nil {
			objectHeaders(resp.Header, info, b)
			resp.Header.Del("Content-Type")
			resp.Header.Del("Content-Length")
		}
		return resp, false
	case errors.Is(err, domain.ErrPreconditionFailed):
		return w.plain(http.StatusPreconditionFailed, "precondition failed", OutcomeError), false
	case errors.Is(err, domain.ErrInvalidRange):
		info, ierr := w.deps.Objects.GetObjectInfo(ctx, bucket, key, object.GetObjectOptions{})
		if ierr != nil {
			return w.readError(ctx, c, req, b, key, ierr)
		}
		return w.rangeError(info, b), false
	case errors.As(err, &marker), errors.Is(err, domain.ErrObjectNotFound), errors.Is(err, domain.ErrNoSuchVersion),
		errors.Is(err, domain.ErrBucketNotFound), errors.Is(err, domain.ErrInvalidObjectName):
		return nil, true
	case errors.Is(err, domain.ErrAccessDenied), errors.Is(err, domain.ErrSSECustomerKeyRequired):
		// SSE-C objects need the key of their owner: never served.
		return w.errorPage(ctx, c, req, http.StatusForbidden, "access denied"), false
	case errors.Is(err, domain.ErrReadQuorum), errors.Is(err, domain.ErrWriteQuorum), errors.Is(err, domain.ErrLockTimeout),
		errors.Is(err, domain.ErrDiskNotFound):
		w.deps.Logger.WarnContext(ctx, "distribution: origin unavailable", "id", c.d.ID, "key", key, "err", err)
		resp := w.errorPage(ctx, c, req, http.StatusServiceUnavailable, "service unavailable")
		resp.Header.Set("Retry-After", "1")
		return resp, false
	default:
		w.deps.Logger.ErrorContext(ctx, "distribution: read failed", "id", c.d.ID, "key", key, "err", err)
		return w.errorPage(ctx, c, req, http.StatusInternalServerError, "internal error"), false
	}
}

// rangeError is the 416 of an unsatisfiable range.
func (w *website) rangeError(info domain.ObjectInfo, _ Behavior) *WebResponse {
	resp := w.plain(http.StatusRequestedRangeNotSatisfiable, "requested range not satisfiable", OutcomeError)
	resp.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size))
	return resp
}

// errorPage answers status with the error page of the distribution, when
// it has one for status (GET and HEAD), or with plain text.
func (w *website) errorPage(ctx context.Context, c *compiledDistribution, req WebRequest, status int, reason string) *WebResponse {
	page, ok := c.errorPages[status]
	if !ok || (req.Method != http.MethodGet && req.Method != http.MethodHead) {
		return w.plain(status, reason, OutcomeError)
	}
	key := c.keyPrefix + strings.TrimPrefix(page.Path, "/")
	// The error page is served by the distribution itself: only an
	// explicit deny on it keeps it hidden.
	public := Behavior{Access: AccessPublic}
	if s, _ := w.authorize(ctx, c, req, public, policy.ActionGetObject, key); s != 0 {
		return w.plain(status, reason, OutcomeError)
	}
	plainReq := req
	plainReq.Header = stripConditional(req.Header)
	resp, missing := w.readObject(ctx, c, plainReq, Behavior{Cache: &Cache{CacheControl: "no-store"}}, key, OutcomeErrorPage)
	if missing || resp.Status != http.StatusOK {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return w.plain(status, reason, OutcomeError)
	}
	resp.Status = status
	if page.ResponseStatus != 0 {
		resp.Status = page.ResponseStatus
	}
	resp.Header.Del("ETag")
	resp.Header.Del("Last-Modified")
	resp.Header.Del("Accept-Ranges")
	return resp
}

// upload stores the body of a PUT or POST to a signed behavior that
// allows it.
func (w *website) upload(ctx context.Context, c *compiledDistribution, req WebRequest, p plan) *WebResponse {
	if p.directory {
		return w.plain(http.StatusBadRequest, "cannot upload to a directory", OutcomeError)
	}
	if req.Body == nil || req.ContentLength < 0 {
		return w.plain(http.StatusLengthRequired, "Content-Length is required", OutcomeError)
	}
	if req.ContentLength > maxUploadSize {
		return w.plain(http.StatusRequestEntityTooLarge, "the upload is too large", OutcomeError)
	}
	if status, reason := w.authorize(ctx, c, req, p.behavior, policy.ActionPutObject, p.key); status != 0 {
		return w.plain(status, reason, OutcomeDenied)
	}
	ct := req.Header.Get("Content-Type")
	info, err := w.deps.Objects.PutObject(ctx, object.PutObjectInput{
		Bucket: c.d.Origin.Bucket, Key: p.key, Reader: req.Body, Size: req.ContentLength, ContentType: ct,
	})
	if err != nil {
		w.deps.Logger.WarnContext(ctx, "distribution: upload failed", "id", c.d.ID, "key", p.key, "err", err)
		switch {
		case errors.Is(err, domain.ErrQuotaExceeded), errors.Is(err, domain.ErrEntityTooLarge):
			return w.plain(http.StatusRequestEntityTooLarge, "the upload is too large", OutcomeError)
		case errors.Is(err, domain.ErrInvalidArgument), errors.Is(err, domain.ErrInvalidObjectName):
			return w.plain(http.StatusBadRequest, "invalid upload", OutcomeError)
		case errors.Is(err, domain.ErrAccessDenied), errors.Is(err, domain.ErrObjectLocked):
			return w.plain(http.StatusForbidden, "access denied", OutcomeError)
		case errors.Is(err, domain.ErrWriteQuorum):
			return w.plain(http.StatusServiceUnavailable, "service unavailable", OutcomeError)
		}
		return w.plain(http.StatusInternalServerError, "internal error", OutcomeError)
	}
	resp := w.plain(http.StatusOK, "stored\n", OutcomeUpload)
	resp.Key = p.key
	if info.ETag != "" {
		resp.Header.Set("ETag", `"`+info.ETag+`"`)
	}
	return resp
}

// options answers an OPTIONS request: a CORS preflight, or the methods of
// the behavior.
func (w *website) options(cors *corsPolicy, origin string, req WebRequest, b Behavior) *WebResponse {
	reqMethod := req.Header.Get("Access-Control-Request-Method")
	if origin == "" || reqMethod == "" {
		resp := &WebResponse{Status: http.StatusNoContent, Header: http.Header{}, Outcome: OutcomePreflight}
		resp.Header.Set("Allow", strings.Join(b.AllowedMethods, ", "))
		return resp
	}
	if cors == nil || !cors.rule.MatchesOrigin(origin) || !cors.rule.MatchesMethod(reqMethod) ||
		!cors.rule.MatchesHeaders(splitHeaderList(req.Header.Values("Access-Control-Request-Headers"))) {
		return w.plain(http.StatusForbidden, "CORS preflight refused", OutcomeDenied)
	}
	resp := &WebResponse{Status: http.StatusNoContent, Header: http.Header{}, Outcome: OutcomePreflight}
	for k, v := range simpleCORS(cors, origin) {
		resp.Header[k] = v
	}
	resp.Header.Set("Access-Control-Allow-Methods", strings.Join(cors.rule.AllowedMethods, ", "))
	if h := req.Header.Get("Access-Control-Request-Headers"); h != "" {
		resp.Header.Set("Access-Control-Allow-Headers", h)
	}
	if cors.rule.MaxAgeSeconds != nil {
		resp.Header.Set("Access-Control-Max-Age", strconv.Itoa(*cors.rule.MaxAgeSeconds))
	}
	return resp
}

// corsPolicy is the CORS policy of a request: the rule matching origins,
// methods and headers, and whether credentials are allowed.
type corsPolicy struct {
	rule        domain.CORSRule
	credentials bool
}

// corsRule returns the CORS policy of a behavior (or of the
// distribution); nil when none applies.
func corsRule(dist, behavior *CORS) *corsPolicy {
	c := behavior
	if c == nil {
		c = dist
	}
	if c == nil {
		return nil
	}
	methods := c.AllowedMethods
	if len(methods) == 0 {
		methods = []string{http.MethodGet, http.MethodHead}
	}
	p := &corsPolicy{credentials: c.AllowCredentials, rule: domain.CORSRule{
		AllowedOrigins: c.AllowedOrigins, AllowedMethods: methods, AllowedHeaders: c.AllowedHeaders,
		ExposeHeaders: c.ExposeHeaders,
	}}
	if c.MaxAge > 0 {
		age := c.MaxAge
		p.rule.MaxAgeSeconds = &age
	}
	return p
}

// simpleCORS returns the CORS headers of an actual request from origin.
func simpleCORS(p *corsPolicy, origin string) http.Header {
	h := http.Header{}
	if p == nil || origin == "" {
		return h
	}
	h.Set("Vary", "Origin")
	if !p.rule.MatchesOrigin(origin) {
		return h
	}
	// Credentials never go with "*": the origin is echoed.
	if p.rule.AllowsAnyOrigin() && !p.credentials {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
	}
	if p.credentials {
		h.Set("Access-Control-Allow-Credentials", "true")
	}
	if len(p.rule.ExposeHeaders) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(p.rule.ExposeHeaders, ", "))
	}
	return h
}

func splitHeaderList(values []string) []string {
	var out []string
	for _, v := range values {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// plain is a plain-text answer.
func (w *website) plain(status int, msg, outcome string) *WebResponse {
	body := msg
	if !strings.HasSuffix(body, "\n") {
		body = strconv.Itoa(status) + " " + http.StatusText(status) + ": " + msg + "\n"
	}
	h := http.Header{}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	return &WebResponse{Status: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Outcome: outcome}
}

// redirectTo is a redirection.
func (w *website) redirectTo(status int, location string) *WebResponse {
	resp := w.plain(status, "redirecting to "+location, OutcomeRedirect)
	resp.Header.Set("Location", location)
	return resp
}

// objectHeaders sets the headers describing an object.
func objectHeaders(h http.Header, info domain.ObjectInfo, b Behavior) {
	ct := info.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	if info.ETag != "" {
		h.Set("ETag", `"`+info.ETag+`"`)
	}
	if !info.ModTime.IsZero() {
		h.Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	}
	h.Set("Accept-Ranges", "bytes")
	switch {
	case b.Cache != nil && b.Cache.CacheControl != "":
		h.Set("Cache-Control", b.Cache.CacheControl)
	case info.CacheControl != "":
		h.Set("Cache-Control", info.CacheControl)
	}
	if info.ContentEncoding != "" {
		h.Set("Content-Encoding", info.ContentEncoding)
	}
	if info.ContentDisposition != "" {
		h.Set("Content-Disposition", info.ContentDisposition)
	}
	if info.ContentLanguage != "" {
		h.Set("Content-Language", info.ContentLanguage)
	}
	if !info.Expires.IsZero() {
		h.Set("Expires", info.Expires.UTC().Format(http.TimeFormat))
	}
}

// conditionsOf reads the conditional headers; an unparsable date is
// ignored (RFC 9110).
func conditionsOf(h http.Header) domain.Conditions {
	c := domain.Conditions{IfMatch: h.Get("If-Match"), IfNoneMatch: h.Get("If-None-Match")}
	if t, err := http.ParseTime(h.Get("If-Modified-Since")); err == nil {
		c.IfModifiedSince = t
	}
	if t, err := http.ParseTime(h.Get("If-Unmodified-Since")); err == nil {
		c.IfUnmodifiedSince = t
	}
	return c
}

// rangeOf parses the Range header; a malformed one is ignored (the whole
// object is served, RFC 9110 §14.2).
func rangeOf(h http.Header) *domain.Range {
	v := h.Get("Range")
	if v == "" {
		return nil
	}
	r, err := domain.ParseRange(v)
	if err != nil {
		return nil
	}
	return r
}

// cookiesOf parses the Cookie headers.
func cookiesOf(h http.Header) map[string]string {
	out := map[string]string{}
	for _, c := range (&http.Request{Header: h}).Cookies() {
		if _, dup := out[c.Name]; !dup {
			out[c.Name] = c.Value
		}
	}
	return out
}

func schemeOf(secure bool) string {
	if secure {
		return "https"
	}
	return "http"
}

func signingGroup(b Behavior) string {
	if b.Signing == nil {
		return ""
	}
	return b.Signing.KeyGroup
}
