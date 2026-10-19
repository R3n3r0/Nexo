package distribution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Limits of a distribution.
const (
	MaxIDLength     = 64
	MaxAliases      = 100
	MaxBehaviors    = 100
	MaxErrorPages   = 50
	MaxRedirects    = 100
	MaxHeaders      = 50
	MaxIPEntries    = 200
	MaxOriginSecret = 8
	// DefaultOriginSecretHeader is the header of the origin secret when the
	// configuration names none.
	DefaultOriginSecretHeader = "X-Origin-Secret"
	// secretHashPrefix marks a hashed origin secret.
	secretHashPrefix = "sha256:"
)

// websiteMethods are the methods a behavior may allow.
func websiteMethods() []string {
	return []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPost, http.MethodPut}
}

// defaultMethods are the methods of a behavior that names none.
func defaultMethods() []string {
	return []string{http.MethodGet, http.MethodHead, http.MethodOptions}
}

// NormalizeHost is the canonical form of a host of a request or of an
// alias, the one the routing table, the cache (its domains, keys and
// purges) and the access logs compare: lowercase, without port and
// trailing dot; an IPv6 literal loses its brackets.
func NormalizeHost(h string) string { return normalizeHost(h) }

// normalizeHost implements NormalizeHost.
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// isIPLiteral reports an IP address host.
func isIPLiteral(h string) bool {
	_, err := netip.ParseAddr(h)
	return err == nil
}

// validHostname reports a DNS host name: labels of letters, digits, '-'
// and '_' (1 to 63 characters, not starting or ending with '-'), at most
// 253 characters, not an IP literal.
func validHostname(h string) bool {
	if h == "" || len(h) > 253 || isIPLiteral(h) {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			alnum := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
			if !alnum && c != '-' && c != '_' {
				return false
			}
		}
	}
	return true
}

// normalizeAlias validates and normalizes an alias: an exact host name or
// "*." followed by a host name of at least two labels.
func normalizeAlias(a string) (string, error) {
	a = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(a)), ".")
	if rest, ok := strings.CutPrefix(a, "*."); ok {
		if !validHostname(rest) || !strings.Contains(rest, ".") {
			return "", fmt.Errorf("%w: wildcard alias %q must be *.<domain> with a domain of at least two labels", ErrInvalid, a)
		}
		return a, nil
	}
	if strings.Contains(a, "*") || !validHostname(a) {
		return "", fmt.Errorf("%w: alias %q is not a host name", ErrInvalid, a)
	}
	return a, nil
}

// validID reports a valid distribution or key group id.
func validID(id string) bool {
	if id == "" || len(id) > MaxIDLength || id == "." || id == ".." {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || (i > 0 && (c == '-' || c == '_' || c == '.'))
		if !ok {
			return false
		}
	}
	return true
}

// normalizePathPrefix returns the origin path prefix as "/a/b" (empty for
// the whole bucket); a prefix with "." or ".." segments, empty segments,
// backslashes or control characters cannot be confined and is refused.
func normalizePathPrefix(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: origin path prefix %q has an empty, '.' or '..' segment", ErrInvalid, p)
		}
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", fmt.Errorf("%w: origin path prefix %q has a control character or a backslash", ErrInvalid, p)
		}
	}
	if err := domain.ValidateObjectPrefix(p + "/"); err != nil {
		return "", fmt.Errorf("%w: origin path prefix: %v", ErrInvalid, err)
	}
	return "/" + p, nil
}

// validDocumentPath checks a path of the distribution (error page, SPA
// fallback): absolute, without '.' or '..' segments.
func validDocumentPath(p string) error {
	if !strings.HasPrefix(p, "/") || len(p) > 1024 {
		return fmt.Errorf("%w: path %q must start with '/'", ErrInvalid, p)
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("%w: path %q has a '.' or '..' segment", ErrInvalid, p)
		}
	}
	return nil
}

// validHeaderName reports an HTTP token.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, c) >= 0 {
			return false
		}
	}
	return true
}

