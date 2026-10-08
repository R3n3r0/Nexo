package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/r3n3r0/nexo/internal/domain"
)

// AnonymousPreset is a simplified anonymous access level of a bucket or of a
// prefix (docs/09 §9.3), implemented with bucket policy statements.
//
// The generation and the detection mirror policy.Service
// (GeneratePublicPolicy, ApplyPublicPreset, DetectPreset,
// ListPublicAccess): nexoctl is a client and does not link the server
// policy engine (depguard cmd-no-layers keeps services out of the
// commands), so the logic is duplicated here and an equivalence test
// (anonymous_equivalence_test.go) keeps the two in step.
type AnonymousPreset string

// Anonymous presets.
const (
	AnonymousPrivate  AnonymousPreset = "private"
	AnonymousDownload AnonymousPreset = "download"
	AnonymousUpload   AnonymousPreset = "upload"
	AnonymousPublic   AnonymousPreset = "public"
	// AnonymousCustom is reported when anonymous statements grant a
	// combination that is not a preset.
	AnonymousCustom AnonymousPreset = "custom"
)

// AnonymousRule is the anonymous access of one prefix of a bucket.
type AnonymousRule struct {
	Prefix string
	Preset AnonymousPreset
}

// Statement identifiers and probes of the presets (same values as the
// server, see policy/public.go).
const (
	anonSidPrefix = "NexoAnonymous"
	anonProbe     = "nexo-preset-probe"
)

// S3 actions used by the presets.
const (
	actGetBucketLocation = "s3:GetBucketLocation"
	actListMultipart     = "s3:ListBucketMultipartUploads"
	actListBucket        = "s3:ListBucket"
	actGetObject         = "s3:GetObject"
	actPutObject         = "s3:PutObject"
	actDeleteObject      = "s3:DeleteObject"
	actAbortMultipart    = "s3:AbortMultipartUpload"
	actListParts         = "s3:ListMultipartUploadParts"
)

// ParseAnonymousPreset parses the preset argument of "anonymous set".
func ParseAnonymousPreset(s string) (AnonymousPreset, error) {
	switch p := AnonymousPreset(strings.ToLower(s)); p {
	case AnonymousPrivate, AnonymousDownload, AnonymousUpload, AnonymousPublic:
		return p, nil
	}
	return "", UsageError("unknown access %q (private, download, upload, public)", s)
}

// ParseBucketPolicy decodes a bucket policy document.
func ParseBucketPolicy(doc string) (*domain.Policy, error) {
	var p domain.Policy
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return nil, fmt.Errorf("%w: bucket policy: %v", domain.ErrMalformedPolicy, err)
	}
	return &p, nil
}

// checkAnonymousArgs validates the bucket and prefix of a preset.
func checkAnonymousArgs(bucket, prefix string) error {
	if bucket == "" || strings.ContainsAny(bucket, "/*?$") {
		return UsageError("invalid bucket name %q", bucket)
	}
	if strings.ContainsAny(prefix, "*?") || strings.Contains(prefix, "${") || strings.HasPrefix(prefix, "/") {
		return UsageError("invalid prefix %q (no wildcards, variables or leading slash)", prefix)
	}
	return nil
}

// anonymousSids returns the Sids of the statements of a prefix: bucket
// actions, listing, object actions.
func anonymousSids(prefix string) [3]string {
	sum := sha256.Sum256([]byte(prefix))
	id := hex.EncodeToString(sum[:6])
	return [3]string{anonSidPrefix + "Bucket" + id, anonSidPrefix + "List" + id, anonSidPrefix + "Objects" + id}
}

