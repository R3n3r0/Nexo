package accessrule

import (
	"context"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// secretRefPrefix starts the references to a secret: ${secret:<name>}.
const secretRefPrefix = "secret:"

// actionClasses maps the action classes to their S3 actions.
func actionClasses() map[string][]policy.Action {
	read := []policy.Action{policy.ActionGetObject, policy.ActionGetObjectVersion,
		policy.ActionGetObjectAttributes, policy.ActionGetObjectVersionAttributes}
	list := []policy.Action{policy.ActionListBucket, policy.ActionListBucketVersions, policy.ActionListMultipart}
	write := []policy.Action{policy.ActionPutObject, policy.ActionAbortMultipart, policy.ActionListMultipartParts}
	del := []policy.Action{policy.ActionDeleteObject, policy.ActionDeleteObjectVersion}
	tag := []policy.Action{policy.ActionGetObjectTagging, policy.ActionPutObjectTagging, policy.ActionDeleteObjectTagging,
		policy.ActionGetObjectVersionTagging, policy.ActionPutObjectVersionTagging, policy.ActionDeleteObjectVersionTagging}
	all := make([]policy.Action, 0, len(read)+len(list)+len(write)+len(del)+len(tag))
	for _, c := range [][]policy.Action{read, list, write, del, tag} {
		all = append(all, c...)
	}
	return map[string][]policy.Action{
		ActionRead: read, ActionList: list, ActionWrite: write, ActionDelete: del, ActionTag: tag, ActionAll: all,
	}
}

// lockActions are the object lock actions an access rule never grants
// (lowercase).
func lockActions() map[string]bool {
	out := map[string]bool{}
	for _, a := range []policy.Action{policy.ActionBypassGovernanceRetention, policy.ActionPutObjectRetention,
		policy.ActionPutObjectLegalHold, policy.ActionPutBucketObjectLock} {
		out[strings.ToLower(string(a))] = true
	}
	return out
}

// listingActions are the bucket-level actions whose key is the listing
// prefix (lowercase).
func listingActions() map[string]bool {
	return map[string]bool{
		strings.ToLower(string(policy.ActionListBucket)):         true,
		strings.ToLower(string(policy.ActionListBucketVersions)): true,
		strings.ToLower(string(policy.ActionListMultipart)):      true,
	}
}

// principal is a principal of a rule other than "*".
type principal struct {
	// kind is one of the Principal*Prefix constants.
	kind string
	// pattern is the name, with "*" and "?" wildcards.
	pattern string
}

// templateCond is a header or capture condition: a name and the value it
// must have (a template with ${...} references).
type templateCond struct {
	name  string
	value string
}

// compiledConditions are the parsed conditions of a rule.
type compiledConditions struct {
	sourceIP    []netip.Prefix
	notSourceIP []netip.Prefix
	// referer and host are lowercase patterns.
	referer  []string
	host     []string
	secure   *bool
	from, to time.Time
	// methods are uppercase.
	methods  []string
	headers  []templateCond
	captures []templateCond
}

// empty reports conditions that constrain nothing.
func (c *compiledConditions) empty() bool {
	return len(c.sourceIP) == 0 && len(c.notSourceIP) == 0 && len(c.referer) == 0 && len(c.host) == 0 &&
		c.secure == nil && c.from.IsZero() && c.to.IsZero() && len(c.methods) == 0 && len(c.headers) == 0 && len(c.captures) == 0
}

// compiledRule is a validated rule ready for evaluation.
type compiledRule struct {
	rule Rule
	// index is the position of the rule in its document.
	index  int
	effect domain.Effect
	// everyone reports the principal "*".
	everyone   bool
	principals []principal
	// actions are the lowercase S3 actions (classes expanded),
	// actionPatterns the lowercase actions with wildcards.
	actions        map[string]bool
	actionPatterns []string
	// pattern is the key pattern (Type "" when the rule has none).
	pattern match.Pattern
	groups  []string
	cond    *compiledConditions
}

// match returns the RuleMatch of the rule, with captures.
func (r *compiledRule) match(captures map[string]string) policy.RuleMatch {
	return policy.RuleMatch{ID: r.rule.ID, Priority: r.rule.Priority, Effect: r.effect, Captures: captures}
}

// public reports an allow rule for everyone.
func (r *compiledRule) public() bool { return r.everyone && r.effect == domain.Allow }

// compiledSet is the compiled rules of a bucket, in evaluation order:
// deny rules first, then allow rules, each by priority and then by
// position in the document. The pattern of index i of set is the one of
// rules[i] (the empty prefix, matching every key, for the rules without
// a pattern).
type compiledSet struct {
	etag   string
	rules  []*compiledRule
	set    match.Set
	public bool
}

// compileSet orders compiled rules and indexes their patterns.
func compileSet(ctx context.Context, m match.Service, etag string, rules []*compiledRule) (*compiledSet, error) {
	ordered := append([]*compiledRule(nil), rules...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if (a.effect == domain.Deny) != (b.effect == domain.Deny) {
			return a.effect == domain.Deny
		}
		if a.rule.Priority != b.rule.Priority {
			return a.rule.Priority < b.rule.Priority
		}
		return a.index < b.index
	})
	patterns := make([]match.Pattern, len(ordered))
	cs := &compiledSet{etag: etag, rules: ordered}
	for i, r := range ordered {
		patterns[i] = r.pattern
		if r.pattern.Type == "" {
			patterns[i] = match.Pattern{Type: match.TypePrefix}
		}
		cs.public = cs.public || r.public()
	}
	set, err := m.CompileSet(ctx, patterns)
	if err != nil {
		return nil, err
	}
	cs.set = set
	return cs, nil
}

// sortedKeys returns the keys of a map, sorted.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// wildcard matches s against a pattern where "*" is any run of characters
// and "?" one character (both strings already in the same letter case).
func wildcard(pattern, s string) bool {
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == s
	}
	p, t := []rune(pattern), []rune(s)
	pi, ti := 0, 0
	star, mark := -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]):
			pi++
			ti++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ti
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
