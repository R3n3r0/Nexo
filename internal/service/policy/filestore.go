package policy

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
)

// File store layout, under the store directory:
//
//	policies/<name>.json   one Named document per policy; names outside
//	                       [a-z0-9+=,@_.-] (or starting with ".") are stored
//	                       as "~<base32(name)>.json" so that they stay
//	                       unique on case-insensitive file systems
//	users.json             {"<user>": ["<policy>", ...]}
//	groups.json            {"<group>": ["<policy>", ...]}
//
// Every write goes to a temporary file in the same directory, is synced
// and renamed over the target (atomic replace), then the directory is
// synced.
const (
	filePoliciesDir = "policies"
	fileUsers       = "users.json"
	fileGroups      = "groups.json"
	fileTempPrefix  = ".tmp-"
)

// fileStore persists the policies in a directory. It assumes it is the
// only writer of the directory: the content is loaded once by
// NewFileStore and then served from memory, every change being written
// through to disk before it becomes visible.
type fileStore struct {
	dir string

	mu    sync.Mutex // serializes writes; the cache has its own lock
	cache *memoryStore
}

var _ PolicyStore = (*fileStore)(nil)

// NewFileStore returns a PolicyStore persisted in dir (created with mode
// 0700 when missing) and loads its content. A malformed file is an error
// wrapping domain.ErrInvalidFormat: the store never starts with a partial
// view of the policies.
func NewFileStore(dir string) (PolicyStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: policy file store: empty directory", domain.ErrInvalidArgument)
	}
	if err := os.MkdirAll(filepath.Join(dir, filePoliciesDir), 0o700); err != nil {
		return nil, fmt.Errorf("policy file store: %w", err)
	}
	s := &fileStore{dir: dir, cache: newMemoryStore()}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// policyFileName maps a policy name to its file name.
func policyFileName(name string) string {
	safe := !strings.HasPrefix(name, ".")
	for _, r := range name {
		if !isLowerFileRune(r) {
			safe = false
			break
		}
	}
	if safe {
		return name + ".json"
	}
	return "~" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(name))) + ".json"
}

// isLowerFileRune reports whether r may appear as is in a policy file
// name: lowercase letters, digits and the IAM name punctuation.
func isLowerFileRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	default:
		return strings.ContainsRune("+=,@_.-", r)
	}
}

func (s *fileStore) load() error {
	policiesDir := filepath.Join(s.dir, filePoliciesDir)
	entries, err := os.ReadDir(policiesDir)
	if err != nil {
		return fmt.Errorf("policy file store: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), fileTempPrefix) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(policiesDir, e.Name())) // #nosec G304 -- a file of the policy directory of the server
		if err != nil {
			return fmt.Errorf("policy file store: %w", err)
		}
		var n Named
		if err := json.Unmarshal(data, &n); err != nil {
			return fmt.Errorf("%w: policy file %s: %v", domain.ErrInvalidFormat, e.Name(), err)
		}
		if !validPolicyName(n.Name) || n.Policy == nil || policyFileName(n.Name) != e.Name() {
			return fmt.Errorf("%w: policy file %s: name %q does not match", domain.ErrInvalidFormat, e.Name(), n.Name)
		}
		n.BuiltIn = false
		s.cache.policies[n.Name] = n
	}
	for kind, file := range map[EntityKind]string{EntityUser: fileUsers, EntityGroup: fileGroups} {
		data, err := os.ReadFile(filepath.Join(s.dir, file)) // #nosec G304 -- a file of the policy directory of the server, named after a validated policy name
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("policy file store: %w", err)
		}
		var m map[string][]string
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("%w: policy file %s: %v", domain.ErrInvalidFormat, file, err)
		}
		for name, names := range m {
			if name != "" {
				s.cache.setAttachedLocked(Entity{Kind: kind, Name: name}, names)
			}
		}
	}
	return nil
}

// Get implements PolicyStore.
func (s *fileStore) Get(ctx context.Context, name string) (Named, error) {
	return s.cache.Get(ctx, name)
}

// List implements PolicyStore.
func (s *fileStore) List(ctx context.Context) ([]Named, error) {
	return s.cache.List(ctx)
}

// Attached implements PolicyStore.
func (s *fileStore) Attached(ctx context.Context, e Entity) ([]string, error) {
	return s.cache.Attached(ctx, e)
}

// ListAttached implements PolicyStore.
func (s *fileStore) ListAttached(ctx context.Context, kind EntityKind) (map[string][]string, error) {
	return s.cache.ListAttached(ctx, kind)
}

// Set implements PolicyStore.
func (s *fileStore) Set(ctx context.Context, p Named) error {
	if !validPolicyName(p.Name) || p.Policy == nil {
		return fmt.Errorf("%w: policy store: invalid policy %q", domain.ErrInvalidArgument, p.Name)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.BuiltIn = false
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("policy file store: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeFileAtomic(filepath.Join(s.dir, filePoliciesDir, policyFileName(p.Name)), data); err != nil {
		return err
	}
	return s.cache.Set(ctx, p)
}

// Delete implements PolicyStore.
func (s *fileStore) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.cache.Get(ctx, name); err != nil {
		return err
	}
	dir := filepath.Join(s.dir, filePoliciesDir)
	if err := os.Remove(filepath.Join(dir, policyFileName(name))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("policy file store: %w", err)
	}
	syncDir(dir)
	return s.cache.Delete(ctx, name)
}

// SetAttached implements PolicyStore.
func (s *fileStore) SetAttached(ctx context.Context, e Entity, names []string) error {
	if err := checkEntity(e); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mapping, err := s.cache.ListAttached(ctx, e.Kind)
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
		return fmt.Errorf("policy file store: %w", err)
	}
	file := fileUsers
	if e.Kind == EntityGroup {
		file = fileGroups
	}
	if err := writeFileAtomic(filepath.Join(s.dir, file), data); err != nil {
		return err
	}
	return s.cache.SetAttached(ctx, e, names)
}

// writeFileAtomic replaces path with data: temporary file in the same
// directory, fsync, rename, directory fsync.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, fileTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("policy file store: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("policy file store: %w", err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("policy file store: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("policy file store: %w", err)
	}
	if err = os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("policy file store: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("policy file store: %w", err)
	}
	syncDir(dir)
	return nil
}

// syncDir flushes a directory entry change. It is best effort: some
// platforms (Windows) cannot sync directories.
func syncDir(dir string) {
	d, err := os.Open(dir) // #nosec G304 -- the policy directory of the server
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
