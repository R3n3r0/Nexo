package kms

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// DocsKeysDir is the directory of the key registry in the replicated
// system documents: one document per key (kms/keys/<name>.json), created
// atomically.
const DocsKeysDir = "kms/keys"

// keyDocSuffix ends the name of a key document.
const keyDocSuffix = ".json"

// docsKeyStore keeps the key registry in the replicated system documents,
// shared by every node of the deployment.
type docsKeyStore struct {
	docs sysstore.Store
}

var _ KeyStore = (*docsKeyStore)(nil)

// NewDocsKeyStore returns the key registry on the system document store
// (SNMD and cluster).
func NewDocsKeyStore(docs sysstore.Store) (KeyStore, error) {
	if docs == nil {
		return nil, fmt.Errorf("%w: kms key store: the document store is required", domain.ErrInvalidArgument)
	}
	return &docsKeyStore{docs: docs}, nil
}

// keyDoc is the stored form of a key: the random id tells the document
// of a creation from the one of a concurrent creation of the same name.
type keyDoc struct {
	KeyInfo
	ID string `json:"id"`
}

func encodeKey(key KeyInfo) ([]byte, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("kms key store: random id: %w", err)
	}
	data, err := json.Marshal(keyDoc{KeyInfo: key, ID: hex.EncodeToString(id)})
	if err != nil {
		return nil, fmt.Errorf("kms key store: encode %q: %w", key.Name, err)
	}
	return data, nil
}

func decodeKey(name string, data []byte) (KeyInfo, error) {
	var doc keyDoc
	if err := json.Unmarshal(data, &doc); err != nil || doc.Name != name {
		return KeyInfo{}, fmt.Errorf("kms key store: key %q: %w", name, domain.ErrInvalidFormat)
	}
	return doc.KeyInfo, nil
}

// Create implements KeyStore.
func (s *docsKeyStore) Create(ctx context.Context, key KeyInfo) error {
	if err := validKeyName(key.Name); err != nil {
		return err
	}
	data, err := encodeKey(key)
	if err != nil {
		return err
	}
	stored, err := s.docs.Create(ctx, DocsKeysDir+"/"+key.Name+keyDocSuffix, data)
	if err != nil {
		return fmt.Errorf("kms key store: create %q: %w", key.Name, err)
	}
	if !bytes.Equal(stored, data) {
		return fmt.Errorf("kms key store: %q: %w", key.Name, domain.ErrKMSKeyExists)
	}
	return nil
}

// Get implements KeyStore.
func (s *docsKeyStore) Get(ctx context.Context, name string) (KeyInfo, error) {
	if err := validKeyName(name); err != nil {
		return KeyInfo{}, err
	}
	data, err := s.docs.Get(ctx, DocsKeysDir+"/"+name+keyDocSuffix)
	if errors.Is(err, sysstore.ErrNotFound) {
		return KeyInfo{}, fmt.Errorf("kms key store: %q: %w", name, domain.ErrKMSKeyNotFound)
	}
	if err != nil {
		return KeyInfo{}, fmt.Errorf("kms key store: get %q: %w", name, err)
	}
	return decodeKey(name, data)
}

// List implements KeyStore.
func (s *docsKeyStore) List(ctx context.Context) ([]KeyInfo, error) {
	docs, err := s.docs.List(ctx, DocsKeysDir)
	if errors.Is(err, sysstore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kms key store: list: %w", err)
	}
	out := make([]KeyInfo, 0, len(docs))
	for file, data := range docs {
		name, ok := strings.CutSuffix(file, keyDocSuffix)
		if !ok {
			continue
		}
		key, err := decodeKey(name, data)
		if err != nil {
			continue
		}
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// fileKeyStore keeps the key registry in a directory of the single drive:
// one file per key, created exclusively.
type fileKeyStore struct {
	dir string
}

var _ KeyStore = (*fileKeyStore)(nil)

// NewFileKeyStore returns the key registry in dir (SNSD: a directory of
// the system volume of the drive), created when missing.
func NewFileKeyStore(dir string) (KeyStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: kms key store: empty directory", domain.ErrInvalidArgument)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("kms key store: %w", err)
	}
	return &fileKeyStore{dir: dir}, nil
}

// Create implements KeyStore.
func (s *fileKeyStore) Create(ctx context.Context, key KeyInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validKeyName(key.Name); err != nil {
		return err
	}
	data, err := encodeKey(key)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, key.Name+keyDocSuffix)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("kms key store: %q: %w", key.Name, domain.ErrKMSKeyExists)
	}
	if err != nil {
		return fmt.Errorf("kms key store: create %q: %w", key.Name, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("kms key store: write %q: %w", key.Name, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("kms key store: sync %q: %w", key.Name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("kms key store: close %q: %w", key.Name, err)
	}
	return nil
}

// Get implements KeyStore.
func (s *fileKeyStore) Get(ctx context.Context, name string) (KeyInfo, error) {
	if err := ctx.Err(); err != nil {
		return KeyInfo{}, err
	}
	if err := validKeyName(name); err != nil {
		return KeyInfo{}, err
	}
	data, err := os.ReadFile(filepath.Join(s.dir, name+keyDocSuffix))
	if errors.Is(err, fs.ErrNotExist) {
		return KeyInfo{}, fmt.Errorf("kms key store: %q: %w", name, domain.ErrKMSKeyNotFound)
	}
	if err != nil {
		return KeyInfo{}, fmt.Errorf("kms key store: read %q: %w", name, err)
	}
	return decodeKey(name, data)
}

// List implements KeyStore.
func (s *fileKeyStore) List(ctx context.Context) ([]KeyInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("kms key store: list: %w", err)
	}
	out := make([]KeyInfo, 0, len(entries))
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), keyDocSuffix)
		if !ok || e.IsDir() {
			continue
		}
		key, err := s.Get(ctx, name)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
