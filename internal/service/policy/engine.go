package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// Option configures the policy engine.
type Option func(*options)

type options struct {
	// maxPolicySize bounds JSON documents (default 20 KiB, as AWS).
	maxPolicySize int
}

// WithMaxPolicySize bounds the size of policy documents.
func WithMaxPolicySize(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxPolicySize = n
		}
	}
}

// policyNameRE is the accepted shape of a policy name.
var policyNameRE = regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]{1,128}$`)

// service is the policy engine: it evaluates documents and keeps the named
// policies and attachments in memory, writing through the store.
type service struct {
	deps Deps
	opts options

	mu       sync.RWMutex
	loaded   bool
	snap     *Snapshot
	builtins map[string]Named
}

var _ Service = (*service)(nil)

// New returns the policy engine. A nil Store keeps the policies in memory.
func New(deps Deps, opts ...Option) Service {
	if deps.Store == nil {
		deps.Store = NewMemoryStore(nil)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := options{maxPolicySize: 20 << 10}
	for _, opt := range opts {
		opt(&o)
	}
	return &service{deps: deps, opts: o, builtins: builtInPolicies(deps.Clock.Now().UTC())}
}

func (s *service) ensureLoaded(ctx context.Context) error {
	s.mu.RLock()
	loaded := s.loaded
	s.mu.RUnlock()
	if loaded {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	snap, err := s.deps.Store.Load(ctx)
	if err != nil {
		return fmt.Errorf("policy: load: %w", err)
	}
	s.snap = cloneSnapshot(snap)
	s.loaded = true
	return nil
}

// --- evaluation ----------------------------------------------------------------

// effect is the outcome of evaluating a set of documents.
type effect int

const (
	effectNone effect = iota
	effectAllow
	effectDeny
)

// Evaluate implements Service.
func (s *service) Evaluate(ctx context.Context, args Args) (Decision, error) {
	identity := args.Identity
	if identity == nil {
		identity = domain.Anonymous()
	}
	if identity.IsRoot {
		return Decision{Allowed: true, Reason: "root credentials"}, nil
	}
	if args.Action == "" {
		return Decision{}, fmt.Errorf("%w: policy: empty action", domain.ErrInvalidArgument)
	}
	if err := s.ensureLoaded(ctx); err != nil {
		return Decision{}, err
	}
	now := args.Now
	if now.IsZero() {
		now = s.deps.Clock.Now()
	}
	condCtx := conditionContext(args, identity, now)
	resource := resourceARN(args.Bucket, args.Object)

	var (
		iamEffect  = effectNone
		iamStmt    *domain.Statement
		denyReason string
	)
	if !identity.IsAnonymous {
		docs := s.identityPolicies(ctx, identity)
		iamEffect, iamStmt = evaluateDocs(docs, args.Action, resource, condCtx, nil)
		if iamEffect == effectDeny {
			denyReason = "explicit deny in identity policy"
		}
		if identity.SessionPolicy != nil && iamEffect != effectDeny {
			sessEffect, sessStmt := evaluateDocs([]*domain.Policy{identity.SessionPolicy}, args.Action, resource, condCtx, nil)
			switch {
			case sessEffect == effectDeny:
				iamEffect, iamStmt, denyReason = effectDeny, sessStmt, "explicit deny in session policy"
			case sessEffect != effectAllow && iamEffect == effectAllow:
				// The session policy narrows the parent's permissions.
				iamEffect, iamStmt = effectNone, nil
			}
		}
	}
	bpEffect, bpStmt := effectNone, (*domain.Statement)(nil)
	if args.BucketPolicy != nil {
		bpEffect, bpStmt = evaluateDocs([]*domain.Policy{args.BucketPolicy}, args.Action, resource, condCtx, identity)
		if bpEffect == effectDeny {
			denyReason = "explicit deny in bucket policy"
		}
	}
	switch {
	case iamEffect == effectDeny:
		return Decision{Reason: denyReason, MatchedStatement: iamStmt}, nil
	case bpEffect == effectDeny:
		return Decision{Reason: denyReason, MatchedStatement: bpStmt}, nil
	case iamEffect == effectAllow:
		return Decision{Allowed: true, Reason: "allowed by identity policy", MatchedStatement: iamStmt}, nil
	case bpEffect == effectAllow:
		return Decision{Allowed: true, Reason: "allowed by bucket policy", MatchedStatement: bpStmt}, nil
	case identity.IsAnonymous:
		return Decision{Reason: "anonymous access is not granted by the bucket policy"}, nil
	default:
		return Decision{Reason: "no policy allows " + string(args.Action)}, nil
	}
}

// identityPolicies resolves the documents attached to an identity: the
// names it carries plus the stored attachments of its user and groups.
func (s *service) identityPolicies(ctx context.Context, id *domain.Identity) []*domain.Policy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := map[string]struct{}{}
	add := func(list []string) {
		for _, n := range list {
			names[n] = struct{}{}
		}
	}
	add(id.PolicyNames)
	add(s.snap.Users[id.EffectiveUser()])
	if id.AccessKey != "" && id.AccessKey != id.EffectiveUser() {
		add(s.snap.Users[id.AccessKey])
	}
	for _, g := range id.Groups {
		add(s.snap.Groups[g])
	}
	docs := make([]*domain.Policy, 0, len(names))
	for n := range names {
		if named, ok := s.lookupLocked(n); ok {
			docs = append(docs, named.Policy)
			continue
		}
		s.deps.Logger.DebugContext(ctx, "policy: attached policy not found", "policy", n, "user", id.EffectiveUser())
	}
	return docs
}

func (s *service) lookupLocked(name string) (Named, bool) {
	if b, ok := s.builtins[name]; ok {
		return b, true
	}
	n, ok := s.snap.Policies[name]
	return n, ok
}

// conditionContext assembles the condition keys of a request.
func conditionContext(args Args, id *domain.Identity, now time.Time) map[string][]string {
	ctx := make(map[string][]string, len(args.Conditions)+6)
	for k, v := range args.Conditions {
		ctx[k] = v
	}
	set := func(k, v string) {
		if _, ok := lookupKey(ctx, k); !ok && v != "" {
			ctx[k] = []string{v}
		}
	}
	set("aws:CurrentTime", now.UTC().Format(time.RFC3339))
	set("aws:EpochTime", fmt.Sprint(now.Unix()))
	if id.IsAnonymous {
		set("aws:principaltype", "Anonymous")
	} else {
		set("aws:username", id.EffectiveUser())
		set("aws:userid", id.AccessKey)
		set("aws:principaltype", "User")
	}
	if args.Bucket != "" {
		set("s3:bucket", args.Bucket)
	}
	return ctx
}

// resourceARN renders the S3 resource of a request.
func resourceARN(bucket, object string) string {
	if bucket == "" {
		return allS3Resources
	}
	if object == "" {
		return "arn:aws:s3:::" + bucket
	}
	return "arn:aws:s3:::" + bucket + "/" + object
}

// evaluateDocs returns the combined effect of the documents: a matching Deny
// wins, otherwise a matching Allow, otherwise none. principal is non-nil for
// bucket policies, whose statements must name the caller.
func evaluateDocs(docs []*domain.Policy, action Action, resource string, condCtx map[string][]string, principal *domain.Identity) (effect, *domain.Statement) {
	var allowStmt *domain.Statement
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		for i := range doc.Statement {
			stmt := &doc.Statement[i]
			if !statementMatches(stmt, action, resource, condCtx, principal) {
				continue
			}
			if stmt.Effect == domain.Deny {
				return effectDeny, stmt
			}
			if allowStmt == nil {
				allowStmt = stmt
			}
		}
	}
	if allowStmt != nil {
		return effectAllow, allowStmt
	}
	return effectNone, nil
}

// statementMatches checks principal, action, resource and conditions.
func statementMatches(stmt *domain.Statement, action Action, resource string, condCtx map[string][]string, principal *domain.Identity) bool {
	if principal != nil {
		switch {
		case stmt.Principal != nil:
			if !principalMatches(stmt.Principal, principal) {
				return false
			}
		case stmt.NotPrincipal != nil:
			if principalMatches(stmt.NotPrincipal, principal) {
				return false
			}
		default:
			return false // bucket policy statements must name a principal
		}
	}
	switch {
	case len(stmt.Action) > 0:
		if !anyMatchFold(stmt.Action, string(action), condCtx) {
			return false
		}
	case len(stmt.NotAction) > 0:
		if anyMatchFold(stmt.NotAction, string(action), condCtx) {
			return false
		}
	default:
		return false
	}
	if !isAdminAction(action) {
		switch {
		case len(stmt.Resource) > 0:
			if !anyMatch(stmt.Resource, resource, condCtx) {
				return false
			}
		case len(stmt.NotResource) > 0:
			if anyMatch(stmt.NotResource, resource, condCtx) {
				return false
			}
		}
	}
	if len(stmt.Condition) > 0 && !evalCondition(stmt.Condition, condCtx) {
		return false
	}
	return true
}

func isAdminAction(a Action) bool {
	return strings.HasPrefix(strings.ToLower(string(a)), "admin:")
}

func anyMatch(patterns domain.StringSet, value string, condCtx map[string][]string) bool {
	for _, p := range patterns {
		if matchWildcard(substitute(p, condCtx), value) {
			return true
		}
	}
	return false
}

func anyMatchFold(patterns domain.StringSet, value string, condCtx map[string][]string) bool {
	for _, p := range patterns {
		if matchWildcardFold(substitute(p, condCtx), value) {
			return true
		}
	}
	return false
}

// principalMatches reports whether the principal element names the caller:
// "*" (everyone, anonymous included), the access key, the effective user or
// an IAM user ARN whose name part matches.
func principalMatches(p *domain.Principal, id *domain.Identity) bool {
	if p.IsWildcard() {
		return true
	}
	if id.IsAnonymous {
		return false
	}
	candidates := []string{id.AccessKey, id.EffectiveUser(), "arn:aws:iam:::user/" + id.EffectiveUser()}
	for _, pattern := range p.AWS {
		for _, c := range candidates {
			if c != "" && (matchWildcard(pattern, c) || strings.HasSuffix(pattern, ":user/"+c)) {
				return true
			}
		}
	}
	return false
}

// --- parsing and validation ----------------------------------------------------

// malformedPolicy builds the S3 MalformedPolicy error.
func malformedPolicy(detail string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "MalformedPolicy", detail)
}

// Parse implements Service.
func (s *service) Parse(ctx context.Context, raw []byte) (*domain.Policy, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, malformedPolicy("Policy document is empty")
	}
	if len(raw) > s.opts.maxPolicySize {
		return nil, malformedPolicy(fmt.Sprintf("Policy document exceeds %d bytes", s.opts.maxPolicySize))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p domain.Policy
	if err := dec.Decode(&p); err != nil {
		return nil, malformedPolicy("Policy document is not valid JSON: " + err.Error())
	}
	if err := s.Validate(ctx, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// knownActionPrefixes are the services actions may belong to.
var knownActionPrefixes = map[string]bool{"s3": true, "admin": true, "sts": true, "kms": true}

// Validate implements Service.
func (s *service) Validate(_ context.Context, p *domain.Policy) error {
	if p == nil {
		return malformedPolicy("Policy document is empty")
	}
	if p.Version != domain.PolicyVersion {
		return malformedPolicy("Policy Version must be " + domain.PolicyVersion)
	}
	if len(p.Statement) == 0 {
		return malformedPolicy("Policy must contain at least one Statement")
	}
	sids := map[string]bool{}
	for i, stmt := range p.Statement {
		where := fmt.Sprintf("Statement %d", i+1)
		if stmt.SID != "" {
			if sids[stmt.SID] {
				return malformedPolicy(where + ": duplicate Sid " + stmt.SID)
			}
			sids[stmt.SID] = true
		}
		if !stmt.Effect.IsValid() {
			return malformedPolicy(where + ": Effect must be Allow or Deny")
		}
		if (len(stmt.Action) == 0) == (len(stmt.NotAction) == 0) {
			return malformedPolicy(where + ": exactly one of Action and NotAction is required")
		}
		if stmt.Principal != nil && stmt.NotPrincipal != nil {
			return malformedPolicy(where + ": Principal and NotPrincipal are mutually exclusive")
		}
		needsResource := false
		for _, a := range append(append(domain.StringSet{}, stmt.Action...), stmt.NotAction...) {
			prefix, name, ok := strings.Cut(a, ":")
			if !ok || name == "" || !knownActionPrefixes[strings.ToLower(prefix)] {
				return malformedPolicy(where + ": invalid action " + a)
			}
			if strings.EqualFold(prefix, "s3") || a == "*" {
				needsResource = true
			}
		}
		if len(stmt.Resource) > 0 && len(stmt.NotResource) > 0 {
			return malformedPolicy(where + ": Resource and NotResource are mutually exclusive")
		}
		if needsResource && len(stmt.Resource) == 0 && len(stmt.NotResource) == 0 {
			return malformedPolicy(where + ": Resource is required for S3 actions")
		}
		for _, r := range append(append(domain.StringSet{}, stmt.Resource...), stmt.NotResource...) {
			if r != "*" && !strings.HasPrefix(r, "arn:aws:s3:::") {
				return malformedPolicy(where + ": invalid resource " + r + " (expected arn:aws:s3:::...)")
			}
		}
		for op, keys := range stmt.Condition {
			if _, ok := parseOperator(op); !ok {
				return malformedPolicy(where + ": unknown condition operator " + op)
			}
			if len(keys) == 0 {
				return malformedPolicy(where + ": condition " + op + " has no keys")
			}
		}
	}
	return nil
}

// --- named policies ----------------------------------------------------------------

// SetPolicy implements Service.
func (s *service) SetPolicy(ctx context.Context, name string, p *domain.Policy) error {
	if !policyNameRE.MatchString(name) {
		return fmt.Errorf("%w: invalid policy name %q", domain.ErrInvalidArgument, name)
	}
	if err := s.Validate(ctx, p); err != nil {
		return err
	}
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.builtins[name]; ok {
		return fmt.Errorf("%w: policy %s is built-in and read-only", domain.ErrInvalidArgument, name)
	}
	next := cloneSnapshot(s.snap)
	next.Policies[name] = Named{Name: name, Policy: clonePolicy(p), UpdatedAt: s.deps.Clock.Now().UTC()}
	return s.saveLocked(ctx, next)
}

// GetPolicy implements Service.
func (s *service) GetPolicy(ctx context.Context, name string) (*domain.Policy, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	named, ok := s.lookupLocked(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrPolicyNotFound, name)
	}
	return clonePolicy(named.Policy), nil
}

// DeletePolicy implements Service.
func (s *service) DeletePolicy(ctx context.Context, name string) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.builtins[name]; ok {
		return fmt.Errorf("%w: policy %s is built-in and cannot be deleted", domain.ErrInvalidArgument, name)
	}
	if _, ok := s.snap.Policies[name]; !ok {
		return fmt.Errorf("%w: %s", domain.ErrPolicyNotFound, name)
	}
	next := cloneSnapshot(s.snap)
	delete(next.Policies, name)
	for _, attachments := range []map[string][]string{next.Users, next.Groups} {
		for principal, list := range attachments {
			if list = removeName(list, name); len(list) == 0 {
				delete(attachments, principal)
			} else {
				attachments[principal] = list
			}
		}
	}
	return s.saveLocked(ctx, next)
}

// ListPolicies implements Service: built-ins first, then user policies, by name.
func (s *service) ListPolicies(ctx context.Context) ([]Named, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Named, 0, len(s.builtins)+len(s.snap.Policies))
	for _, b := range s.builtins {
		out = append(out, Named{Name: b.Name, Policy: clonePolicy(b.Policy), UpdatedAt: b.UpdatedAt, BuiltIn: true})
	}
	for _, n := range s.snap.Policies {
		out = append(out, Named{Name: n.Name, Policy: clonePolicy(n.Policy), UpdatedAt: n.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BuiltIn != out[j].BuiltIn {
			return out[i].BuiltIn
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Attach implements Service.
func (s *service) Attach(ctx context.Context, in AttachInput) error {
	return s.updateAttachments(ctx, in, true)
}

// Detach implements Service.
func (s *service) Detach(ctx context.Context, in AttachInput) error {
	return s.updateAttachments(ctx, in, false)
}

func (s *service) updateAttachments(ctx context.Context, in AttachInput, attach bool) error {
	if (in.User == "") == (in.Group == "") {
		return fmt.Errorf("%w: exactly one of user and group is required", domain.ErrInvalidArgument)
	}
	if len(in.PolicyNames) == 0 {
		return fmt.Errorf("%w: at least one policy name is required", domain.ErrInvalidArgument)
	}
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if attach {
		for _, n := range in.PolicyNames {
			if _, ok := s.lookupLocked(n); !ok {
				return fmt.Errorf("%w: %s", domain.ErrPolicyNotFound, n)
			}
		}
	}
	next := cloneSnapshot(s.snap)
	target, key := next.Users, in.User
	if in.Group != "" {
		target, key = next.Groups, in.Group
	}
	list := target[key]
	for _, n := range in.PolicyNames {
		if attach {
			list = addName(list, n)
		} else {
			list = removeName(list, n)
		}
	}
	if len(list) == 0 {
		delete(target, key)
	} else {
		target[key] = list
	}
	return s.saveLocked(ctx, next)
}

// saveLocked persists next and makes it current. Called with mu held.
func (s *service) saveLocked(ctx context.Context, next *Snapshot) error {
	if err := s.deps.Store.Save(ctx, next); err != nil {
		return fmt.Errorf("policy: save: %w", err)
	}
	s.snap = next
	return nil
}

func addName(list []string, name string) []string {
	for _, n := range list {
		if n == name {
			return list
		}
	}
	list = append(list, name)
	sort.Strings(list)
	return list
}

func removeName(list []string, name string) []string {
	out := list[:0:0]
	for _, n := range list {
		if n != name {
			out = append(out, n)
		}
	}
	return out
}

// clonePolicy deep-copies a document through its JSON form.
func clonePolicy(p *domain.Policy) *domain.Policy {
	if p == nil {
		return nil
	}
	data, err := json.Marshal(p)
	if err != nil {
		return p
	}
	var out domain.Policy
	if err := json.Unmarshal(data, &out); err != nil {
		return p
	}
	return &out
}
