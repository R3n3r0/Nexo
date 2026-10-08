package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// Paths of the policy documents in the system document store.
const (
	// DocsDir is the directory of the policy documents.
	DocsDir         = "policies"
	docsNamedDir    = DocsDir + "/named"
	docsUsersFile   = DocsDir + "/users.json"
	docsGroupsFile  = DocsDir + "/groups.json"
	docsFileSuffix  = ".json"
	docsAttachTitle = "policy docs store"
)

// docsStore persists the policies in a sysstore.Store shared by the nodes
// of a deployment. The content is loaded lazily and cached; Invalidate
// drops the cache when another node changed it (peer notification). The
// cache is guarded by a generation counter, so that a load racing with an
// invalidation never installs the state it read before.
type docsStore struct {
	docs sysstore.Store

	mu     sync.Mutex // serializes the writes
	cacheM sync.Mutex // guards cache and gen
	cache  *memoryStore
	gen    uint64
}

var _ SharedStore = (*docsStore)(nil)

// NewDocsStore returns a SharedStore persisted in the DocsDir directory of
// docs: one document per named policy and one mapping document per entity
// kind. The attachment mappings are rewritten whole: concurrent
// attachments from several nodes are not merged (the IAM service, through
// Deps.Attachments, is the source of truth of the attachments).
func NewDocsStore(docs sysstore.Store) (SharedStore, error) {
	if docs == nil {
		return nil, fmt.Errorf("%w: %s: the document store is required", domain.ErrInvalidArgument, docsAttachTitle)
	}
	return &docsStore{docs: docs}, nil
}

// Invalidate implements SharedStore.
func (s *docsStore) Invalidate(context.Context) {
	s.cacheM.Lock()
	defer s.cacheM.Unlock()
	s.cache = nil
	s.gen++
}

// current returns the cache, loading it when needed.
func (s *docsStore) current(ctx context.Context) (*memoryStore, error) {
	s.cacheM.Lock()
	c, gen := s.cache, s.gen
	s.cacheM.Unlock()
	if c != nil {
		return c, nil
	}
	loaded, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	s.cacheM.Lock()
	defer s.cacheM.Unlock()
	if s.gen == gen && s.cache == nil {
		s.cache = loaded
	}
	return loaded, nil
}

// load reads every policy and mapping document.
func (s *docsStore) load(ctx context.Context) (*memoryStore, error) {
	m := newMemoryStore()
	named, err := s.docs.List(ctx, docsNamedDir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", docsAttachTitle, err)
	}
	for file, data := range named {
		if !strings.HasSuffix(file, docsFileSuffix) {
			continue
		}
		var n Named
		if err := json.Unmarshal(data, &n); err != nil {
			return nil, fmt.Errorf("%w: policy document %s: %v", domain.ErrInvalidFormat, file, err)
		}
		if !validPolicyName(n.Name) || n.Policy == nil || policyFileName(n.Name) != file {
			return nil, fmt.Errorf("%w: policy document %s: name %q does not match", domain.ErrInvalidFormat, file, n.Name)
		}
		n.BuiltIn = false
		m.policies[n.Name] = n
	}
	for kind, file := range map[EntityKind]string{EntityUser: docsUsersFile, EntityGroup: docsGroupsFile} {
		data, err := s.docs.Get(ctx, file)
		if errors.Is(err, sysstore.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", docsAttachTitle, err)
		}
		var mapping map[string][]string
		if err := json.Unmarshal(data, &mapping); err != nil {
			return nil, fmt.Errorf("%w: policy document %s: %v", domain.ErrInvalidFormat, file, err)
		}
		for name, names := range mapping {
			if name != "" {
				m.setAttachedLocked(Entity{Kind: kind, Name: name}, names)
			}
		}
	}
	return m, nil
}

// Get implements PolicyStore.
func (s *docsStore) Get(ctx context.Context, name string) (Named, error) {
	c, err := s.current(ctx)
	if err != nil {
		return Named{}, err
	}
	return c.Get(ctx, name)
}

// List implements PolicyStore.
func (s *docsStore) List(ctx context.Context) ([]Named, error) {
	c, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	return c.List(ctx)
}

// Attached implements PolicyStore.
func (s *docsStore) Attached(ctx context.Context, e Entity) ([]string, error) {
	c, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	return c.Attached(ctx, e)
}

// ListAttached implements PolicyStore.
func (s *docsStore) ListAttached(ctx context.Context, kind EntityKind) (map[string][]string, error) {
	c, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListAttached(ctx, kind)
}

// Set implements PolicyStore.
func (s *docsStore) Set(ctx context.Context, p Named) error {
	if !validPolicyName(p.Name) || p.Policy == nil {
		return fmt.Errorf("%w: policy store: invalid policy %q", domain.ErrInvalidArgument, p.Name)
	}
	p.BuiltIn = false
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: %w", docsAttachTitle, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.current(ctx)
	if err != nil {
		return err
	}
	if err := s.docs.Put(ctx, docsNamedDir+"/"+policyFileName(p.Name), data); err != nil {
		s.Invalidate(ctx)
		return fmt.Errorf("%s: %w", docsAttachTitle, err)
	}
	return c.Set(ctx, p)
}

// Delete implements PolicyStore.
func (s *docsStore) Delete(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.current(ctx)
	if err != nil {
		return err
	}
	if _, err := c.Get(ctx, name); err != nil {
		return err
	}
	if err := s.docs.Delete(ctx, docsNamedDir+"/"+policyFileName(name)); err != nil {
		s.Invalidate(ctx)
		return fmt.Errorf("%s: %w", docsAttachTitle, err)
	}
	return c.Delete(ctx, name)
}

// SetAttached implements PolicyStore.
func (s *docsStore) SetAttached(ctx context.Context, e Entity, names []string) error {
	if err := checkEntity(e); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.current(ctx)
	if err != nil {
		return err
	}
	mapping, err := c.ListAttached(ctx, e.Kind)
	if err != nil {
		return err
	}
	if list := normalizeNames(names); len(list) > 0 {
		mapping[e.Name] = list
	} else {
		delete(mapping, e.Name)
	}
	data, err := json.MarshalIndent(mapping, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: %w", docsAttachTitle, err)
	}
	file := docsUsersFile
	if e.Kind == EntityGroup {
		file = docsGroupsFile
	}
	if err := s.docs.Put(ctx, file, data); err != nil {
		s.Invalidate(ctx)
		return fmt.Errorf("%s: %w", docsAttachTitle, err)
	}
	return c.SetAttached(ctx, e, names)
}
