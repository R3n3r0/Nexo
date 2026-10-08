package policy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// allS3Resources matches every bucket and object; it is also the resource
// of requests that do not name a bucket (s3:ListAllMyBuckets, admin
// actions).
const allS3Resources = "arn:aws:s3:::*"

// evalContext holds the condition keys of a request, normalized with
// normalizeKey.
type evalContext struct {
	keys map[string][]string
}

// get returns the values of a condition key.
func (c *evalContext) get(key string) ([]string, bool) {
	v, ok := c.keys[normalizeKey(key)]
	return v, ok
}

// variable returns the value of a single-valued key, for policy variables.
func (c *evalContext) variable(key string) (string, bool) {
	v, ok := c.get(key)
	if !ok || len(v) != 1 {
		return "", false
	}
	return v[0], true
}

// setDefault sets a key unless the request already carries it.
func (c *evalContext) setDefault(key string, values ...string) {
	k := normalizeKey(key)
	if _, ok := c.keys[k]; ok || len(values) == 0 {
		return
	}
	c.keys[k] = values
}

// newEvalContext assembles the condition keys of a request: the values
// supplied by the transport plus the keys derived from the identity and
// the clock.
func newEvalContext(args Args, id *domain.Identity, now time.Time) *evalContext {
	c := &evalContext{keys: make(map[string][]string, len(args.Conditions)+8)}
	for k, v := range args.Conditions {
		c.keys[normalizeKey(k)] = v
	}
	c.setDefault("aws:CurrentTime", now.UTC().Format(time.RFC3339))
	c.setDefault("aws:EpochTime", strconv.FormatInt(now.Unix(), 10))
	c.setDefault("aws:principaltype", principalType(id))
	if !id.IsAnonymous {
		if u := id.EffectiveUser(); u != "" {
			c.setDefault("aws:username", u)
		}
		if id.AccessKey != "" {
			c.setDefault("aws:userid", id.AccessKey)
		}
	}
	if id.AuthType != "" {
		c.setDefault("s3:authType", string(id.AuthType))
	}
	if id.SignatureAge > 0 {
		c.setDefault("s3:signatureAge", strconv.FormatInt(id.SignatureAge.Milliseconds(), 10))
	}
	for claim, v := range id.Claims {
		if values := claimValues(v); len(values) > 0 {
			c.setDefault("jwt:"+claim, values...)
		}
	}
	return c
}

// principalType is the value of aws:principaltype.
func principalType(id *domain.Identity) string {
	switch {
	case id.IsAnonymous:
		return "Anonymous"
	case id.IsRoot && id.Parent == "":
		return "Account"
	case len(id.Claims) > 0:
		return "AssumedRole"
	default:
		return "User"
	}
}

// claimValues renders a JWT claim as condition values.
func claimValues(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case bool:
		return []string{strconv.FormatBool(t)}
	case float64:
		return []string{strconv.FormatFloat(t, 'f', -1, 64)}
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, claimValues(e)...)
		}
		return out
	}
	return nil
}

// resourceARN renders the S3 resource of a request.
func resourceARN(bucket, object string) string {
	switch {
	case bucket == "":
		return allS3Resources
	case object == "":
		return "arn:aws:s3:::" + bucket
	default:
		return "arn:aws:s3:::" + bucket + "/" + object
	}
}

// evalDoc is a policy document taking part in an evaluation.
type evalDoc struct {
	source Source
	name   string
	group  string
	policy *domain.Policy
}

// request is a request prepared for statement matching.
type request struct {
	identity *domain.Identity
	action   string
	// resource is the ARN of the bucket or object, allS3Resources when the
	// request names no bucket (s3:ListAllMyBuckets, most admin actions).
	resource string
	ctx      *evalContext
}

// hit is a statement that matched the request.
type hit struct {
	doc   *evalDoc
	index int
	stmt  *domain.Statement
}

