package iam

import (
	"context"
	"fmt"
	"sync"
	"unicode/utf8"
)

// maxStoreKeyLen bounds store keys (bytes).
const maxStoreKeyLen = 1024

// validateEntry checks a namespace and a key (see Store).
func validateEntry(ns Namespace, key string) error {
	if err := validateNamespace(ns); err != nil {
		return err
	}
	if key == "" || len(key) > maxStoreKeyLen || !utf8.ValidString(key) {
		return invalidArg("store key must be 1 to %d bytes of UTF-8", maxStoreKeyLen)
	}
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return invalidArg("store key contains a NUL byte")
		}
	}
	return nil
}

func validateNamespace(ns Namespace) error {
	if ns == "" || len(ns) > 64 {
		return invalidArg("store namespace %q must be 1 to 64 characters", ns)
	}
	for i := 0; i < len(ns); i++ {
		c := ns[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return invalidArg("store namespace %q may only contain [a-z0-9-]", ns)
		}
	}
	return nil
}

// memoryStore keeps the entries in memory (tests, single-process setups
// that do not need persistence).
type memoryStore struct {
	mu      sync.RWMutex
	entries map[Namespace]map[string][]byte
}

var _ Store = (*memoryStore)(nil)

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() Store {
	return &memoryStore{entries: make(map[Namespace]map[string][]byte)}
}

// Get implements Store.
func (m *memoryStore) Get(_ context.Context, ns Namespace, key string) ([]byte, error) {
	if err := validateEntry(ns, key); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.entries[ns][key]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrEntryNotFound, ns, key)
	}
	return cloneBytes(v), nil
}

// Put implements Store.
func (m *memoryStore) Put(_ context.Context, ns Namespace, key string, value []byte) error {
	if err := validateEntry(ns, key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries[ns] == nil {
		m.entries[ns] = make(map[string][]byte)
	}
	m.entries[ns][key] = cloneBytes(value)
	return nil
}

// Delete implements Store.
func (m *memoryStore) Delete(_ context.Context, ns Namespace, key string) error {
	if err := validateEntry(ns, key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries[ns], key)
	return nil
}

// List implements Store.
func (m *memoryStore) List(_ context.Context, ns Namespace) (map[string][]byte, error) {
	if err := validateNamespace(ns); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string][]byte, len(m.entries[ns]))
	for k, v := range m.entries[ns] {
		out[k] = cloneBytes(v)
	}
	return out, nil
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}
