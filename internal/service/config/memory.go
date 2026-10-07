package config

import (
	"context"
	"sync"
)

// memoryStore is a Store that keeps the configuration in memory.
type memoryStore struct {
	mu   sync.Mutex
	data map[string]KV
}

var _ Store = (*memoryStore)(nil)

// NewMemoryStore returns an in-memory Store, optionally seeded.
func NewMemoryStore(seed map[string]KV) Store {
	return &memoryStore{data: cloneAll(seed)}
}

// Load implements Store.
func (m *memoryStore) Load(_ context.Context) (map[string]KV, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneAll(m.data), nil
}

// Save implements Store.
func (m *memoryStore) Save(_ context.Context, all map[string]KV) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = cloneAll(all)
	return nil
}

func cloneAll(in map[string]KV) map[string]KV {
	out := make(map[string]KV, len(in))
	for k, v := range in {
		out[k] = v.Clone()
	}
	return out
}