// anonymousStatements generates the statements of a preset.
func anonymousStatements(bucket, prefix string, preset AnonymousPreset) ([]domain.Statement, error) {
	var read, write bool
	switch preset {
	case AnonymousPrivate:
		return nil, nil
	case AnonymousDownload:
		read = true
	case AnonymousUpload:
		write = true
	case AnonymousPublic:
		read, write = true, true
	default:
		return nil, UsageError("unknown access %q (private, download, upload, public)", preset)
	}
	sids := anonymousSids(prefix)
	everyone := func() *domain.Principal { return &domain.Principal{AWS: domain.StringSet{"*"}} }
	bucketARN := "arn:aws:s3:::" + bucket
	bucketActions := domain.StringSet{actGetBucketLocation}
	var objectActions domain.StringSet
	if read {
		objectActions = append(objectActions, actGetObject)
	}
	if write {
		bucketActions = append(bucketActions, actListMultipart)
		objectActions = append(objectActions, actPutObject, actAbortMultipart, actListParts)
	}
	if read && write {
		objectActions = append(objectActions, actDeleteObject)
	}
	out := []domain.Statement{{SID: sids[0], Effect: domain.Allow, Principal: everyone(), Action: bucketActions, Resource: domain.StringSet{bucketARN}}}
	if read {
		list := domain.Statement{SID: sids[1], Effect: domain.Allow, Principal: everyone(), Action: domain.StringSet{actListBucket}, Resource: domain.StringSet{bucketARN}}
		if prefix != "" {
			list.Condition = domain.Condition{"StringLike": {"s3:prefix": domain.StringSet{prefix + "*"}}}
		}
		out = append(out, list)
	}
	out = append(out, domain.Statement{SID: sids[2], Effect: domain.Allow, Principal: everyone(), Action: objectActions, Resource: domain.StringSet{bucketARN + "/" + prefix + "*"}})
	return out, nil
}

// ApplyAnonymousPreset returns current (not modified) with the statements
// of the preset for prefix replacing the ones previously generated for the
// same prefix; the other statements are kept. The result is nil when no
// statement is left (the bucket policy should be deleted).
func ApplyAnonymousPreset(current *domain.Policy, bucket, prefix string, preset AnonymousPreset) (*domain.Policy, error) {
	if err := checkAnonymousArgs(bucket, prefix); err != nil {
		return nil, err
	}
	generated, err := anonymousStatements(bucket, prefix, preset)
	if err != nil {
		return nil, err
	}
	out := &domain.Policy{Version: domain.PolicyVersion}
	if current != nil {
		cp, err := clonePolicy(current)
		if err != nil {
			return nil, err
		}
		out.ID = cp.ID
		sids := anonymousSids(prefix)
		for _, st := range cp.Statement {
			if st.SID != sids[0] && st.SID != sids[1] && st.SID != sids[2] {
				out.Statement = append(out.Statement, st)
			}
		}
	}
	out.Statement = append(out.Statement, generated...)
	if len(out.Statement) == 0 {
		return nil, nil
	}
	return out, nil
}

func clonePolicy(p *domain.Policy) (*domain.Policy, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("%w: bucket policy: %v", domain.ErrMalformedPolicy, err)
	}
	var out domain.Policy
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%w: bucket policy: %v", domain.ErrMalformedPolicy, err)
	}
	return &out, nil
}

// DetectAnonymousPreset reports the anonymous access that the bucket
// policy p grants on prefix, probing what an anonymous request may do.
func DetectAnonymousPreset(p *domain.Policy, bucket, prefix string) AnonymousPreset {
	if p == nil || checkAnonymousArgs(bucket, prefix) != nil {
		return AnonymousPrivate
	}
	probe := prefix + anonProbe
	var listKeys map[string][]string
	if prefix != "" {
		listKeys = map[string][]string{"s3:prefix": {prefix}}
	}
	list := anonymousAllowed(p, actListBucket, bucket, "", listKeys)
	get := anonymousAllowed(p, actGetObject, bucket, probe, nil)
	put := anonymousAllowed(p, actPutObject, bucket, probe, nil)
	del := anonymousAllowed(p, actDeleteObject, bucket, probe, nil)
	read := list && get
	switch {
	case read && put:
		return AnonymousPublic
	case read && !put && !del:
		return AnonymousDownload
	case put && !list && !get && !del:
		return AnonymousUpload
	case list || get || put || del || anonymousTouches(p, bucket, prefix):
		return AnonymousCustom
	default:
		return AnonymousPrivate
	}
}

