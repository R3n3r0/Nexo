package policy

import (
	"context"
	"encoding/json"
	"sync"
)

// memoryStore keeps the snapshot in memory (tests, bootstrap).
type memoryStore struct {
	mu   sync.Mutex
	snap *Snapshot
}

var _ PolicyStore = (*memoryStore)(nil)

// NewMemoryStore returns an in-memory PolicyStore, optionally seeded.
func NewMemoryStore(seed *Snapshot) PolicyStore {
	return &memoryStore{snap: cloneSnapshot(seed)}
}

// Load implements PolicyStore.
func (m *memoryStore) Load(context.Context) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneSnapshot(m.snap), nil
}

// Save implements PolicyStore.
func (m *memoryStore) Save(_ context.Context, s *Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snap = cloneSnapshot(s)
	return nil
}

// cloneSnapshot deep-copies a snapshot (through JSON, the persisted form),
// normalizing nil maps.
func cloneSnapshot(s *Snapshot) *Snapshot {
	out := &Snapshot{Policies: map[string]Named{}, Users: map[string][]string{}, Groups: map[string][]string{}}
	if s == nil {
		return out
	}
	data, err := json.Marshal(s)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, out)
	if out.Policies == nil {
		out.Policies = map[string]Named{}
	}
	if out.Users == nil {
		out.Users = map[string][]string{}
	}
	if out.Groups == nil {
		out.Groups = map[string][]string{}
	}
	return out
}
