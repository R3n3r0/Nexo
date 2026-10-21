package iam

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// Persisted records, one JSON document per store entry.

type userRecord struct {
	AccessKey   string                  `json:"accessKey"`
	SecretKey   string                  `json:"secretKey"`
	Status      domain.CredentialStatus `json:"status"`
	PolicyNames []string                `json:"policyNames,omitempty"`
	CreatedAt   time.Time               `json:"createdAt"`
	UpdatedAt   time.Time               `json:"updatedAt"`
	// RevocationEpoch counts the times the user was disabled
	// (domain.Credentials.RevocationEpoch).
	RevocationEpoch uint64 `json:"revocationEpoch,omitempty"`
}

type groupRecord struct {
	Name        string                  `json:"name"`
	Status      domain.CredentialStatus `json:"status"`
	Members     []string                `json:"members,omitempty"`
	PolicyNames []string                `json:"policyNames,omitempty"`
	CreatedAt   time.Time               `json:"createdAt"`
	UpdatedAt   time.Time               `json:"updatedAt"`
}

type serviceAccountRecord struct {
	AccessKey   string                  `json:"accessKey"`
	SecretKey   string                  `json:"secretKey"`
	ParentUser  string                  `json:"parentUser"`
	Name        string                  `json:"name,omitempty"`
	Description string                  `json:"description,omitempty"`
	Status      domain.CredentialStatus `json:"status"`
	Expiration  time.Time               `json:"expiration"`
	Policy      *domain.Policy          `json:"policy,omitempty"`
	CreatedAt   time.Time               `json:"createdAt"`
	UpdatedAt   time.Time               `json:"updatedAt"`
	// RevocationEpoch counts the times the key was disabled
	// (domain.Credentials.RevocationEpoch).
	RevocationEpoch uint64 `json:"revocationEpoch,omitempty"`
}

type stsRecord struct {
	AccessKey    string    `json:"accessKey"`
	SecretKey    string    `json:"secretKey"`
	SessionToken string    `json:"sessionToken"`
	ParentUser   string    `json:"parentUser"`
	Expiration   time.Time `json:"expiration"`
	CreatedAt    time.Time `json:"createdAt"`
	// Federated is set for the credentials of an identity of an external
	// provider (federated.go).
	Federated *federatedRecord `json:"federated,omitempty"`
}

// state is the cached IAM state. Records are never modified in place:
// writers store a modified copy, so that values handed out under the read
// lock stay consistent.
type state struct {
	users  map[string]*userRecord
	groups map[string]*groupRecord
	sas    map[string]*serviceAccountRecord
	sts    map[string]*stsRecord
}

func newState() *state {
	return &state{
		users:  make(map[string]*userRecord),
		groups: make(map[string]*groupRecord),
		sas:    make(map[string]*serviceAccountRecord),
		sts:    make(map[string]*stsRecord),
	}
}

// service is the IAM implementation: a write-through cache over a Store.
type service struct {
	deps Deps
	opts options

	mu sync.RWMutex
	// st is nil until loaded and after Invalidate.
	st *state
	// root and stsKey are set by Bootstrap.
	root   *domain.Credentials
	stsKey []byte
}

var (
	_ Service            = (*service)(nil)
	_ policy.Attachments = (*service)(nil)
)

// New returns the IAM service. A nil Store keeps the state in memory.
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
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return &service{deps: deps, opts: o}
}

func (s *service) now() time.Time { return s.deps.Clock.Now().UTC() }

// Bootstrap implements Service.
func (s *service) Bootstrap(ctx context.Context, root domain.Credentials) error {
	if root.AccessKey == "" || root.SecretKey == "" {
		return invalidArg("root credentials must not be empty")
	}
	root.Status = domain.StatusEnabled
	root.SessionToken = ""
	root.Expiration = time.Time{}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.root = &root
	s.stsKey = stsSigningKey(root.AccessKey, root.SecretKey)
	if s.st != nil && s.st.accessKeyInUse(root.AccessKey) {
		s.deps.Logger.WarnContext(ctx, "iam: an IAM credential uses the root access key and is shadowed by root", "accessKey", root.AccessKey)
	}
	return nil
}

// Invalidate implements Service.
func (s *service) Invalidate(context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st = nil
}

// isRootKey reports whether ak is the root access key. Callers hold mu.
func (s *service) isRootKey(ak string) bool {
	return s.root != nil && ak != "" && ak == s.root.AccessKey
}

// withRead runs fn under the read lock with the loaded state.
func (s *service) withRead(ctx context.Context, fn func(st *state) error) error {
	for {
		s.mu.RLock()
		if st := s.st; st != nil {
			defer s.mu.RUnlock()
			return fn(st)
		}
		s.mu.RUnlock()
		if err := s.ensureLoaded(ctx); err != nil {
			return err
		}
	}
}

// withWrite runs fn under the write lock with the loaded state. fn must
// persist every change before applying it to the state.
func (s *service) withWrite(ctx context.Context, fn func(st *state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		st, err := s.load(ctx)
		if err != nil {
			return err
		}
		s.st = st
	}
	return fn(s.st)
}