// ListAnonymousAccess returns the prefixes of the bucket with anonymous
// access, sorted, with their preset.
func ListAnonymousAccess(p *domain.Policy, bucket string) []AnonymousRule {
	if p == nil || checkAnonymousArgs(bucket, "") != nil {
		return nil
	}
	objects := "arn:aws:s3:::" + bucket + "/"
	candidates := map[string]bool{}
	for i := range p.Statement {
		st := &p.Statement[i]
		if st.Effect != domain.Allow || !anonymousPrincipal(st) {
			continue
		}
		for _, r := range st.Resource {
			if prefix, ok := strings.CutPrefix(literalPrefix(r), objects); ok {
				candidates[prefix] = true
			}
		}
	}
	prefixes := SortedKeys(candidates)
	out := make([]AnonymousRule, 0, len(prefixes))
	for _, prefix := range prefixes {
		if checkAnonymousArgs(bucket, prefix) != nil {
			continue
		}
		if preset := DetectAnonymousPreset(p, bucket, prefix); preset != AnonymousPrivate {
			out = append(out, AnonymousRule{Prefix: prefix, Preset: preset})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Prefix < out[j].Prefix })
	return out
}

// anonymousTouches reports whether an anonymous Allow statement targets
// objects that may lie under the prefix with a resource at least as broad
// as the prefix.
func anonymousTouches(p *domain.Policy, bucket, prefix string) bool {
	target := "arn:aws:s3:::" + bucket + "/" + prefix
	for i := range p.Statement {
		st := &p.Statement[i]
		if st.Effect != domain.Allow || !anonymousPrincipal(st) {
			continue
		}
		if len(st.NotResource) > 0 {
			return true
		}
		for _, r := range st.Resource {
			if r == "*" {
				return true
			}
			literal := literalPrefix(r)
			if literal == r && !strings.Contains(strings.TrimPrefix(r, "arn:aws:s3:::"), "/") {
				continue // the bucket itself
			}
			if strings.HasPrefix(target, literal) {
				return true
			}
		}
	}
	return false
}

// literalPrefix returns the part of a resource pattern before its first
// wildcard or variable.
func literalPrefix(r string) string {
	if i := strings.IndexAny(r, "*?"); i >= 0 {
		r = r[:i]
	}
	if i := strings.Index(r, "${"); i >= 0 {
		r = r[:i]
	}
	return r
}

// --- reduced evaluation of anonymous requests --------------------------------

// anonymousAllowed evaluates an anonymous request against the bucket
// policy: an applicable Deny wins, otherwise an applicable Allow grants.
// Only the condition keys an anonymous probe carries are known (keys plus
// aws:principaltype); policy variables and time keys are not resolved
// offline: such statements never widen access (an Allow does not apply, a
// Deny does), like the unknown operators of the server.
func anonymousAllowed(p *domain.Policy, action, bucket, object string, keys map[string][]string) bool {
	resource := "arn:aws:s3:::" + bucket
	if object != "" {
		resource += "/" + object
	}
	ctx := map[string][]string{"aws:principaltype": {"Anonymous"}}
	for k, v := range keys {
		ctx[conditionKey(k)] = v
	}
	allowed := false
	for i := range p.Statement {
		st := &p.Statement[i]
		if !statementApplies(st, action, resource, ctx) {
			continue
		}
		if st.Effect != domain.Allow {
			return false
		}
		allowed = true
	}
	return allowed
}

