package policy

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
)

// memoryStore keeps the policies and attachments in memory (tests,
// bootstrap, and the cache of the file store).
type memoryStore struct {
	mu       sync.RWMutex
	policies map[string]Named
	attached map[EntityKind]map[string][]string
}

var _ PolicyStore = (*memoryStore)(nil)

// NewMemoryStore returns an empty in-memory PolicyStore.
func NewMemoryStore() PolicyStore {
	return newMemoryStore()
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		policies: map[string]Named{},
		attached: map[EntityKind]map[string][]string{EntityUser: {}, EntityGroup: {}},
	}
}

// Get implements PolicyStore.
func (m *memoryStore) Get(_ context.Context, name string) (Named, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.policies[name]
	if !ok {
		return Named{}, fmt.Errorf("%w: %s", domain.ErrPolicyNotFound, name)
	}
	return n, nil
}

// Set implements PolicyStore.
func (m *memoryStore) Set(_ context.Context, p Named) error {
	if !validPolicyName(p.Name) || p.Policy == nil {
		return fmt.Errorf("%w: policy store: invalid policy %q", domain.ErrInvalidArgument, p.Name)
	}
	p.Policy = clonePolicy(p.Policy)
	p.BuiltIn = false
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policies[p.Name] = p
	return nil
}

// Delete implements PolicyStore.
func (m *memoryStore) Delete(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.policies[name]; !ok {
		return fmt.Errorf("%w: %s", domain.ErrPolicyNotFound, name)
	}
	delete(m.policies, name)
	return nil
}

// List implements PolicyStore.
func (m *memoryStore) List(context.Context) ([]Named, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Named, 0, len(m.policies))
	for _, n := range m.policies {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Attached implements PolicyStore.
func (m *memoryStore) Attached(_ context.Context, e Entity) ([]string, error) {
	if err := checkEntity(e); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.attached[e.Kind][e.Name]...), nil
}

// SetAttached implements PolicyStore.
func (m *memoryStore) SetAttached(_ context.Context, e Entity, names []string) error {
	if err := checkEntity(e); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setAttachedLocked(e, names)
	return nil
}

func (m *memoryStore) setAttachedLocked(e Entity, names []string) {
	if list := normalizeNames(names); len(list) > 0 {
		m.attached[e.Kind][e.Name] = list
	} else {
		delete(m.attached[e.Kind], e.Name)
	}
}

// ListAttached implements PolicyStore.
func (m *memoryStore) ListAttached(_ context.Context, kind EntityKind) (map[string][]string, error) {
	if err := checkEntity(Entity{Kind: kind, Name: "-"}); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneMapping(m.attached[kind]), nil
}

// checkEntity validates an entity of the attachment API.
func checkEntity(e Entity) error {
	if e.Kind != EntityUser && e.Kind != EntityGroup {
		return fmt.Errorf("%w: policy store: unknown entity kind %q", domain.ErrInvalidArgument, e.Kind)
	}
	if e.Name == "" {
		return fmt.Errorf("%w: policy store: empty %s name", domain.ErrInvalidArgument, e.Kind)
	}
	return nil
}

// normalizeNames sorts and deduplicates policy names, dropping empty ones.
func normalizeNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func cloneMapping(m map[string][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = append([]string(nil), v...)
	}
	return out
}
