package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// service is the policy engine. It keeps no policy state of its own: the
// named policies and attachments live in the store, read through at every
// evaluation, so that changes are effective immediately.
type service struct {
	deps     Deps
	opts     options
	builtins map[string]Named

	// mu serializes the read-modify-write updates of attachments.
	mu sync.Mutex
}

var _ Service = (*service)(nil)

// New returns the policy engine. A nil Store keeps the policies in memory;
// a nil Clock uses the real clock and a nil Logger slog.Default().
func New(deps Deps, opts ...Option) Service {
	if deps.Store == nil {
		deps.Store = NewMemoryStore()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &service{deps: deps, opts: newOptions(opts), builtins: builtInPolicies(deps.Clock.Now().UTC())}
}

// --- evaluation ---------------------------------------------------------------

// Evaluate implements Service.
func (s *service) Evaluate(ctx context.Context, args Args) (Decision, error) {
	return s.evaluate(ctx, args, false)
}

// Simulate implements Service.
func (s *service) Simulate(ctx context.Context, args Args) (Decision, error) {
	return s.evaluate(ctx, args, true)
}

// --- parsing and validation ---------------------------------------------------

// Parse implements Service.
func (s *service) Parse(_ context.Context, raw []byte) (*domain.Policy, error) {
	p, offsets, err := parseDocument(raw, s.opts.maxPolicySize)
	if err != nil {
		return nil, err
	}
	if err := validatePolicy(p, s.opts.maxPolicySize); err != nil {
		locateStatement(err, offsets, raw)
		return nil, err
	}
	return p, nil
}

// Validate implements Service.
func (s *service) Validate(_ context.Context, p *domain.Policy) error {
	return validatePolicy(p, s.opts.maxPolicySize)
}

// ParseBucketPolicy implements Service.
func (s *service) ParseBucketPolicy(_ context.Context, bucket string, raw []byte) (*domain.Policy, error) {
	p, offsets, err := parseDocument(raw, s.opts.maxPolicySize)
	if err != nil {
		return nil, err
	}
	if err := validatePolicy(p, s.opts.maxPolicySize); err != nil {
		locateStatement(err, offsets, raw)
		return nil, err
	}
	if err := validateBucketRules(bucket, p); err != nil {
		locateStatement(err, offsets, raw)
		return nil, err
	}
	return p, nil
}

// ValidateBucketPolicy implements Service.
func (s *service) ValidateBucketPolicy(_ context.Context, bucket string, p *domain.Policy) error {
	if err := validatePolicy(p, s.opts.maxPolicySize); err != nil {
		return err
	}
	return validateBucketRules(bucket, p)
}

// locateStatement adds the position of the offending statement to a
// semantic validation error.
func locateStatement(err error, offsets []int, raw []byte) {
	var ve *ValidationError
	if errors.As(err, &ve) && ve.Statement > 0 {
		locate(err, ve.Statement-1, offsets, raw)
	}
}

// --- named policies -----------------------------------------------------------

// lookup returns a built-in or stored policy.
func (s *service) lookup(ctx context.Context, name string) (Named, error) {
	if b, ok := s.builtins[name]; ok {
		return b, nil
	}
	return s.deps.Store.Get(ctx, name)
}

// SetPolicy implements Service.
func (s *service) SetPolicy(ctx context.Context, name string, p *domain.Policy) error {
	if !validPolicyName(name) {
		return fmt.Errorf("%w: invalid policy name %q", domain.ErrInvalidArgument, name)
	}
	if _, ok := s.builtins[name]; ok {
		return fmt.Errorf("%w: policy %s is built-in and read-only", domain.ErrInvalidArgument, name)
	}
	if err := s.Validate(ctx, p); err != nil {
		return err
	}
	if err := validateIdentityRules(p); err != nil {
		return err
	}
	named := Named{Name: name, Policy: clonePolicy(p), UpdatedAt: s.deps.Clock.Now().UTC()}
	if err := s.deps.Store.Set(ctx, named); err != nil {
		return fmt.Errorf("policy: save %s: %w", name, err)
	}
	return nil
}

// GetPolicy implements Service.
func (s *service) GetPolicy(ctx context.Context, name string) (*domain.Policy, error) {
	named, err := s.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	return clonePolicy(named.Policy), nil
}

// DeletePolicy implements Service: the policy is first detached from every
// entity (so that a failure never leaves it attached to fewer entities
// than reported while still granting) and then deleted.
func (s *service) DeletePolicy(ctx context.Context, name string) error {
	if _, ok := s.builtins[name]; ok {
		return fmt.Errorf("%w: policy %s is built-in and cannot be deleted", domain.ErrInvalidArgument, name)
	}
	if _, err := s.deps.Store.Get(ctx, name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, kind := range []EntityKind{EntityUser, EntityGroup} {
		mapping, err := s.listAttached(ctx, kind)
		if err != nil {
			return err
		}
		for _, entity := range sortedKeys(mapping) {
			if !contains(mapping[entity], name) {
				continue
			}
			if err := s.detachLocked(ctx, Entity{Kind: kind, Name: entity}, []string{name}); err != nil {
				return fmt.Errorf("policy: detach %s from %s %s: %w", name, kind, entity, err)
			}
		}
	}
	if err := s.deps.Store.Delete(ctx, name); err != nil {
		return fmt.Errorf("policy: delete %s: %w", name, err)
	}
	return nil
}

// ListPolicies implements Service.
func (s *service) ListPolicies(ctx context.Context) ([]Named, error) {
	stored, err := s.deps.Store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy: list: %w", err)
	}
	out := s.ListBuiltin(ctx)
	for _, n := range stored {
		if _, shadowed := s.builtins[n.Name]; shadowed {
			continue
		}
		out = append(out, Named{Name: n.Name, Policy: clonePolicy(n.Policy), UpdatedAt: n.UpdatedAt})
	}
	return out, nil
}

// ListBuiltin implements Service.
func (s *service) ListBuiltin(context.Context) []Named {
	out := sortedBuiltins(s.builtins)
	for i := range out {
		out[i].Policy = clonePolicy(out[i].Policy)
	}
	return out
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
	entity := Entity{Kind: EntityUser, Name: in.User}
	if in.Group != "" {
		entity = Entity{Kind: EntityGroup, Name: in.Group}
	}
	if attach {
		for _, n := range in.PolicyNames {
			if _, err := s.lookup(ctx, n); err != nil {
				return err
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !attach {
		return s.detachLocked(ctx, entity, in.PolicyNames)
	}
	if s.deps.Attachments != nil {
		return s.deps.Attachments.Attach(ctx, in)
	}
	list, err := s.attached(ctx, entity)
	if err != nil {
		return err
	}
	if err := s.deps.Store.SetAttached(ctx, entity, normalizeNames(append(list, in.PolicyNames...))); err != nil {
		return fmt.Errorf("policy: attachments of %s %s: %w", entity.Kind, entity.Name, err)
	}
	return nil
}

// detachLocked removes policies from an entity. Called with mu held.
func (s *service) detachLocked(ctx context.Context, e Entity, names []string) error {
	if s.deps.Attachments != nil {
		in := AttachInput{PolicyNames: names, User: e.Name}
		if e.Kind == EntityGroup {
			in = AttachInput{PolicyNames: names, Group: e.Name}
		}
		return s.deps.Attachments.Detach(ctx, in)
	}
	list, err := s.attached(ctx, e)
	if err != nil {
		return err
	}
	for _, n := range names {
		list = removeName(list, n)
	}
	if err := s.deps.Store.SetAttached(ctx, e, list); err != nil {
		return fmt.Errorf("policy: attachments of %s %s: %w", e.Kind, e.Name, err)
	}
	return nil
}

// attached returns the policies attached to an entity, from
// Deps.Attachments when set, from the store otherwise.
func (s *service) attached(ctx context.Context, e Entity) ([]string, error) {
	var (
		names []string
		err   error
	)
	if s.deps.Attachments != nil {
		names, err = s.deps.Attachments.Attached(ctx, e)
	} else {
		names, err = s.deps.Store.Attached(ctx, e)
	}
	if err != nil {
		return nil, fmt.Errorf("policy: attachments of %s %s: %w", e.Kind, e.Name, err)
	}
	return names, nil
}

// listAttached returns every attachment of a kind (see attached).
func (s *service) listAttached(ctx context.Context, kind EntityKind) (map[string][]string, error) {
	var (
		mapping map[string][]string
		err     error
	)
	if s.deps.Attachments != nil {
		mapping, err = s.deps.Attachments.ListAttached(ctx, kind)
	} else {
		mapping, err = s.deps.Store.ListAttached(ctx, kind)
	}
	if err != nil {
		return nil, fmt.Errorf("policy: %s attachments: %w", kind, err)
	}
	return mapping, nil
}

// Entities implements Service.
func (s *service) Entities(ctx context.Context, name string) (PolicyEntities, error) {
	if _, err := s.lookup(ctx, name); err != nil {
		return PolicyEntities{}, err
	}
	out := PolicyEntities{Policy: name, Users: []string{}, Groups: []string{}}
	for _, kind := range []EntityKind{EntityUser, EntityGroup} {
		mapping, err := s.listAttached(ctx, kind)
		if err != nil {
			return PolicyEntities{}, err
		}
		for _, entity := range sortedKeys(mapping) {
			if !contains(mapping[entity], name) {
				continue
			}
			if kind == EntityUser {
				out.Users = append(out.Users, entity)
			} else {
				out.Groups = append(out.Groups, entity)
			}
		}
	}
	return out, nil
}

func contains(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

func removeName(list []string, name string) []string {
	out := make([]string, 0, len(list))
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
