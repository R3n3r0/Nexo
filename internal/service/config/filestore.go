package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
)

// fileStore is a Store that keeps the whole configuration in one JSON
// document, replaced atomically (temporary file, fsync, rename) on every
// Save. The file is private to the server (mode 0600): settings may hold
// credentials of notification targets.
type fileStore struct {
	mu   sync.Mutex
	path string
}

var _ Store = (*fileStore)(nil)

// fileStoreTempPrefix prefixes the temporary files of the atomic writes.
const fileStoreTempPrefix = ".config-"

// NewFileStore returns a Store persisting the configuration in the JSON
// file path (on the system volume: <drive>/.nexo.sys/config/config.json).
// The parent directory is created when missing; a missing file is an empty
// configuration.
func NewFileStore(path string) (Store, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: config file store: empty path", domain.ErrInvalidArgument)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("config file store: %w", err)
	}
	return &fileStore{path: path}, nil
}

// Load implements Store. A damaged document is domain.ErrInvalidFormat.
func (s *fileStore) Load(ctx context.Context) (map[string]KV, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]KV{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config file store: %w", err)
	}
	all := map[string]KV{}
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("config file store: %s: %w: %v", s.path, domain.ErrInvalidFormat, err)
	}
	for name, kv := range all {
		if kv == nil {
			all[name] = KV{}
		}
	}
	return all, nil
}

// Save implements Store.
func (s *fileStore) Save(ctx context.Context, all map[string]KV) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if all == nil {
		all = map[string]KV{}
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("config file store: encode: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeFileAtomic(s.path, data)
}

// writeFileAtomic replaces path with data: a reader sees the old or the
// new document, never a partial one.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, fileStoreTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("config file store: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("config file store: %w", err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("config file store: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("config file store: %w", err)
	}
	if err = os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("config file store: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config file store: %w", err)
	}
	// Flush the directory entry (best effort: not every platform can).
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
