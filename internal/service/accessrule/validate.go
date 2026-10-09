package accessrule

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/match"
)

// Syntax of the identifiers of the rules, the names of the principals,
// of the headers and of the secrets.
const (
	ruleIDPattern     = `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`
	headerNamePattern = "^[!#$%&'*+.^_`|~0-9A-Za-z-]+$"
	secretNamePattern = `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`
	methodPattern     = `^[A-Z]{1,16}$`
	actionPattern     = `^s3:[A-Za-z*?]+$`
	// maxListLength bounds the principals, actions and condition values
	// of a rule.
	maxListLength = 100
)

// syntax holds the compiled syntax checks.
type syntax struct {
	ruleID, headerName, secretName, method, action *regexp.Regexp
}

func newSyntax() syntax {
	return syntax{
		ruleID:     regexp.MustCompile(ruleIDPattern),
		headerName: regexp.MustCompile(headerNamePattern),
		secretName: regexp.MustCompile(secretNamePattern),
		method:     regexp.MustCompile(methodPattern),
		action:     regexp.MustCompile(actionPattern),
	}
}

// compiler validates and compiles rules.
type compiler struct {
	matcher match.Service
	syn     syntax
}

func newCompiler(m match.Service) *compiler {
	return &compiler{matcher: m, syn: newSyntax()}
}

// normalizeRules validates rules and returns them in canonical form
// (lowercase effects and action classes, trimmed values) with their
// compiled form, in the order of the list.
func (c *compiler) normalizeRules(ctx context.Context, rules []Rule) ([]Rule, []*compiledRule, error) {
	out := make([]Rule, len(rules))
	compiled := make([]*compiledRule, len(rules))
	seen := make(map[string]int, len(rules))
	for i, r := range rules {
		nr, cr, err := c.rule(ctx, i, r)
		if err != nil {
			return nil, nil, err
		}
		if j, dup := seen[nr.ID]; dup {
			return nil, nil, ruleError(i, nr.ID, "id", "duplicate id: rule %d has the same", j+1)
		}
		seen[nr.ID] = i
		cr.index = i
		out[i], compiled[i] = nr, cr
	}
	return out, compiled, nil
}

// rule validates and compiles rule i.
func (c *compiler) rule(ctx context.Context, i int, r Rule) (Rule, *compiledRule, error) {
	r.ID = strings.TrimSpace(r.ID)
	id := r.ID
	if !c.syn.ruleID.MatchString(id) {
		return r, nil, ruleError(i, id, "id", "must be 1-64 letters, digits, '.', '_' or '-', starting with a letter or a digit")
	}
	if r.Priority < 0 || r.Priority > MaxPriority {
		return r, nil, ruleError(i, id, "priority", "must be between 0 and %d", MaxPriority)
	}
	cr := &compiledRule{}
	switch Effect(strings.ToLower(strings.TrimSpace(string(r.Effect)))) {
	case EffectAllow:
		r.Effect, cr.effect = EffectAllow, domain.Allow
	case EffectDeny:
		r.Effect, cr.effect = EffectDeny, domain.Deny
	default:
		return r, nil, ruleError(i, id, "effect", "must be allow or deny, not %q", r.Effect)
	}
	var err error
	if r.Principals, err = c.principals(i, id, r.Principals, cr); err != nil {
		return r, nil, err
	}
	if r.Actions, err = c.actions(i, id, r.Actions, cr); err != nil {
		return r, nil, err
	}
	if r.Match != nil {
		p := *r.Match
		p.Type = match.Type(strings.ToLower(strings.TrimSpace(string(p.Type))))
		if p.Pattern == "" {
			return r, nil, ruleError(i, id, "match.pattern", "is empty: omit match to cover the whole bucket")
		}
		m, err := c.matcher.Compile(ctx, p)
		if err != nil {
			var pe *match.PatternError
			if errors.As(err, &pe) {
				field := "match.pattern"
				if pe.Position == 0 && strings.Contains(pe.Msg, "type") {
					field = "match.type"
				}
				re := ruleError(i, id, field, "%s", pe.Msg)
				re.Position = pe.Position
				return r, nil, re
			}
			return r, nil, ruleError(i, id, "match", "%v", err)
		}
		r.Match = &p
		cr.pattern = p
		cr.groups = m.Groups()
	}
	if r.Conditions != nil {
		cond, err := c.conditions(i, id, r.Conditions, cr.groups)
		if err != nil {
			return r, nil, err
		}
		if cond.empty() {
			r.Conditions = nil
		} else {
			cr.cond = cond
			r.Conditions = canonicalConditions(r.Conditions, cond)
		}
	}
	r.Description = strings.TrimSpace(r.Description)
	cr.rule = r
	return r, cr, nil
}