// evaluate is the single implementation of Evaluate and Simulate: trace
// only records the statements, it never changes the decision.
func (s *service) evaluate(ctx context.Context, args Args, trace bool) (Decision, error) {
	id := args.Identity
	if id == nil {
		id = domain.Anonymous()
	}
	if args.Action == "" {
		return Decision{}, fmt.Errorf("%w: policy: empty action", domain.ErrInvalidArgument)
	}
	if args.Object != "" && args.Bucket == "" {
		return Decision{}, fmt.Errorf("%w: policy: object %q without a bucket", domain.ErrInvalidArgument, args.Object)
	}
	if id.IsRoot && id.SessionPolicy == nil {
		return Decision{Allowed: true, Reason: "root credentials", Source: SourceRoot}, nil
	}
	docs, err := s.collectDocs(ctx, args, id)
	if err != nil {
		return Decision{}, err
	}
	now := args.Now
	if now.IsZero() {
		now = s.deps.Clock.Now()
	}
	action := string(args.Action)
	req := &request{
		identity: id,
		action:   action,
		resource: resourceARN(args.Bucket, args.Object),
		ctx:      newEvalContext(args, id, now),
	}

	var (
		deny, parentAllow, inlineAllow, bucketAllow *hit
		hasInline                                   bool
		evaluated                                   []EvaluatedStatement
	)
scan:
	for d := range docs {
		doc := &docs[d]
		if doc.source == SourceInline {
			hasInline = true
		}
		for i := range doc.policy.Statement {
			stmt := &doc.policy.Statement[i]
			mismatch := matchStatement(stmt, req, doc.source == SourceBucket)
			if trace {
				evaluated = append(evaluated, EvaluatedStatement{
					Source: doc.source, PolicyName: doc.name, Group: doc.group, Index: i, Sid: stmt.SID,
					Effect: stmt.Effect, Matched: mismatch == MismatchNone, Mismatch: mismatch, Statement: stmt,
				})
			}
			if mismatch != MismatchNone {
				continue
			}
			h := &hit{doc: doc, index: i, stmt: stmt}
			// Anything but a valid Allow denies: a malformed effect in a
			// document that skipped validation never widens access.
			if stmt.Effect != domain.Allow {
				if deny == nil {
					deny = h
				}
				if !trace {
					break scan
				}
				continue
			}
			switch doc.source {
			case SourceInline:
				inlineAllow = firstHit(inlineAllow, h)
			case SourceBucket:
				bucketAllow = firstHit(bucketAllow, h)
			default:
				parentAllow = firstHit(parentAllow, h)
			}
		}
	}

	d := decide(id, action, deny, parentAllow, inlineAllow, bucketAllow, hasInline)
	d.Evaluated = evaluated
	return d, nil
}

func firstHit(current, h *hit) *hit {
	if current != nil {
		return current
	}
	return h
}

// decide combines the matched statements into the decision.
func decide(id *domain.Identity, action string, deny, parentAllow, inlineAllow, bucketAllow *hit, hasInline bool) Decision {
	if deny != nil {
		return decisionFrom(false, "explicit deny in "+describe(deny), deny)
	}
	switch {
	case id.IsAnonymous:
		if bucketAllow != nil {
			return decisionFrom(true, "allowed by "+describe(bucketAllow), bucketAllow)
		}
		return Decision{Reason: "anonymous access to " + action + " is not granted by the bucket policy"}
	case id.IsRoot:
		// Root with an inline policy: the parent allows everything.
		if inlineAllow != nil {
			return decisionFrom(true, "allowed by "+describe(inlineAllow), inlineAllow)
		}
		return Decision{Reason: "the inline policy does not allow " + action, Source: SourceInline}
	}
	grant := parentAllow
	if grant == nil {
		grant = bucketAllow
	}
	if grant == nil {
		return Decision{Reason: "no policy allows " + action}
	}
	if hasInline && inlineAllow == nil {
		return Decision{Reason: "the inline policy does not allow " + action + " (" + describe(grant) + " does)", Source: SourceInline}
	}
	return decisionFrom(true, "allowed by "+describe(grant), grant)
}

func decisionFrom(allowed bool, reason string, h *hit) Decision {
	return Decision{
		Allowed: allowed, Reason: reason, Source: h.doc.source, PolicyName: h.doc.name, Group: h.doc.group,
		MatchedStatement: h.stmt, Sid: h.stmt.SID, Index: h.index,
	}
}