func statementApplies(st *domain.Statement, action, resource string, ctx map[string][]string) bool {
	if !anonymousPrincipal(st) {
		return false
	}
	switch {
	case len(st.Action) > 0:
		if !anyWildcard(st.Action, action, true) {
			return false
		}
	case len(st.NotAction) > 0:
		if anyWildcard(st.NotAction, action, true) {
			return false
		}
	default:
		return false
	}
	if !resourceMatches(st, resource) {
		return false
	}
	if len(st.Condition) == 0 {
		return true
	}
	holds, known := conditionHolds(st.Condition, ctx)
	if !known {
		return st.Effect != domain.Allow
	}
	return holds
}

// anonymousPrincipal reports whether a bucket policy statement applies to
// anonymous requests.
func anonymousPrincipal(st *domain.Statement) bool {
	switch {
	case st.Principal != nil:
		return st.Principal.IsWildcard()
	case st.NotPrincipal != nil:
		return !st.NotPrincipal.IsWildcard()
	default:
		return false
	}
}

func resourceMatches(st *domain.Statement, resource string) bool {
	switch {
	case len(st.Resource) > 0:
		for _, r := range st.Resource {
			if m, ok := matchResourcePattern(r, resource); ok && m {
				return true
			}
		}
		return false
	case len(st.NotResource) > 0:
		for _, r := range st.NotResource {
			if m, ok := matchResourcePattern(r, resource); !ok || m {
				return false
			}
		}
		return true
	default:
		return true
	}
}

// matchResourcePattern matches a resource pattern; ok is false when it
// holds a policy variable that is not an escape.
func matchResourcePattern(pat, resource string) (matched, ok bool) {
	if pat == "*" {
		return true, true
	}
	pat, ok = expandEscapes(pat)
	if !ok {
		return false, false
	}
	return wildcardMatch([]rune(pat), []rune(resource), false), true
}

// expandEscapes resolves the ${*}, ${?} and ${$} escapes of a pattern into
// private-use runes standing for literal characters; any other variable
// makes ok false.
func expandEscapes(s string) (string, bool) {
	if !strings.Contains(s, "${") {
		return s, true
	}
	var sb strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			sb.WriteString(s)
			return sb.String(), true
		}
		j := strings.IndexByte(s[i+2:], '}')
		if j < 0 {
			sb.WriteString(s)
			return sb.String(), true
		}
		sb.WriteString(s[:i])
		switch s[i+2 : i+2+j] {
		case "*":
			sb.WriteRune(litStar)
		case "?":
			sb.WriteRune(litQuestion)
		case "$":
			sb.WriteRune('$')
		default:
			return "", false
		}
		s = s[i+3+j:]
	}
}

// Private-use runes standing for the literal "*" and "?" of escapes.
const (
	litStar     = ''
	litQuestion = ''
)

func anyWildcard(patterns domain.StringSet, s string, fold bool) bool {
	for _, p := range patterns {
		if p == "*" || wildcardMatch([]rune(p), []rune(s), fold) {
			return true
		}
	}
	return false
}

// wildcardMatch matches s against a pattern with "*" and "?" wildcards.
func wildcardMatch(p, s []rune, fold bool) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(p) && (p[pi] == '?' || runeEq(p[pi], s[si], fold)):
			pi++
			si++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

func runeEq(p, s rune, fold bool) bool {
	switch p {
	case litStar:
		p = '*'
	case litQuestion:
		p = '?'
	}
	return p == s || (fold && unicode.ToLower(p) == unicode.ToLower(s))
}

// conditionKey normalizes a condition key like the server: the part before
// the first "/" is case-insensitive.
func conditionKey(k string) string {
	if i := strings.IndexByte(k, '/'); i >= 0 {
		return strings.ToLower(k[:i]) + k[i:]
	}
	return strings.ToLower(k)
}

// condOperator is a parsed condition operator.
type condOperator struct {
	base                            string
	negated, ifExists, forAll, forAny bool
}