// validHeaderValue reports a header value without control characters.
func validHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		if c := v[i]; (c < ' ' && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// parseIPEntries parses IPs and CIDRs.
func parseIPEntries(list []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, e := range list {
		e = strings.TrimSpace(e)
		if p, err := netip.ParsePrefix(e); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("%w: %q is neither an IP nor a CIDR", ErrInvalid, e)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// hashSecret returns the stored form of an origin secret.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return secretHashPrefix + hex.EncodeToString(sum[:])
}

// isSecretRef reports a reference to a named secret ("${secret:<name>}").
func isSecretRef(v string) bool {
	name, ok := secretRefName(v)
	return ok && name != ""
}

// secretRefName returns the name of a "${secret:<name>}" reference.
func secretRefName(v string) (string, bool) {
	rest, ok := strings.CutPrefix(v, "${secret:")
	if !ok {
		return "", false
	}
	name, ok := strings.CutSuffix(rest, "}")
	if !ok || strings.ContainsAny(name, "${}") {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(name)), true
}

// isHashedSecret reports a stored (hashed) origin secret.
func isHashedSecret(v string) bool {
	h, ok := strings.CutPrefix(v, secretHashPrefix)
	if !ok || len(h) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// validateCORS checks a CORS policy.
func validateCORS(c *CORS, where string) error {
	if c == nil {
		return nil
	}
	if len(c.AllowedOrigins) == 0 {
		return fmt.Errorf("%w: %s CORS: allowedOrigins is required", ErrInvalid, where)
	}
	for _, o := range c.AllowedOrigins {
		if o == "" || strings.Count(o, "*") > 1 || !validHeaderValue(o) {
			return fmt.Errorf("%w: %s CORS: invalid origin %q", ErrInvalid, where, o)
		}
		if c.AllowCredentials && strings.Contains(o, "*") {
			// As browsers and S3: any site matching the pattern would read
			// the answers made with the cookies of the viewer.
			return fmt.Errorf("%w: %s CORS: allowCredentials needs exact origins, not %q", ErrInvalid, where, o)
		}
	}
	for i, m := range c.AllowedMethods {
		m = strings.ToUpper(strings.TrimSpace(m))
		if !slices.Contains([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost, http.MethodDelete}, m) {
			return fmt.Errorf("%w: %s CORS: method %q", ErrInvalid, where, m)
		}
		c.AllowedMethods[i] = m
	}
	for _, h := range append(append([]string(nil), c.AllowedHeaders...), c.ExposeHeaders...) {
		if h != "*" && !validHeaderName(strings.ReplaceAll(h, "*", "x")) {
			return fmt.Errorf("%w: %s CORS: invalid header %q", ErrInvalid, where, h)
		}
	}
	if c.MaxAge < 0 {
		return fmt.Errorf("%w: %s CORS: maxAge must not be negative", ErrInvalid, where)
	}
	return nil
}

// validateBehavior checks and normalizes a behavior; def reports the
// default behavior (no path pattern).
func (s *service) validateBehavior(ctx context.Context, b *Behavior, where string, def bool) error {
	if def && b.PathPattern != nil {
		return fmt.Errorf("%w: the default behavior has no path pattern", ErrInvalid)
	}
	if !def {
		if b.PathPattern == nil {
			return fmt.Errorf("%w: %s: pathPattern is required", ErrInvalid, where)
		}
		b.PathPattern.Type = PatternType(strings.ToLower(string(b.PathPattern.Type)))
		if b.PathPattern.Pattern == "" {
			// An empty prefix would match every path: the default
			// behavior is the place for that.
			return fmt.Errorf("%w: %s: pathPattern.pattern is required", ErrInvalid, where)
		}
		if _, err := s.deps.Matcher.Compile(ctx, *b.PathPattern); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrInvalid, where, err)
		}
	}
	switch b.Access {
	case "", AccessPublic, AccessSigned, AccessAuthenticated, AccessDeny:
	default:
		return fmt.Errorf("%w: %s: access %q must be public, signed, authenticated or deny", ErrInvalid, where, b.Access)
	}
	for i, m := range b.AllowedMethods {
		m = strings.ToUpper(strings.TrimSpace(m))
		if !slices.Contains(websiteMethods(), m) {
			return fmt.Errorf("%w: %s: method %q must be one of %s", ErrInvalid, where, m, strings.Join(websiteMethods(), ", "))
		}
		b.AllowedMethods[i] = m
	}
	if b.IndexDocument != "" && (strings.Contains(b.IndexDocument, "/") || b.IndexDocument == "." || b.IndexDocument == "..") {
		return fmt.Errorf("%w: %s: indexDocument %q must be a file name", ErrInvalid, where, b.IndexDocument)
	}
	if b.SPAFallback != "" {
		if err := validDocumentPath(b.SPAFallback); err != nil {
			return fmt.Errorf("%s: spaFallback: %w", where, err)
		}
	}
	if b.Cache != nil && !validHeaderValue(b.Cache.CacheControl) {
		return fmt.Errorf("%w: %s: invalid cacheControl", ErrInvalid, where)
	}
	if b.Signing != nil {
		b.Signing.KeyGroup = strings.TrimSpace(b.Signing.KeyGroup)
		if !validID(b.Signing.KeyGroup) {
			return fmt.Errorf("%w: %s: signing.keyGroup %q is not a key group id", ErrInvalid, where, b.Signing.KeyGroup)
		}
	}
	if len(b.ResponseHeaders) > MaxHeaders {
		return fmt.Errorf("%w: %s: at most %d response headers", ErrInvalid, where, MaxHeaders)
	}
	for k, v := range b.ResponseHeaders {
		if !validHeaderName(k) || !validHeaderValue(v) {
			return fmt.Errorf("%w: %s: invalid response header %q", ErrInvalid, where, k)
		}
	}
	return validateCORS(b.CORS, where)
}

// normalize validates d and returns its stored form: hosts lowercased,
// methods uppercased, origin secrets hashed. It does not check the
// uniqueness of the aliases (see checkAliases).
func (s *service) normalize(ctx context.Context, d Distribution) (Distribution, error) {
	d = cloneDistribution(d)
	d.ID = strings.TrimSpace(d.ID)
	if !validID(d.ID) {
		return d, fmt.Errorf("%w: id %q: lowercase letters, digits, '.', '_' and '-', at most %d characters", ErrInvalid, d.ID, MaxIDLength)
	}
	switch d.Mode {
	case "":
		d.Mode = ModeWebsite
	case ModeWebsite, ModeS3:
	default:
		return d, fmt.Errorf("%w: mode %q must be website or s3", ErrInvalid, d.Mode)
	}
	if len(d.Aliases) == 0 {
		return d, fmt.Errorf("%w: at least one alias (domain) is required", ErrInvalid)
	}
	if len(d.Aliases) > MaxAliases {
		return d, fmt.Errorf("%w: at most %d aliases", ErrInvalid, MaxAliases)
	}
	seen := map[string]bool{}
	aliases := make([]string, 0, len(d.Aliases))
	for _, a := range d.Aliases {
		n, err := normalizeAlias(a)
		if err != nil {
			return d, err
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		if why := s.reservedAlias(n, d.Origin.Bucket); why != "" {
			return d, fmt.Errorf("%w: alias %q %s", ErrAliasConflict, n, why)
		}
		aliases = append(aliases, n)
	}
	d.Aliases = aliases
	if err := domain.ValidateBucketName(d.Origin.Bucket); err != nil {
		return d, fmt.Errorf("%w: origin bucket: %v", ErrInvalid, err)
	}
	if domain.IsReservedBucket(d.Origin.Bucket) {
		return d, fmt.Errorf("%w: origin bucket %q is reserved", ErrInvalid, d.Origin.Bucket)
	}
	prefix, err := normalizePathPrefix(d.Origin.PathPrefix)
	if err != nil {
		return d, err
	}
	if prefix != "" && d.Mode == ModeS3 {
		return d, fmt.Errorf("%w: origin pathPrefix is supported by the website mode only", ErrInvalid)
	}
	d.Origin.PathPrefix = prefix

	if err := s.validateBehavior(ctx, &d.DefaultBehavior, "defaultBehavior", true); err != nil {
		return d, err
	}
	if err := checkS3Mode(d); err != nil {
		return d, err
	}
	if len(d.Behaviors) > MaxBehaviors {
		return d, fmt.Errorf("%w: at most %d behaviors", ErrInvalid, MaxBehaviors)
	}
	for i := range d.Behaviors {
		where := "behaviors[" + strconv.Itoa(i) + "]"
		if err := s.validateBehavior(ctx, &d.Behaviors[i], where, false); err != nil {
			return d, err
		}
		if err := checkEffective(d.Mode, effectiveBehavior(d.DefaultBehavior, d.Behaviors[i]), where); err != nil {
			return d, err
		}
	}
	if err := checkEffective(d.Mode, effectiveBehavior(Behavior{}, d.DefaultBehavior), "defaultBehavior"); err != nil {
		return d, err
	}

	if len(d.ErrorPages) > MaxErrorPages {
		return d, fmt.Errorf("%w: at most %d error pages", ErrInvalid, MaxErrorPages)
	}
	statuses := map[int]bool{}
	for i, p := range d.ErrorPages {
		if p.Status < 400 || p.Status > 599 {
			return d, fmt.Errorf("%w: errorPages[%d]: status %d must be 4xx or 5xx", ErrInvalid, i, p.Status)
		}
		if statuses[p.Status] {
			return d, fmt.Errorf("%w: errorPages[%d]: status %d listed twice", ErrInvalid, i, p.Status)
		}
		statuses[p.Status] = true
		if p.ResponseStatus != 0 && (p.ResponseStatus < 200 || p.ResponseStatus > 599) {
			return d, fmt.Errorf("%w: errorPages[%d]: responseStatus %d", ErrInvalid, i, p.ResponseStatus)
		}
		if err := validDocumentPath(p.Path); err != nil {
			return d, fmt.Errorf("errorPages[%d]: %w", i, err)
		}
	}

	if len(d.Redirects) > MaxRedirects {
		return d, fmt.Errorf("%w: at most %d redirects", ErrInvalid, MaxRedirects)
	}
	for i := range d.Redirects {
		if err := validateRedirect(&d.Redirects[i]); err != nil {
			return d, fmt.Errorf("redirects[%d]: %w", i, err)
		}
	}

	if len(d.Headers.Response) > MaxHeaders {
		return d, fmt.Errorf("%w: at most %d response headers", ErrInvalid, MaxHeaders)
	}
	for k, v := range d.Headers.Response {
		if !validHeaderName(k) || !validHeaderValue(v) {
			return d, fmt.Errorf("%w: invalid response header %q", ErrInvalid, k)
		}
	}
	if err := validateCORS(d.Headers.CORS, "headers"); err != nil {
		return d, err
	}
	if err := normalizeRestrictions(&d.Restrictions); err != nil {
		return d, err
	}
	switch c := strings.TrimSpace(d.TLS.Certificate); {
	case c == "" || c == CertificateNone || c == CertificateAuto:
		d.TLS.Certificate = c
	case strings.HasPrefix(c, CertificateUploadPrefix) && validID(strings.TrimPrefix(c, CertificateUploadPrefix)):
		d.TLS.Certificate = c
	default:
		return d, fmt.Errorf("%w: tls.certificate %q must be auto, none or upload:<id>", ErrInvalid, c)
	}
	if err := validateTLSChallenge(&d); err != nil {
		return d, err
	}
	switch d.Logging.Format {
	case "", LogFormatJSON, LogFormatCombined:
	default:
		return d, fmt.Errorf("%w: logging.format %q must be json or combined", ErrInvalid, d.Logging.Format)
	}
	return d, nil
}

// reservedAlias tells why an alias (normalized) may not be used by a
// distribution of bucket ("" when it may): the aliases are resolved
// before the virtual hosts and the main domains, so an alias equal to a
// main host (--server-url, --browser-redirect-url) or a wildcard covering
// one, and an alias in the virtual-host space of a --domain (or a
// wildcard covering it) would capture the S3 endpoint of the deployment
// or of other buckets. The virtual host of the origin bucket itself is
// the only exception.
func (s *service) reservedAlias(alias, bucket string) string {
	wild, isWild := strings.CutPrefix(alias, "*.")
	for _, m := range s.opts.mainHosts {
		if (!isWild && alias == m) || (isWild && strings.HasSuffix(m, "."+wild)) {
			return "covers the S3 or console domain " + m + " of the deployment"
		}
	}
	for _, dom := range s.opts.domains {
		switch {
		case !isWild && alias == dom, isWild && (wild == dom || strings.HasSuffix(dom, "."+wild)):
			return "covers the domain " + dom + " of the deployment"
		case isWild && strings.HasSuffix(wild, "."+dom):
			return "covers virtual hosts of the buckets of the domain " + dom
		case !isWild && strings.HasSuffix(alias, "."+dom) && alias != bucket+"."+dom:
			return "is the virtual host of another bucket of the domain " + dom
		}
	}
	return ""
}

// checkS3Mode refuses the settings the s3 mode does not apply: an s3
// distribution is the whole S3 API of its bucket on a custom domain,
// governed by IAM, the bucket policy and the access rules (a deny rule
// keeps a prefix private). Of the behaviors only the access of the
// default one applies: public, authenticated (no anonymous request) or
// deny. Nothing is accepted that would be silently ignored.
func checkS3Mode(d Distribution) error {
	if d.Mode != ModeS3 {
		return nil
	}
	websiteOnly := func(what string) error {
		return fmt.Errorf("%w: %s apply to the website mode only: an s3 distribution is governed by IAM, the bucket policy and the access rules", ErrInvalid, what)
	}
	b := d.DefaultBehavior
	switch {
	case len(d.Behaviors) > 0:
		return websiteOnly("path behaviors (an access rule denies a prefix)")
	case len(d.ErrorPages) > 0:
		return websiteOnly("error pages")
	case len(d.Redirects) > 0:
		return websiteOnly("redirects")
	case len(d.Headers.Response) > 0 || len(b.ResponseHeaders) > 0:
		return websiteOnly("response headers")
	case d.Headers.CORS != nil || b.CORS != nil:
		return websiteOnly("CORS settings (the CORS configuration of the bucket applies)")
	case len(b.AllowedMethods) > 0:
		return websiteOnly("allowed methods (the S3 API decides the methods)")
	case b.IndexDocument != "" || b.SPAFallback != "":
		return websiteOnly("index documents and SPA fallbacks")
	case b.Cache != nil:
		return websiteOnly("behavior cache settings (the cache settings of the distribution apply)")
	case b.Signing != nil:
		return websiteOnly("signed URLs")
	case b.Images != nil && b.Images.Negotiate:
		return websiteOnly("image variants on the URL of the image (the S3 API takes x-nexo-image-format and x-nexo-image-width)")
	}
	switch b.Access {
	case "", AccessPublic, AccessAuthenticated, AccessDeny:
		return nil
	}
	return fmt.Errorf("%w: defaultBehavior.access %q: the s3 mode supports public, authenticated or deny", ErrInvalid, b.Access)
}

// checkEffective checks the rules that depend on the inherited fields of
// a behavior: a signed behavior names its key group, POST and PUT need a
// signed behavior in the website mode.
func checkEffective(mode Mode, eff Behavior, where string) error {
	if eff.Access == AccessSigned && (eff.Signing == nil || eff.Signing.KeyGroup == "") {
		return fmt.Errorf("%w: %s: a signed behavior needs signing.keyGroup", ErrInvalid, where)
	}
	writes := slices.Contains(eff.AllowedMethods, http.MethodPost) || slices.Contains(eff.AllowedMethods, http.MethodPut)
	if mode == ModeWebsite && writes && eff.Access != AccessSigned {
		return fmt.Errorf("%w: %s: POST and PUT need access: signed", ErrInvalid, where)
	}
	return nil
}

// validateRedirect checks a redirect rule.
func validateRedirect(r *Redirect) error {
	switch r.Status {
	case 0:
		r.Status = http.StatusMovedPermanently
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return fmt.Errorf("%w: status %d must be 301, 302, 303, 307 or 308", ErrInvalid, r.Status)
	}
	kinds := 0
	if r.From != "" || r.To != "" {
		kinds++
		if r.From == "" || r.To == "" {
			return fmt.Errorf("%w: from and to go together", ErrInvalid)
		}
		if len(r.From) > MaxPatternLength {
			return fmt.Errorf("%w: from longer than %d characters", ErrInvalid, MaxPatternLength)
		}
		if _, err := regexp.Compile(r.From); err != nil {
			return fmt.Errorf("%w: from: invalid regular expression: %v", ErrInvalid, err)
		}
		if !validHeaderValue(r.To) {
			return fmt.Errorf("%w: to has control characters", ErrInvalid)
		}
	}
	if r.HostFrom != "" || r.ToHost != "" {
		kinds++
		from, to := normalizeHost(r.HostFrom), normalizeHost(r.ToHost)
		if !validHostname(from) || !validHostname(to) {
			return fmt.Errorf("%w: hostFrom and toHost must be host names", ErrInvalid)
		}
		r.HostFrom, r.ToHost = from, to
	}
	if r.HTTPSOnly {
		kinds++
	}
	if kinds != 1 {
		return fmt.Errorf("%w: a redirect is one of from/to, hostFrom/toHost or httpsOnly", ErrInvalid)
	}
	return nil
}

// normalizeRestrictions checks the restrictions and hashes the origin
// secrets given in clear.
func normalizeRestrictions(r *Restrictions) error {
	if ip := r.SourceIP; ip != nil {
		if len(ip.Allow)+len(ip.Deny) > MaxIPEntries {
			return fmt.Errorf("%w: at most %d source IP entries", ErrInvalid, MaxIPEntries)
		}
		if _, err := parseIPEntries(ip.Allow); err != nil {
			return fmt.Errorf("restrictions.sourceIp.allow: %w", err)
		}
		if _, err := parseIPEntries(ip.Deny); err != nil {
			return fmt.Errorf("restrictions.sourceIp.deny: %w", err)
		}
	}
	if sec := r.OriginSecretHeader; sec != nil {
		if sec.Name == "" {
			sec.Name = DefaultOriginSecretHeader
		}
		if !validHeaderName(sec.Name) {
			return fmt.Errorf("%w: restrictions.originSecretHeader.name %q", ErrInvalid, sec.Name)
		}
		sec.Name = http.CanonicalHeaderKey(sec.Name)
		values := sec.Values
		if sec.Value != "" {
			values = append([]string{sec.Value}, values...)
		}
		sec.Value = ""
		out := make([]string, 0, len(values))
		for _, v := range values {
			if v == "" || !validHeaderValue(v) {
				return fmt.Errorf("%w: restrictions.originSecretHeader: empty or invalid secret", ErrInvalid)
			}
			if !isHashedSecret(v) && !isSecretRef(v) {
				v = hashSecret(v)
			}
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
		if len(out) == 0 {
			return fmt.Errorf("%w: restrictions.originSecretHeader needs at least one secret", ErrInvalid)
		}
		if len(out) > MaxOriginSecret {
			return fmt.Errorf("%w: at most %d origin secrets", ErrInvalid, MaxOriginSecret)
		}
		sec.Values = out
	}
	if rl := r.RateLimit; rl != nil {
		if rl.RequestsPerSecond < 0 || rl.Burst < 0 {
			return fmt.Errorf("%w: restrictions.rateLimit must not be negative", ErrInvalid)
		}
		switch rl.Per {
		case "", RateLimitPerIP, RateLimitPerDistribution:
		default:
			return fmt.Errorf("%w: restrictions.rateLimit.per %q must be ip or distribution", ErrInvalid, rl.Per)
		}
	}
	return nil
}

// effectiveBehavior returns b with the unset fields inherited from def.
func effectiveBehavior(def, b Behavior) Behavior {
	out := b
	if out.Access == "" {
		out.Access = def.Access
	}
	if len(out.AllowedMethods) == 0 {
		out.AllowedMethods = def.AllowedMethods
	}
	if out.IndexDocument == "" {
		out.IndexDocument = def.IndexDocument
	}
	if out.SPAFallback == "" {
		out.SPAFallback = def.SPAFallback
	}
	if out.Cache == nil {
		out.Cache = def.Cache
	}
	if out.Signing == nil {
		out.Signing = def.Signing
	}
	if out.CORS == nil {
		out.CORS = def.CORS
	}
	if out.Images == nil {
		out.Images = def.Images
	}
	if len(def.ResponseHeaders) > 0 {
		merged := make(map[string]string, len(def.ResponseHeaders)+len(b.ResponseHeaders))
		for k, v := range def.ResponseHeaders {
			merged[k] = v
		}
		for k, v := range b.ResponseHeaders {
			merged[k] = v
		}
		out.ResponseHeaders = merged
	}
	if out.Access == "" {
		out.Access = AccessPublic
	}
	if len(out.AllowedMethods) == 0 {
		out.AllowedMethods = defaultMethods()
	}
	return out
}

// cloneDistribution returns a deep copy of d (the slices and maps of the
// result are not shared).
func cloneDistribution(d Distribution) Distribution {
	out := d
	out.Aliases = slices.Clone(d.Aliases)
	out.DefaultBehavior = cloneBehavior(d.DefaultBehavior)
	if d.Behaviors != nil {
		out.Behaviors = make([]Behavior, len(d.Behaviors))
		for i, b := range d.Behaviors {
			out.Behaviors[i] = cloneBehavior(b)
		}
	}
	out.ErrorPages = slices.Clone(d.ErrorPages)
	out.Redirects = slices.Clone(d.Redirects)
	out.Headers.Response = cloneMap(d.Headers.Response)
	out.Headers.CORS = cloneCORS(d.Headers.CORS)
	if ip := d.Restrictions.SourceIP; ip != nil {
		out.Restrictions.SourceIP = &IPRestriction{Allow: slices.Clone(ip.Allow), Deny: slices.Clone(ip.Deny)}
	}
	if sec := d.Restrictions.OriginSecretHeader; sec != nil {
		c := *sec
		c.Values = slices.Clone(sec.Values)
		out.Restrictions.OriginSecretHeader = &c
	}
	if rl := d.Restrictions.RateLimit; rl != nil {
		c := *rl
		out.Restrictions.RateLimit = &c
	}
	return out
}

func cloneBehavior(b Behavior) Behavior {
	out := b
	if b.PathPattern != nil {
		p := *b.PathPattern
		out.PathPattern = &p
	}
	out.AllowedMethods = slices.Clone(b.AllowedMethods)
	if b.Cache != nil {
		c := *b.Cache
		out.Cache = &c
	}
	if b.Signing != nil {
		s := *b.Signing
		out.Signing = &s
	}
	out.ResponseHeaders = cloneMap(b.ResponseHeaders)
	out.CORS = cloneCORS(b.CORS)
	if b.Images != nil {
		i := *b.Images
		out.Images = &i
	}
	return out
}

func cloneCORS(c *CORS) *CORS {
	if c == nil {
		return nil
	}
	out := *c
	out.AllowedOrigins = slices.Clone(c.AllowedOrigins)
	out.AllowedMethods = slices.Clone(c.AllowedMethods)
	out.AllowedHeaders = slices.Clone(c.AllowedHeaders)
	out.ExposeHeaders = slices.Clone(c.ExposeHeaders)
	return &out
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// validateTLSChallenge checks the ACME challenge of the certificate
// "auto": a known challenge, a DNS provider with dns-01 only, and dns-01
// for the wildcard aliases (the only challenge proving the control of a
// whole domain).
func validateTLSChallenge(d *Distribution) error {
	d.TLS.Challenge = strings.ToLower(strings.TrimSpace(d.TLS.Challenge))
	d.TLS.DNSProvider = strings.TrimSpace(d.TLS.DNSProvider)
	if d.TLS.Certificate != CertificateAuto {
		if d.TLS.Challenge != "" || d.TLS.DNSProvider != "" {
			return fmt.Errorf("%w: tls.challenge and tls.dnsProvider apply to the certificate auto only", ErrInvalid)
		}
		return nil
	}
	switch d.TLS.Challenge {
	case "", ChallengeHTTP01, ChallengeTLSALPN01:
		if d.TLS.DNSProvider != "" {
			return fmt.Errorf("%w: tls.dnsProvider needs tls.challenge %s", ErrInvalid, ChallengeDNS01)
		}
	case ChallengeDNS01:
		if !validID(d.TLS.DNSProvider) {
			return fmt.Errorf("%w: tls.challenge %s needs tls.dnsProvider, the name of a DNS provider", ErrInvalid, ChallengeDNS01)
		}
	default:
		return fmt.Errorf("%w: tls.challenge %q must be %s, %s or %s", ErrInvalid, d.TLS.Challenge, ChallengeHTTP01, ChallengeTLSALPN01, ChallengeDNS01)
	}
	if d.TLS.Challenge != ChallengeDNS01 {
		for _, a := range d.Aliases {
			if strings.HasPrefix(a, "*.") {
				return fmt.Errorf("%w: the wildcard alias %s needs tls.challenge %s with a tls.dnsProvider for its automatic certificate", ErrInvalid, a, ChallengeDNS01)
			}
		}
	}
	return nil
}