func (s *service) ensureLoaded(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != nil {
		return nil
	}
	st, err := s.load(ctx)
	if err != nil {
		return err
	}
	s.st = st
	return nil
}

// load reads the whole state from the store.
func (s *service) load(ctx context.Context) (*state, error) {
	st := newState()
	if err := loadNamespace(ctx, s.deps.Store, NamespaceUsers, st.users, func(r *userRecord) string { return r.AccessKey }); err != nil {
		return nil, err
	}
	if err := loadNamespace(ctx, s.deps.Store, NamespaceGroups, st.groups, func(r *groupRecord) string { return r.Name }); err != nil {
		return nil, err
	}
	if err := loadNamespace(ctx, s.deps.Store, NamespaceServiceAccounts, st.sas, func(r *serviceAccountRecord) string { return r.AccessKey }); err != nil {
		return nil, err
	}
	if err := loadNamespace(ctx, s.deps.Store, NamespaceSTS, st.sts, func(r *stsRecord) string { return r.AccessKey }); err != nil {
		return nil, err
	}
	return st, nil
}

// loadNamespace decodes every entry of ns into dst, checking that each
// record is stored under its own key.
func loadNamespace[T any](ctx context.Context, store Store, ns Namespace, dst map[string]*T, keyOf func(*T) string) error {
	entries, err := store.List(ctx, ns)
	if err != nil {
		return fmt.Errorf("iam: load %s: %w", ns, err)
	}
	for k, raw := range entries {
		rec := new(T)
		if err := json.Unmarshal(raw, rec); err != nil {
			return corrupted(fmt.Sprintf("entry %s/%s", ns, k), err)
		}
		if keyOf(rec) != k {
			return corrupted(fmt.Sprintf("entry %s/%s", ns, k), fmt.Errorf("record names %q", keyOf(rec)))
		}
		dst[k] = rec
	}
	return nil
}

// put persists a record.
func (s *service) put(ctx context.Context, ns Namespace, key string, rec any) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("iam: encode %s/%s: %w", ns, key, err)
	}
	if err := s.deps.Store.Put(ctx, ns, key, raw); err != nil {
		return fmt.Errorf("iam: save %s/%s: %w", ns, key, err)
	}
	return nil
}

// del removes a record from the store.
func (s *service) del(ctx context.Context, ns Namespace, key string) error {
	if err := s.deps.Store.Delete(ctx, ns, key); err != nil {
		return fmt.Errorf("iam: delete %s/%s: %w", ns, key, err)
	}
	return nil
}

// accessKeyInUse reports whether a user, service account or STS credential
// uses ak.
func (st *state) accessKeyInUse(ak string) bool {
	_, u := st.users[ak]
	_, sa := st.sas[ak]
	_, t := st.sts[ak]
	return u || sa || t
}

// groupsOf returns the groups user belongs to, sorted; enabledOnly skips
// the disabled groups.
func (st *state) groupsOf(user string, enabledOnly bool) []string {
	var out []string
	for name, g := range st.groups {
		if enabledOnly && g.Status == domain.StatusDisabled {
			continue
		}
		if slices.Contains(g.Members, user) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// effectivePolicies returns the policies of a user: its own and those of
// its enabled groups, sorted and deduplicated.
func (st *state) effectivePolicies(u *userRecord, groups []string) []string {
	names := slices.Clone(u.PolicyNames)
	for _, g := range groups {
		names = append(names, st.groups[g].PolicyNames...)
	}
	return sortedUnique(names)
}

// sortedKeys returns the keys of m, sorted.
func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// clonePolicy deep-copies a policy so that callers cannot alias the
// cached one.
func clonePolicy(p *domain.Policy) *domain.Policy {
	if p == nil {
		return nil
	}
	out := &domain.Policy{ID: p.ID, Version: p.Version}
	if p.Statement != nil {
		out.Statement = make(domain.Statements, len(p.Statement))
	}
	for i, st := range p.Statement {
		out.Statement[i] = domain.Statement{
			SID:          st.SID,
			Effect:       st.Effect,
			Principal:    clonePrincipal(st.Principal),
			NotPrincipal: clonePrincipal(st.NotPrincipal),
			Action:       slices.Clone(st.Action),
			NotAction:    slices.Clone(st.NotAction),
			Resource:     slices.Clone(st.Resource),
			NotResource:  slices.Clone(st.NotResource),
		}
		if st.Condition != nil {
			cond := make(domain.Condition, len(st.Condition))
			for op, keys := range st.Condition {
				m := make(map[string]domain.StringSet, len(keys))
				for k, v := range keys {
					m[k] = slices.Clone(v)
				}
				cond[op] = m
			}
			out.Statement[i].Condition = cond
		}
	}
	return out
}

func clonePrincipal(p *domain.Principal) *domain.Principal {
	if p == nil {
		return nil
	}
	return &domain.Principal{AWS: slices.Clone(p.AWS), Service: slices.Clone(p.Service), Federated: slices.Clone(p.Federated)}
}
