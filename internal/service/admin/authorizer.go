package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// AuthorizerDeps are the dependencies of the Admin API authorizer.
type AuthorizerDeps struct {
	// Policy is the policy engine that decides (required).
	Policy policy.Service
	// Creds resolves the identity of another user for
	// PermissionsInput.User (required: iam.Service implements it).
	Creds  auth.CredentialLookup
	Logger *slog.Logger
}

// AuthorizerOption configures the authorizer.
type AuthorizerOption func(*authorizer)

type authorizer struct {
	deps AuthorizerDeps
}

var _ Authorizer = (*authorizer)(nil)

// NewAuthorizer returns the authorization service of the Admin API. A
// missing policy engine or credential lookup is a wiring error
// (domain.ErrInvalidArgument).
func NewAuthorizer(deps AuthorizerDeps, opts ...AuthorizerOption) (Authorizer, error) {
	if deps.Policy == nil || deps.Creds == nil {
		return nil, fmt.Errorf("%w: admin authorizer requires the policy engine and the credential lookup", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	a := &authorizer{deps: deps}
	for _, o := range opts {
		o(a)
	}
	return a, nil
}

// Authorize implements Authorizer.
func (a *authorizer) Authorize(ctx context.Context, in AuthorizeInput) (Decision, error) {
	if len(in.Actions) == 0 {
		return Decision{}, fmt.Errorf("%w: admin: no action to authorize", domain.ErrInvalidArgument)
	}
	id := in.Requester.Identity
	if id.IsAnonymous || (id.AccessKey == "" && !id.IsRoot) {
		return Decision{DeniedAction: in.Actions[0], Reason: "authentication required"}, nil
	}
	conds := mergeConditions(in.Requester.Conditions, in.Conditions)
	out := Decision{Allowed: true}
	for _, action := range in.Actions {
		d, err := a.deps.Policy.Evaluate(ctx, policy.Args{
			Identity:   &id,
			Action:     action,
			Bucket:     in.Bucket,
			Object:     in.Object,
			Conditions: conds,
		})
		if err != nil {
			return Decision{}, fmt.Errorf("admin: authorize %s: %w", action, err)
		}
		if d.Allowed {
			continue
		}
		if in.SelfService && !explicitDeny(d) {
			out.SelfService = true
			continue
		}
		if in.Partial && !explicitDeny(d) {
			partial, err := a.partiallyAllowed(ctx, id, action, in.Bucket, in.Object, conds)
			if err != nil {
				return Decision{}, err
			}
			if partial {
				continue
			}
		}
		return Decision{DeniedAction: action, Reason: d.Reason}, nil
	}
	if out.SelfService {
		out.Reason = "self-service: the requester manages its own resources only"
	}
	return out, nil
}

// partiallyAllowed reports whether action is granted on the resource by
// statements whose action and resource match and whose conditions only may
// fail (AuthorizeInput.Partial): an Allow of the identity policies or of
// the bucket policy and, for derived credentials, of the inline policy
// too. Conditions are optimistic both ways: a Deny whose conditions do not
// hold for this request does not hide the resource either.
func (a *authorizer) partiallyAllowed(ctx context.Context, id domain.Identity, action policy.Action, bucket, object string, conds map[string][]string) (bool, error) {
	d, err := a.deps.Policy.Simulate(ctx, policy.Args{Identity: &id, Action: action, Bucket: bucket, Object: object, Conditions: conds})
	if err != nil {
		return false, fmt.Errorf("admin: authorize %s: %w", action, err)
	}
	if d.Allowed {
		return true, nil
	}
	if explicitDeny(d) {
		return false, nil
	}
	var parent, inline, hasInline bool
	for _, e := range d.Evaluated {
		if e.Source == policy.SourceInline {
			hasInline = true
		}
		if e.Effect != domain.Allow || (e.Mismatch != policy.MismatchNone && e.Mismatch != policy.MismatchCondition) {
			continue
		}
		if e.Source == policy.SourceInline {
			inline = true
		} else {
			parent = true
		}
	}
	if hasInline && !inline {
		return false, nil
	}
	return parent || id.IsRoot, nil
}

// explicitDeny reports a decision taken by a Deny statement.
func explicitDeny(d policy.Decision) bool {
	return d.MatchedStatement != nil && d.MatchedStatement.Effect == domain.Deny
}

// Permissions implements Authorizer.
func (a *authorizer) Permissions(ctx context.Context, in PermissionsInput) (*Permissions, error) {
	id := in.Requester.Identity
	if id.IsAnonymous || (id.AccessKey == "" && !id.IsRoot) {
		return nil, fmt.Errorf("%w: admin: authentication required", domain.ErrUnauthenticated)
	}
	if in.Prefix != "" && in.Bucket == "" {
		return nil, fmt.Errorf("%w: admin: a prefix needs a bucket", domain.ErrInvalidArgument)
	}
	if in.Bucket != "" {
		if err := domain.ValidateBucketName(in.Bucket); err != nil {
			return nil, err
		}
	}
	if err := domain.ValidateObjectPrefix(in.Prefix); err != nil {
		return nil, err
	}
	user := id.EffectiveUser()
	if in.User != "" && in.User != id.AccessKey {
		other, err := lookupIdentity(ctx, a.deps.Creds, in.User)
		if err != nil {
			return nil, err
		}
		id, user = other, in.User
	}
	out := &Permissions{User: user, IsRoot: id.IsRoot, Bucket: in.Bucket, Prefix: in.Prefix, Allowed: []policy.Action{}, Denied: []policy.Action{}}
	// The listings of the folder (the object browser lists it with "/" as
	// the delimiter) are evaluated with the conditions of that listing, as
	// ObjectBrowser.List authorizes it: a home directory restricted by
	// s3:prefix is listable although the bucket root is not.
	listing := mergeConditions(in.Requester.Conditions, map[string][]string{"s3:prefix": {in.Prefix}, "s3:delimiter": {DefaultDelimiter}})
	for _, e := range catalog() {
		args := policy.Args{Identity: &id, Action: e.action, Conditions: in.Requester.Conditions}
		if in.Bucket != "" {
			switch e.scope {
			case scopeBucket:
				args.Bucket = in.Bucket
				if isListing(e.action) {
					args.Conditions = listing
				}
			case scopeObject:
				args.Bucket, args.Object = in.Bucket, in.Prefix+"*"
			}
		}
		d, err := a.deps.Policy.Evaluate(ctx, args)
		if err != nil {
			return nil, fmt.Errorf("admin: permissions: %w", err)
		}
		if d.Allowed {
			out.Allowed = append(out.Allowed, e.action)
		} else {
			out.Denied = append(out.Denied, e.action)
		}
	}
	sortActions(out.Allowed)
	sortActions(out.Denied)
	return out, nil
}

// isListing reports the bucket actions of a listing, evaluated with the
// s3:prefix and s3:delimiter of the listing.
func isListing(a policy.Action) bool {
	switch a {
	case policy.ActionListBucket, policy.ActionListBucketVersions, policy.ActionListMultipart:
		return true
	}
	return false
}

// lookupIdentity resolves the identity of an access key; unknown keys are
// domain.ErrUserNotFound.
func lookupIdentity(ctx context.Context, creds auth.CredentialLookup, accessKey string) (domain.Identity, error) {
	_, id, err := creds.LookupCredentials(ctx, accessKey)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidAccessKey) {
			return domain.Identity{}, fmt.Errorf("%w: %q", domain.ErrUserNotFound, accessKey)
		}
		return domain.Identity{}, fmt.Errorf("admin: identity of %s: %w", accessKey, err)
	}
	id.AccessKey = accessKey
	return id, nil
}