// parseCondOperator parses an operator; ok is false for unknown operators.
func parseCondOperator(op string) (condOperator, bool) {
	var c condOperator
	switch {
	case strings.HasPrefix(op, "ForAllValues:"):
		c.forAll = true
		op = strings.TrimPrefix(op, "ForAllValues:")
	case strings.HasPrefix(op, "ForAnyValue:"):
		c.forAny = true
		op = strings.TrimPrefix(op, "ForAnyValue:")
	}
	if base, found := strings.CutSuffix(op, "IfExists"); found {
		c.ifExists = true
		op = base
	}
	switch op {
	case "StringEquals", "StringEqualsIgnoreCase", "StringLike", "NumericEquals", "NumericLessThan",
		"NumericLessThanEquals", "NumericGreaterThan", "NumericGreaterThanEquals", "DateEquals",
		"DateLessThan", "DateLessThanEquals", "DateGreaterThan", "DateGreaterThanEquals", "Bool",
		"IpAddress", "ArnEquals", "ArnLike":
	case "StringNotEquals", "StringNotEqualsIgnoreCase", "StringNotLike", "NumericNotEquals",
		"DateNotEquals", "NotIpAddress", "ArnNotEquals", "ArnNotLike":
		c.negated = true
	case "Null":
		if c.ifExists || c.forAll || c.forAny {
			return c, false
		}
	default:
		return c, false
	}
	c.base = op
	return c, true
}

// conditionHolds evaluates a condition block on the known keys; known is
// false when the block uses an operator, a key or a value that cannot be
// evaluated offline.
func conditionHolds(cond domain.Condition, ctx map[string][]string) (holds, known bool) {
	ops := make([]condOperator, 0, len(cond))
	keysOf := make([]map[string]domain.StringSet, 0, len(cond))
	for op, keys := range cond {
		parsed, ok := parseCondOperator(op)
		if !ok {
			return false, false
		}
		ops = append(ops, parsed)
		keysOf = append(keysOf, keys)
	}
	holds = true
	for i, op := range ops {
		for key, values := range keysOf[i] {
			h, ok := evalConditionKey(op, ctx, key, values)
			if !ok {
				return false, false
			}
			if !h {
				holds = false
			}
		}
	}
	return holds, true
}

// evalConditionKey evaluates one operator on one key.
func evalConditionKey(op condOperator, ctx map[string][]string, key string, values domain.StringSet) (holds, known bool) {
	k := conditionKey(key)
	if k == "aws:currenttime" || k == "aws:epochtime" {
		return false, false // depends on the clock of the server
	}
	actual, present := ctx[k]
	absent := !present || len(actual) == 0
	if op.base == "Null" {
		for _, v := range values {
			if strings.EqualFold(v, "true") == absent {
				return true, true
			}
		}
		return false, true
	}
	if absent {
		switch {
		case op.ifExists, op.forAll:
			return true, true
		case op.forAny:
			return false, true
		default:
			return op.negated, true
		}
	}
	for _, v := range values {
		if strings.Contains(v, "${") {
			return false, false
		}
	}
	if !strings.HasPrefix(op.base, "String") {
		return false, false
	}
	one := func(a string) bool {
		matched := false
		for _, v := range values {
			if stringOp(op.base, a, v) {
				matched = true
				break
			}
		}
		if op.negated {
			return !matched
		}
		return matched
	}
	switch {
	case op.forAny:
		for _, a := range actual {
			if one(a) {
				return true, true
			}
		}
		return false, true
	case op.forAll, op.negated:
		for _, a := range actual {
			if !one(a) {
				return false, true
			}
		}
		return true, true
	default:
		for _, a := range actual {
			if one(a) {
				return true, true
			}
		}
		return false, true
	}
}

// stringOp compares a request value with a policy value; for the negated
// operators it reports the positive comparison.
func stringOp(base, actual, policy string) bool {
	switch base {
	case "StringEquals", "StringNotEquals":
		return actual == policy
	case "StringEqualsIgnoreCase", "StringNotEqualsIgnoreCase":
		return strings.EqualFold(actual, policy)
	default: // StringLike, StringNotLike
		return wildcardMatch([]rune(policy), []rune(actual), false)
	}
}