// describe names the document of a hit for decision reasons.
func describe(h *hit) string {
	var s string
	switch h.doc.source {
	case SourceIAM:
		s = fmt.Sprintf("identity policy %q", h.doc.name)
	case SourceGroup:
		s = fmt.Sprintf("group policy %q (group %s)", h.doc.name, h.doc.group)
	case SourceInline:
		s = "inline policy"
	default:
		s = "bucket policy"
	}
	s += fmt.Sprintf(", statement %d", h.index+1)
	if h.stmt.SID != "" {
		s += fmt.Sprintf(" (Sid %q)", h.stmt.SID)
	}
	return s
}

// collectDocs gathers the documents of an evaluation in order: identity
// policies (direct, then groups), the inline policy, the bucket policy.
func (s *service) collectDocs(ctx context.Context, args Args, id *domain.Identity) ([]evalDoc, error) {
	var docs []evalDoc
	if !id.IsRoot && !id.IsAnonymous {
		identity, err := s.identityDocs(ctx, id)
		if err != nil {
			return nil, err
		}
		docs = identity
	}
	if !id.IsAnonymous && id.SessionPolicy != nil {
		docs = append(docs, evalDoc{source: SourceInline, policy: id.SessionPolicy})
	}
	if !id.IsRoot && args.Bucket != "" {
		bp, err := s.bucketPolicy(ctx, args)
		if err != nil {
			return nil, err
		}
		if bp != nil {
			docs = append(docs, evalDoc{source: SourceBucket, policy: bp})
		}
	}
	return docs, nil
}

// identityDocs resolves the named policies of an identity: the policies
// attached to its user (and access key) and to its groups, then the names
// it carries. Unknown names are skipped.
func (s *service) identityDocs(ctx context.Context, id *domain.Identity) ([]evalDoc, error) {
	var docs []evalDoc
	seen := map[string]bool{}
	add := func(names []string, source Source, group string) error {
		for _, n := range names {
			if seen[n] {
				continue
			}
			seen[n] = true
			named, err := s.lookup(ctx, n)
			if errors.Is(err, domain.ErrPolicyNotFound) {
				s.deps.Logger.DebugContext(ctx, "policy: attached policy not found", "policy", n, "user", id.EffectiveUser())
				continue
			}
			if err != nil {
				return err
			}
			docs = append(docs, evalDoc{source: source, name: n, group: group, policy: named.Policy})
		}
		return nil
	}
	users := []string{id.EffectiveUser()}
	if id.AccessKey != "" && id.AccessKey != id.EffectiveUser() {
		users = append(users, id.AccessKey)
	}
	for _, u := range users {
		if u == "" {
			continue
		}
		names, err := s.attached(ctx, Entity{Kind: EntityUser, Name: u})
		if err != nil {
			return nil, err
		}
		if err := add(names, SourceIAM, ""); err != nil {
			return nil, err
		}
	}
	for _, g := range id.Groups {
		if g == "" {
			continue
		}
		names, err := s.attached(ctx, Entity{Kind: EntityGroup, Name: g})
		if err != nil {
			return nil, err
		}
		if err := add(names, SourceGroup, g); err != nil {
			return nil, err
		}
	}
	// The names resolved by IAM (direct and group policies) that the
	// attachments above did not already cover.
	if err := add(id.PolicyNames, SourceIAM, ""); err != nil {
		return nil, err
	}
	return docs, nil
}