// principals validates and compiles the principals of a rule.
func (c *compiler) principals(i int, id string, in []string, cr *compiledRule) ([]string, error) {
	if len(in) == 0 {
		return nil, ruleError(i, id, "principals", "at least one principal is required")
	}
	if len(in) > maxListLength {
		return nil, ruleError(i, id, "principals", "at most %d principals", maxListLength)
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for j, p := range in {
		p = strings.TrimSpace(p)
		field := fmt.Sprintf("principals[%d]", j)
		if seen[p] {
			continue
		}
		seen[p] = true
		if p == PrincipalEveryone {
			cr.everyone = true
			out = append(out, p)
			continue
		}
		kind, name, ok := splitPrincipal(p)
		if !ok {
			return nil, ruleError(i, id, field, "%q must be \"*\", user:<name>, group:<name>, accesskey:<key> or distribution:<id>", p)
		}
		if name == "" || strings.ContainsAny(name, " \t\r\n") || len(name) > 256 {
			return nil, ruleError(i, id, field, "%q has an invalid name", p)
		}
		cr.principals = append(cr.principals, principal{kind: kind, pattern: name})
		out = append(out, kind+name)
	}
	return out, nil
}

// splitPrincipal splits "kind:name".
func splitPrincipal(p string) (kind, name string, ok bool) {
	for _, k := range []string{PrincipalUserPrefix, PrincipalGroupPrefix, PrincipalAccessKeyPrefix, PrincipalDistributionPrefix} {
		if len(p) >= len(k) && strings.EqualFold(p[:len(k)], k) {
			return k, p[len(k):], true
		}
	}
	return "", "", false
}

// actions validates and compiles the actions of a rule.
func (c *compiler) actions(i int, id string, in []string, cr *compiledRule) ([]string, error) {
	if len(in) == 0 {
		return nil, ruleError(i, id, "actions", "at least one action is required")
	}
	if len(in) > maxListLength {
		return nil, ruleError(i, id, "actions", "at most %d actions", maxListLength)
	}
	classes := actionClasses()
	out := make([]string, 0, len(in))
	cr.actions = map[string]bool{}
	for j, a := range in {
		a = strings.TrimSpace(a)
		field := fmt.Sprintf("actions[%d]", j)
		if members, ok := classes[strings.ToLower(a)]; ok {
			a = strings.ToLower(a)
			for _, m := range members {
				cr.actions[strings.ToLower(string(m))] = true
			}
			out = append(out, a)
			continue
		}
		if !c.syn.action.MatchString(a) {
			return nil, ruleError(i, id, field, "%q must be read, list, write, delete, tag, all or an S3 action (s3:GetObject, s3:Get*)", a)
		}
		la := strings.ToLower(a)
		if strings.ContainsAny(la, "*?") {
			cr.actionPatterns = append(cr.actionPatterns, la)
		} else {
			cr.actions[la] = true
		}
		out = append(out, a)
	}
	return out, nil
}

// conditions validates and compiles the conditions of a rule; groups are
// the named groups of its pattern (captureEquals).
func (c *compiler) conditions(i int, id string, in *Conditions, groups []string) (*compiledConditions, error) {
	out := &compiledConditions{}
	var err error
	if out.sourceIP, err = prefixes(i, id, "conditions.sourceIp", in.SourceIP); err != nil {
		return nil, err
	}
	if out.notSourceIP, err = prefixes(i, id, "conditions.notSourceIp", in.NotSourceIP); err != nil {
		return nil, err
	}
	if len(in.Referer) > maxListLength || len(in.Host) > maxListLength || len(in.Methods) > maxListLength ||
		len(in.Headers) > maxListLength || len(in.CaptureEquals) > maxListLength {
		return nil, ruleError(i, id, "conditions", "at most %d values per condition", maxListLength)
	}
	for j, r := range in.Referer {
		r = strings.TrimSpace(r)
		if r == "" {
			return nil, ruleError(i, id, fmt.Sprintf("conditions.referer[%d]", j), "is empty")
		}
		out.referer = append(out.referer, strings.ToLower(r))
	}
	for j, h := range in.Host {
		h = strings.ToLower(strings.TrimSpace(h))
		if !validHostPattern(h) {
			return nil, ruleError(i, id, fmt.Sprintf("conditions.host[%d]", j), "%q must be a host name (or *.domain), without port", h)
		}
		out.host = append(out.host, h)
	}
	out.secure = in.SecureTransport
	if tw := in.TimeWindow; tw != nil {
		if out.from, err = parseBound(i, id, "conditions.timeWindow.from", tw.From); err != nil {
			return nil, err
		}
		if out.to, err = parseBound(i, id, "conditions.timeWindow.to", tw.To); err != nil {
			return nil, err
		}
		if !out.from.IsZero() && !out.to.IsZero() && out.to.Before(out.from) {
			return nil, ruleError(i, id, "conditions.timeWindow", "to is before from")
		}
	}
	for j, m := range in.Methods {
		m = strings.ToUpper(strings.TrimSpace(m))
		if !c.syn.method.MatchString(m) {
			return nil, ruleError(i, id, fmt.Sprintf("conditions.methods[%d]", j), "%q is not an HTTP method", m)
		}
		out.methods = append(out.methods, m)
	}
	for _, name := range sortedKeys(in.Headers) {
		value := in.Headers[name]
		field := "conditions.headers." + name
		if !c.syn.headerName.MatchString(name) {
			return nil, ruleError(i, id, field, "%q is not a header name", name)
		}
		if value == "" {
			return nil, ruleError(i, id, field, "the value is empty")
		}
		if err := c.checkTemplate(value); err != nil {
			return nil, ruleError(i, id, field, "%v", err)
		}
		out.headers = append(out.headers, templateCond{name: strings.ToLower(name), value: value})
	}
	for _, group := range sortedKeys(in.CaptureEquals) {
		value := in.CaptureEquals[group]
		field := "conditions.captureEquals." + group
		if !contains(groups, group) {
			return nil, ruleError(i, id, field, "the pattern has no named group %q (a regex with (?P<%s>...))", group, group)
		}
		if err := c.checkTemplate(value); err != nil {
			return nil, ruleError(i, id, field, "%v", err)
		}
		out.captures = append(out.captures, templateCond{name: group, value: value})
	}
	return out, nil
}

// canonicalConditions returns the conditions in canonical form: trimmed
// values, lowercase hosts, uppercase methods, no empty list or map.
func canonicalConditions(in *Conditions, c *compiledConditions) *Conditions {
	trim := func(values []string) []string {
		var out []string
		for _, v := range values {
			out = append(out, strings.TrimSpace(v))
		}
		return out
	}
	out := &Conditions{
		SourceIP:        trim(in.SourceIP),
		NotSourceIP:     trim(in.NotSourceIP),
		Referer:         trim(in.Referer),
		Host:            c.host,
		SecureTransport: in.SecureTransport,
		Methods:         c.methods,
	}
	if tw := in.TimeWindow; tw != nil && (strings.TrimSpace(tw.From) != "" || strings.TrimSpace(tw.To) != "") {
		out.TimeWindow = &TimeWindow{From: strings.TrimSpace(tw.From), To: strings.TrimSpace(tw.To)}
	}
	if len(in.Headers) > 0 {
		out.Headers = in.Headers
	}
	if len(in.CaptureEquals) > 0 {
		out.CaptureEquals = in.CaptureEquals
	}
	return out
}

// prefixes parses CIDRs or IPs.
func prefixes(i int, id, field string, in []string) ([]netip.Prefix, error) {
	if len(in) > maxListLength {
		return nil, ruleError(i, id, field, "at most %d values", maxListLength)
	}
	out := make([]netip.Prefix, 0, len(in))
	for j, s := range in {
		p, err := parsePrefix(s)
		if err != nil {
			return nil, ruleError(i, id, fmt.Sprintf("%s[%d]", field, j), "%q is neither an IP nor a CIDR", s)
		}
		out = append(out, p)
	}
	return out, nil
}

// parsePrefix parses a CIDR, or an IP as a single-address prefix.
func parsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		if p.Addr().Is4In6() {
			return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96), nil
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// parseBound parses an RFC 3339 bound of a time window ("" is open).
func parseBound(i int, id, field, s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, ruleError(i, id, field, "%q is not an RFC 3339 time (2026-01-01T00:00:00Z)", s)
	}
	return t, nil
}