// mergeConditions returns base overlaid with extra (extra wins), without
// modifying either.
func mergeConditions(base, extra map[string][]string) map[string][]string {
	if len(extra) == 0 {
		return base
	}
	out := make(map[string][]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func sortActions(actions []policy.Action) {
	sort.Slice(actions, func(i, j int) bool { return actions[i] < actions[j] })
}

// denied wraps domain.ErrAccessDenied with the denied action of d.
func denied(d Decision) error {
	reason := strings.TrimSpace(d.Reason)
	if reason == "" {
		reason = "not allowed"
	}
	return fmt.Errorf("%w: %s: %s", domain.ErrAccessDenied, d.DeniedAction, reason)
}

// authorizeAll authorizes actions on a resource and turns a denial into an
// error wrapping domain.ErrAccessDenied.
func authorizeAll(ctx context.Context, authz Authorizer, req Requester, bucket, object string, actions ...policy.Action) error {
	d, err := authz.Authorize(ctx, AuthorizeInput{Requester: req, Actions: actions, Bucket: bucket, Object: object})
	if err != nil {
		return err
	}
	if !d.Allowed {
		return denied(d)
	}
	return nil
}

// systemActor is the identity the facade uses towards the domain services
// once it has authorized an administrator to act on the resources of
// other users (root semantics of iam and share).
func systemActor() domain.Identity { return domain.Identity{IsRoot: true} }

// elevate returns the identity to present to a domain service enforcing
// ownership: the requester itself when it acts on its own resources
// (self-service decision), is root already or holds derived credentials
// (which the domain services restrict), the system actor otherwise.
//
// The system actor has the ownership semantics of root: a facade that
// elevates a delegated administrator must keep it away from the resources
// that carry the powers of root (the access keys of root).
func elevate(req Requester, d Decision) domain.Identity {
	if delegated(req, d) {
		return systemActor()
	}
	return req.Identity
}

// delegated reports whether elevate hands the system actor to a non-root
// administrator acting on the resources of other users.
func delegated(req Requester, d Decision) bool {
	id := req.Identity
	return !d.SelfService && !id.IsRoot && id.Parent == ""
}