// bucketPolicy returns the bucket policy of the request: Args.BucketPolicy
// or, when nil, the document of the provider parsed with
// ParseBucketPolicy. A missing policy or bucket is no policy; a stored
// document that no longer parses is a server fault and access fails
// closed.
func (s *service) bucketPolicy(ctx context.Context, args Args) (*domain.Policy, error) {
	if args.BucketPolicy != nil || s.deps.Buckets == nil {
		return args.BucketPolicy, nil
	}
	raw, err := s.deps.Buckets.GetBucketPolicy(ctx, args.Bucket)
	switch {
	case err == nil:
	case errors.Is(err, domain.ErrPolicyNotFound), errors.Is(err, domain.ErrBucketNotFound), errors.Is(err, domain.ErrInvalidBucketName):
		return nil, nil
	default:
		return nil, fmt.Errorf("policy: bucket policy of %s: %w", args.Bucket, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	p, err := s.ParseBucketPolicy(ctx, args.Bucket, raw)
	if err != nil {
		return nil, fmt.Errorf("policy: stored bucket policy of %s: %w: %v", args.Bucket, domain.ErrInvalidFormat, err)
	}
	return p, nil
}

// matchStatement reports the first element of the statement that does not
// match the request, or MismatchNone when the statement applies.
// principals are checked for bucket policy statements only.
func matchStatement(stmt *domain.Statement, req *request, bucketPolicy bool) Mismatch {
	if bucketPolicy && !principalApplies(stmt, req.identity) {
		return MismatchPrincipal
	}
	switch {
	case len(stmt.Action) > 0:
		if !anyAction(stmt.Action, req.action) {
			return MismatchAction
		}
	case len(stmt.NotAction) > 0:
		if anyAction(stmt.NotAction, req.action) {
			return MismatchAction
		}
	default:
		return MismatchAction
	}
	if !resourceApplies(stmt, req) {
		return MismatchResource
	}
	if len(stmt.Condition) > 0 {
		holds, known := evalCondition(stmt.Condition, req.ctx)
		if !known {
			// An unknown operator never widens access: the statement
			// applies when it denies and not when it allows.
			holds = stmt.Effect != domain.Allow
		}
		if !holds {
			return MismatchCondition
		}
	}
	return MismatchNone
}

func anyAction(patterns domain.StringSet, action string) bool {
	for _, p := range patterns {
		if matchWildcard(p, action, true) {
			return true
		}
	}
	return false
}

// resourceApplies checks Resource/NotResource. Statements without either
// (admin, sts and kms statements) apply to every resource.
func resourceApplies(stmt *domain.Statement, req *request) bool {
	switch {
	case len(stmt.Resource) > 0:
		for _, r := range stmt.Resource {
			if matched, ok := matchResource(r, req); ok && matched {
				return true
			}
		}
		return false
	case len(stmt.NotResource) > 0:
		for _, r := range stmt.NotResource {
			// An unresolvable pattern excludes, so it never widens.
			if matched, ok := matchResource(r, req); !ok || matched {
				return false
			}
		}
		return true
	default:
		return true
	}
}

// matchResource matches one resource pattern; ok is false when the
// pattern references a variable that cannot be resolved.
func matchResource(pat string, req *request) (matched, ok bool) {
	if pat == "*" {
		return true, true
	}
	if !strings.ContainsAny(pat, "*?$") {
		return pat == req.resource, true
	}
	p, ok := compilePattern(pat, req.ctx)
	if !ok {
		return false, false
	}
	return p.match(req.resource, false), true
}

// principalApplies checks Principal/NotPrincipal of a bucket policy
// statement; a statement without either applies to nobody.
func principalApplies(stmt *domain.Statement, id *domain.Identity) bool {
	switch {
	case stmt.Principal != nil:
		return principalMatches(stmt.Principal, id)
	case stmt.NotPrincipal != nil:
		return !principalMatches(stmt.NotPrincipal, id)
	default:
		return false
	}
}

// principalMatches reports whether the principal element names the caller:
// "*" (everyone, anonymous included), the access key, the effective user
// or an IAM user ARN of either (arn:aws:iam::<any account>:user/<name>).
func principalMatches(p *domain.Principal, id *domain.Identity) bool {
	if p.IsWildcard() {
		return true
	}
	if id.IsAnonymous {
		return false
	}
	names := []string{id.EffectiveUser()}
	if id.AccessKey != "" && id.AccessKey != id.EffectiveUser() {
		names = append(names, id.AccessKey)
	}
	for _, pattern := range p.AWS {
		for _, n := range names {
			if n == "" {
				continue
			}
			if matchWildcard(pattern, n, false) || matchWildcard(pattern, "arn:aws:iam:::user/"+n, false) {
				return true
			}
			if strings.HasPrefix(pattern, "arn:aws:iam::") && strings.HasSuffix(pattern, ":user/"+n) {
				return true
			}
		}
	}
	return false
}
