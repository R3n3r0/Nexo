package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// DocsFile is the document of the whole configuration in the system
// document store (NewDocsStore).
const DocsFile = "config.json"

// docsStore keeps the whole configuration in one document of a
// sysstore.Store shared by the nodes of a deployment (SNMD, distributed).
type docsStore struct {
	docs sysstore.Store
}

var _ Store = (*docsStore)(nil)

// NewDocsStore returns a Store persisting the configuration in the
// DocsFile document of docs. A missing document is an empty
// configuration.
func NewDocsStore(docs sysstore.Store) (Store, error) {
	if docs == nil {
		return nil, fmt.Errorf("%w: config docs store: the document store is required", domain.ErrInvalidArgument)
	}
	return &docsStore{docs: docs}, nil
}

// Load implements Store. A damaged document is domain.ErrInvalidFormat.
func (s *docsStore) Load(ctx context.Context) (map[string]KV, error) {
	data, err := s.docs.Get(ctx, DocsFile)
	if errors.Is(err, sysstore.ErrNotFound) {
		return map[string]KV{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config docs store: %w", err)
	}
	all := map[string]KV{}
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("config docs store: %s: %w: %v", DocsFile, domain.ErrInvalidFormat, err)
	}
	for name, kv := range all {
		if kv == nil {
			all[name] = KV{}
		}
	}
	return all, nil
}

// Save implements Store.
func (s *docsStore) Save(ctx context.Context, all map[string]KV) error {
	if all == nil {
		all = map[string]KV{}
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("config docs store: encode: %w", err)
	}
	if err := s.docs.Put(ctx, DocsFile, data); err != nil {
		return fmt.Errorf("config docs store: %w", err)
	}
	return nil
}
