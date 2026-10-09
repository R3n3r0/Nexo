package accessrule

import (
	"context"
	"crypto/subtle"
	"net/netip"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// Normalized condition keys read by the rules (lowercase up to the first
// '/', as the policy engine hands them over).
const (
	condSourceIP  = "aws:sourceip"
	condSecure    = "aws:securetransport"
	condReferer   = "aws:referer"
	condPrefix    = "s3:prefix"
	condHost      = "nexo:host"
	condMethod    = "nexo:method"
	condDist      = "nexo:distribution"
	condHeaderPfx = "nexo:header/"
)

// evalRequest is a request prepared for the evaluation of the rules.
type evalRequest struct {
	ctx context.Context
	id  *domain.Identity
	// action is lowercase; lock reports an object lock action.
	action string
	lock   bool
	// key is the normalized key the patterns are matched against.
	key   string
	conds map[string][]string
	now   time.Time
	// method is the HTTP method, uppercase (derived from the action when
	// the request has none).
	method string
	// blocked reports the block public access in effect.
	blocked bool
	secrets SecretStore
}

// first returns the first value of a condition key.
func (r *evalRequest) first(key string) (string, bool) {
	v, ok := r.conds[key]
	if !ok || len(v) == 0 {
		return "", false
	}
	return v[0], true
}

// methodOf is the HTTP method of the S3 request an action stands for.
func methodOf(action string) string {
	switch {
	case strings.Contains(action, "delete"), action == "s3:abortmultipartupload":
		return "DELETE"
	case strings.HasPrefix(action, "s3:put"):
		return "PUT"
	default:
		return "GET"
	}
}

// evaluate evaluates the compiled rules for a request: the fast path
// stops at the first rule that decides; trace evaluates every rule.
func evaluate(cs *compiledSet, req *evalRequest, trace bool) policy.RuleOutcome {
	if trace {
		return evaluateTrace(cs, req)
	}
	var out policy.RuleOutcome
	cs.set.Match(req.key, func(i int) bool {
		r := cs.rules[i]
		ok, _ := r.principalMatches(req)
		return ok && r.actionMatches(req)
	}, func(i int, res match.Result) bool {
		r := cs.rules[i]
		if holds, _ := r.conditionsHold(req, res.Captures); !holds {
			return true
		}
		// Deny rules come first: the first rule that applies decides.
		m := r.match(res.Captures)
		if r.effect == domain.Deny {
			out.Deny = &m
		} else {
			out.Allow = &m
		}
		return false
	})
	return out
}

// evaluateTrace evaluates every rule and reports each outcome.
func evaluateTrace(cs *compiledSet, req *evalRequest) policy.RuleOutcome {
	out := policy.RuleOutcome{Evaluated: make([]policy.EvaluatedRule, 0, len(cs.rules))}
	for i, r := range cs.rules {
		ev := policy.EvaluatedRule{RuleMatch: r.match(nil)}
		ok, blockedOnly := r.principalMatches(req)
		switch {
		case !ok && blockedOnly:
			ev.Mismatch = policy.RuleMismatchBlockedPublic
		case !ok:
			ev.Mismatch = policy.RuleMismatchPrincipal
		case !r.actionMatches(req):
			ev.Mismatch = policy.RuleMismatchAction
		default:
			res := cs.set.Matcher(i).Match(req.key)
			if r.pattern.Type == "" {
				res = match.Result{Matched: true}
			}
			ev.Captures = res.Captures
			if !res.Matched {
				ev.Mismatch = policy.RuleMismatchKey
				break
			}
			holds, failed := r.conditionsHold(req, res.Captures)
			if !holds {
				ev.Mismatch, ev.Condition = policy.RuleMismatchCondition, failed
				break
			}
			ev.Matched = true
			m := ev.RuleMatch
			if r.effect == domain.Deny && out.Deny == nil {
				out.Deny = &m
			}
			if r.effect == domain.Allow && out.Allow == nil {
				out.Allow = &m
			}
		}
		out.Evaluated = append(out.Evaluated, ev)
	}
	return out
}

// principalMatches reports whether the rule names the caller. blockedOnly
// reports a rule that would apply through "*" if public access were not
// blocked.
func (r *compiledRule) principalMatches(req *evalRequest) (ok, blockedOnly bool) {
	if r.everyone {
		if !req.blocked || r.effect != domain.Allow {
			return true, false
		}
		blockedOnly = true
	}
	for _, p := range r.principals {
		if p.matches(req) {
			return true, false
		}
	}
	return false, blockedOnly
}

// matches reports whether the principal names the caller.
func (p principal) matches(req *evalRequest) bool {
	id := req.id
	switch p.kind {
	case PrincipalUserPrefix:
		return !id.IsAnonymous && id.EffectiveUser() != "" && wildcard(p.pattern, id.EffectiveUser())
	case PrincipalGroupPrefix:
		if id.IsAnonymous {
			return false
		}
		for _, g := range id.Groups {
			if g != "" && wildcard(p.pattern, g) {
				return true
			}
		}
		return false
	case PrincipalAccessKeyPrefix:
		return !id.IsAnonymous && id.AccessKey != "" && wildcard(p.pattern, id.AccessKey)
	case PrincipalDistributionPrefix:
		d, ok := req.first(condDist)
		return ok && d != "" && wildcard(p.pattern, d)
	}
	return false
}

// actionMatches reports whether the rule covers the action. An allow rule
// never grants an object lock action.
func (r *compiledRule) actionMatches(req *evalRequest) bool {
	if req.lock && r.effect == domain.Allow {
		return false
	}
	if r.actions[req.action] {
		return true
	}
	for _, p := range r.actionPatterns {
		if wildcard(p, req.action) {
			return true
		}
	}
	return false
}

// conditionsHold evaluates the conditions of the rule. A condition that
// cannot be evaluated never widens access: the rule applies when it
// denies, not when it allows. failed names the condition that decided.
func (r *compiledRule) conditionsHold(req *evalRequest, captures map[string]string) (bool, string) {
	if r.cond == nil {
		return true, ""
	}
	holds, known, failed := r.cond.eval(req, captures)
	if !known {
		return r.effect == domain.Deny, failed
	}
	return holds, failed
}

// eval evaluates every condition: a condition that does not hold decides
// (known), otherwise one that cannot be evaluated makes the outcome
// unknown.
func (c *compiledConditions) eval(req *evalRequest, captures map[string]string) (holds, known bool, failed string) {
	unknown := ""
	if len(c.sourceIP) > 0 || len(c.notSourceIP) > 0 {
		ip, ok := clientIP(req)
		switch {
		case !ok:
			unknown = "sourceIp"
		case len(c.sourceIP) > 0 && !containsIP(c.sourceIP, ip):
			return false, true, "sourceIp"
		case containsIP(c.notSourceIP, ip):
			return false, true, "notSourceIp"
		}
	}
	if len(c.referer) > 0 {
		ref, _ := req.first(condReferer)
		if !anyWildcard(c.referer, strings.ToLower(ref)) {
			return false, true, "referer"
		}
	}
	if len(c.host) > 0 {
		host, _ := req.first(condHost)
		if !hostMatches(c.host, strings.ToLower(host)) {
			return false, true, "host"
		}
	}
	if c.secure != nil {
		v, ok := req.first(condSecure)
		switch {
		case !ok:
			if unknown == "" {
				unknown = "secureTransport"
			}
		case strings.EqualFold(v, "true") != *c.secure:
			return false, true, "secureTransport"
		}
	}
	if (!c.from.IsZero() && req.now.Before(c.from)) || (!c.to.IsZero() && req.now.After(c.to)) {
		return false, true, "timeWindow"
	}
	if len(c.methods) > 0 && !contains(c.methods, req.method) {
		return false, true, "methods"
	}
	for _, h := range c.headers {
		want, ok := expand(req, h.value)
		if !ok {
			if unknown == "" {
				unknown = "headers." + h.name
			}
			continue
		}
		if !headerEquals(req.conds[condHeaderPfx+h.name], want) {
			return false, true, "headers." + h.name
		}
	}
	for _, ce := range c.captures {
		want, ok := expand(req, ce.value)
		if !ok {
			if unknown == "" {
				unknown = "captureEquals." + ce.name
			}
			continue
		}
		got, present := captures[ce.name]
		if !present || got != want {
			return false, true, "captureEquals." + ce.name
		}
	}
	if unknown != "" {
		return false, false, unknown
	}
	return true, true, ""
}

// clientIP returns the client address of the request (aws:SourceIp).
func clientIP(req *evalRequest) (netip.Addr, bool) {
	v, ok := req.first(condSourceIP)
	if !ok {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(strings.TrimSpace(v))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func containsIP(prefixes []netip.Prefix, ip netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func anyWildcard(patterns []string, s string) bool {
	for _, p := range patterns {
		if wildcard(p, s) {
			return true
		}
	}
	return false
}

// hostMatches matches a lowercase host without port against exact hosts
// and "*.domain" patterns (strict subdomains).
func hostMatches(patterns []string, host string) bool {
	if host == "" {
		return false
	}
	for _, p := range patterns {
		if suffix, ok := strings.CutPrefix(p, "*"); ok {
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
			continue
		}
		if p == host {
			return true
		}
	}
	return false
}

// headerEquals compares the values of a header with the expected one in
// constant time.
func headerEquals(values []string, want string) bool {
	found := false
	for _, v := range values {
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(v)), []byte(want)) == 1 {
			found = true
		}
	}
	return found
}

// expand resolves the ${...} references of a condition value: secrets
// (${secret:<name>}), the escapes ${*}, ${?} and ${$}, and condition keys
// of the request (${aws:username}, with an optional default:
// ${key, 'default'}). ok is false when a reference cannot be resolved.
func expand(req *evalRequest, value string) (string, bool) {
	if !strings.Contains(value, "${") {
		return value, true
	}
	var b strings.Builder
	rest := value
	for {
		i := strings.Index(rest, "${")
		if i < 0 {
			b.WriteString(rest)
			return b.String(), true
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return "", false
		}
		b.WriteString(rest[:i])
		v, ok := resolve(req, rest[i+2:i+j])
		if !ok {
			return "", false
		}
		b.WriteString(v)
		rest = rest[i+j+1:]
	}
}

// resolve resolves the inside of a ${...} reference.
func resolve(req *evalRequest, ref string) (string, bool) {
	switch ref {
	case "*", "?", "$":
		return ref, true
	}
	if name, ok := strings.CutPrefix(ref, secretRefPrefix); ok {
		if req.secrets == nil {
			return "", false
		}
		v, err := req.secrets.Secret(req.ctx, name)
		return v, err == nil
	}
	key, def, hasDef := strings.TrimSpace(ref), "", false
	if k, d, found := strings.Cut(ref, ","); found {
		d = strings.TrimSpace(d)
		if len(d) < 2 || d[0] != '\'' || d[len(d)-1] != '\'' {
			return "", false
		}
		key, def, hasDef = strings.TrimSpace(k), d[1:len(d)-1], true
	}
	if v, ok := req.conds[condKey(key)]; ok && len(v) == 1 {
		return v[0], true
	}
	return def, hasDef
}

// condKey normalizes a condition key as the policy engine does: lowercase
// up to the first '/'.
func condKey(k string) string {
	if i := strings.IndexByte(k, '/'); i >= 0 {
		return strings.ToLower(k[:i]) + k[i:]
	}
	return strings.ToLower(k)
}
