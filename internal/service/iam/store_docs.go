package iam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// docsStore persists the encrypted entries in a sysstore.Store, the
// replicated document store of the erasure-coded deployments (SNMD and
// distributed): every node reads and writes the same documents. The layout
// and the encryption are those of the file store:
//
//	<dir>/kdf.json                  Argon2id salt and parameters, key check
//	<dir>/<namespace>/<sha256>.json one encrypted entry per key
type docsStore struct {
	docs sysstore.Store
	dir  string
	key  []byte
}

var _ Store = (*docsStore)(nil)

// DocsDir is the directory of the IAM documents in the system document
// store (NewDocsStore).
const DocsDir = "iam"

// NewDocsStore opens (creating its kdf.json when needed) the encrypted
// store in the directory DocsDir of docs. The encryption key is derived
// with Argon2id from rootSecret; every node of a deployment agrees on the
// single salt stored by the first one. A store written under another root
// secret returns ErrWrongKey.
func NewDocsStore(ctx context.Context, docs sysstore.Store, rootSecret string, opts ...FileStoreOption) (Store, error) {
	if docs == nil {
		return nil, invalidArg("docs store: the document store is required")
	}
	if rootSecret == "" {
		return nil, invalidArg("docs store requires the root secret key")
	}
	o := fileStoreOptions{kdf: defaultKDFParams()}
	for _, opt := range opts {
		opt(&o)
	}
	if err := o.kdf.validate(); err != nil {
		return nil, invalidArg("docs store: %v", err)
	}
	kdfPath := DocsDir + "/" + kdfFileName
	raw, err := docs.Get(ctx, kdfPath)
	if errors.Is(err, sysstore.ErrNotFound) {
		fresh, ferr := newKDF(kdfPath, rootSecret, o.kdf)
		if ferr != nil {
			return nil, ferr
		}
		raw, err = docs.Create(ctx, kdfPath, fresh)
	}
	if err != nil {
		return nil, fmt.Errorf("iam: docs store: read %s: %w", kdfPath, err)
	}
	key, err := verifyKDF(raw, kdfPath, rootSecret)
	if err != nil {
		return nil, err
	}
	return &docsStore{docs: docs, dir: DocsDir, key: key}, nil
}

func (s *docsStore) entryPath(ns Namespace, key string) string {
	return s.dir + "/" + string(ns) + "/" + entryFileName(key)
}

// Get implements Store.
func (s *docsStore) Get(ctx context.Context, ns Namespace, key string) ([]byte, error) {
	if err := validateEntry(ns, key); err != nil {
		return nil, err
	}
	p := s.entryPath(ns, key)
	raw, err := s.docs.Get(ctx, p)
	if errors.Is(err, sysstore.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s/%s", ErrEntryNotFound, ns, key)
	}
	if err != nil {
		return nil, fmt.Errorf("iam: docs store: read %s/%s: %w", ns, key, err)
	}
	plain, err := s.decode(ns, p, raw)
	if err != nil {
		return nil, err
	}
	if plain.Key != key {
		return nil, corrupted(p, errors.New("entry belongs to another key"))
	}
	return plain.Value, nil
}

// Put implements Store.
func (s *docsStore) Put(ctx context.Context, ns Namespace, key string, value []byte) error {
	if err := validateEntry(ns, key); err != nil {
		return err
	}
	plain, err := json.Marshal(entryPlain{Key: key, Value: value})
	if err != nil {
		return fmt.Errorf("iam: docs store: encode %s/%s: %w", ns, key, err)
	}
	nonce, data, err := seal(s.key, plain, entryAAD(ns))
	if err != nil {
		return err
	}
	raw, err := json.Marshal(entryFile{Format: entryFileFormat, Version: fileFormatVersion, Nonce: nonce, Data: data})
	if err != nil {
		return fmt.Errorf("iam: docs store: encode %s/%s: %w", ns, key, err)
	}
	if err := s.docs.Put(ctx, s.entryPath(ns, key), raw); err != nil {
		return fmt.Errorf("iam: docs store: write %s/%s: %w", ns, key, err)
	}
	return nil
}

// Delete implements Store.
func (s *docsStore) Delete(ctx context.Context, ns Namespace, key string) error {
	if err := validateEntry(ns, key); err != nil {
		return err
	}
	if err := s.docs.Delete(ctx, s.entryPath(ns, key)); err != nil {
		return fmt.Errorf("iam: docs store: delete %s/%s: %w", ns, key, err)
	}
	return nil
}

// List implements Store.
func (s *docsStore) List(ctx context.Context, ns Namespace) (map[string][]byte, error) {
	if err := validateNamespace(ns); err != nil {
		return nil, err
	}
	dir := s.dir + "/" + string(ns)
	all, err := s.docs.List(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("iam: docs store: list %s: %w", ns, err)
	}
	out := make(map[string][]byte, len(all))
	for name, raw := range all {
		if !strings.HasSuffix(name, entryFileExtension) {
			continue
		}
		p := path.Join(dir, name)
		plain, err := s.decode(ns, p, raw)
		if err != nil {
			return nil, err
		}
		if entryFileName(plain.Key) != name {
			return nil, corrupted(p, errors.New("entry belongs to another key"))
		}
		out[plain.Key] = plain.Value
	}
	return out, nil
}

// decode decrypts an entry document.
func (s *docsStore) decode(ns Namespace, p string, raw []byte) (*entryPlain, error) {
	var env entryFile
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, corrupted(p, err)
	}
	if env.Format != entryFileFormat {
		return nil, corrupted(p, fmt.Errorf("unknown format %q", env.Format))
	}
	if env.Version != fileFormatVersion {
		return nil, corrupted(p, fmt.Errorf("unsupported version %d", env.Version))
	}
	plainRaw, err := open(s.key, env.Nonce, env.Data, entryAAD(ns))
	if err != nil {
		return nil, corrupted(p, err)
	}
	var plain entryPlain
	if err := json.Unmarshal(plainRaw, &plain); err != nil {
		return nil, corrupted(p, err)
	}
	return &plain, nil
}