// validHostPattern reports a host name, or "*." followed by one.
func validHostPattern(h string) bool {
	h = strings.TrimPrefix(h, "*.")
	if h == "" || len(h) > 253 || strings.Contains(h, "*") {
		return false
	}
	for _, r := range h {
		if !hostRune(r) {
			return false
		}
	}
	// A port is not part of the host condition (IPv6 addresses aside).
	return !strings.Contains(h, ":") || strings.Count(h, ":") > 1
}

// hostRune reports a character of a lowercase host name or IP address.
func hostRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("-.:[]", r)
}

// checkTemplate checks the ${...} references of a condition value.
func (c *compiler) checkTemplate(value string) error {
	rest := value
	for {
		i := strings.Index(rest, "${")
		if i < 0 {
			return nil
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return fmt.Errorf("unterminated ${ in %q", value)
		}
		ref := rest[i+2 : i+j]
		if name, ok := strings.CutPrefix(ref, secretRefPrefix); ok {
			if !c.syn.secretName.MatchString(name) {
				return fmt.Errorf("invalid secret name %q", name)
			}
		} else if strings.TrimSpace(ref) == "" || strings.ContainsAny(ref, "{$") {
			return fmt.Errorf("invalid reference ${%s}", ref)
		}
		rest = rest[i+j+1:]
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
