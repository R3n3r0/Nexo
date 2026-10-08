package iam

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// On-disk layout of the file store:
//
//	<dir>/kdf.json                 Argon2id salt and parameters (clear text)
//	                               plus a key-check value
//	<dir>/<namespace>/<sha256>.json one encrypted entry per key
//
// Every entry file is a JSON envelope {format, version, nonce, data} whose
// data is the AES-256-GCM encryption of {key, value}; the associated data
// binds it to its namespace. File names are the SHA-256 of the key, so that
// they are safe on every file system and do not reveal the key.
const (
	kdfFileName        = "kdf.json"
	kdfFileFormat      = "nexo-iam-kdf"
	entryFileFormat    = "nexo-iam-entry"
	fileFormatVersion  = 1
	keyCheckPlaintext  = "nexo-iam-key-check"
	keyCheckAAD        = "nexo/iam/key-check/v1"
	entryAADPrefix     = "nexo/iam/entry/v1/"
	entryFileExtension = ".json"
	tempFilePrefix     = ".tmp-"
	fileStoreDirPerm   = 0o700
	fileStoreFilePerm  = 0o600
	maxEntryFileSize   = 4 << 20
	maxKDFFileSize     = 64 << 10
)

// kdfFile is the clear-text key derivation document.
type kdfFile struct {
	Format    string `json:"format"`
	Version   int    `json:"version"`
	Algorithm string `json:"algorithm"`
	Salt      []byte `json:"salt"`
	KDFParams
	CheckNonce []byte `json:"checkNonce"`
	Check      []byte `json:"check"`
}

// entryFile is the envelope of an encrypted entry.
type entryFile struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	Nonce   []byte `json:"nonce"`
	Data    []byte `json:"data"`
}

// entryPlain is the decrypted content of an entry.
type entryPlain struct {
	Key   string `json:"key"`
	Value []byte `json:"value"`
}

// FileStoreOption configures NewFileStore.
type FileStoreOption func(*fileStoreOptions)

type fileStoreOptions struct {
	kdf KDFParams
}

// WithFileKDFParams sets the Argon2id parameters used when the store is
// created; an existing store keeps the parameters recorded in kdf.json.
func WithFileKDFParams(p KDFParams) FileStoreOption {
	return func(o *fileStoreOptions) { o.kdf = p.withDefaults() }
}

// fileStore persists encrypted entries in a directory.
type fileStore struct {
	dir string
	key []byte
}

var _ Store = (*fileStore)(nil)

// NewFileStore opens (creating it when needed) the encrypted store in dir,
// typically <drive>/.nexo.sys/config/iam. The encryption key is derived
// with Argon2id from rootSecret and the salt of dir/kdf.json. A store
// written under another root secret returns ErrWrongKey; a damaged
// kdf.json domain.ErrInvalidFormat.
func NewFileStore(ctx context.Context, dir, rootSecret string, opts ...FileStoreOption) (Store, error) {
	if dir == "" {
		return nil, invalidArg("file store directory is required")
	}
	if rootSecret == "" {
		return nil, invalidArg("file store requires the root secret key")
	}
	o := fileStoreOptions{kdf: defaultKDFParams()}
	for _, opt := range opts {
		opt(&o)
	}
	if err := o.kdf.validate(); err != nil {
		return nil, invalidArg("file store: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, fileStoreDirPerm); err != nil {
		return nil, fmt.Errorf("iam: file store: create %s: %w", dir, err)
	}
	key, err := openKDF(dir, rootSecret, o.kdf)
	if err != nil {
		return nil, err
	}
	return &fileStore{dir: dir, key: key}, nil
}

// openKDF reads dir/kdf.json, or creates it, and returns the verified key.
func openKDF(dir, secret string, params KDFParams) ([]byte, error) {
	path := filepath.Join(dir, kdfFileName)
	raw, err := readLimited(path, maxKDFFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		if err := createKDF(dir, path, secret, params); err != nil {
			return nil, err
		}
		raw, err = readLimited(path, maxKDFFileSize)
	}
	if err != nil {
		return nil, fmt.Errorf("iam: file store: read %s: %w", path, err)
	}
	var kf kdfFile
	if err := json.Unmarshal(raw, &kf); err != nil {
		return nil, corrupted(path, err)
	}
	if kf.Format != kdfFileFormat || kf.Algorithm != kdfAlgorithm {
		return nil, corrupted(path, fmt.Errorf("unknown format %q/%q", kf.Format, kf.Algorithm))
	}
	if kf.Version != fileFormatVersion {
		return nil, corrupted(path, fmt.Errorf("unsupported version %d", kf.Version))
	}
	if len(kf.Salt) < saltSize {
		return nil, corrupted(path, errors.New("salt too short"))
	}
	if err := kf.validate(); err != nil {
		return nil, corrupted(path, err)
	}
	key := deriveKey(secret, kf.Salt, kf.KDFParams)
	plain, err := open(key, kf.CheckNonce, kf.Check, []byte(keyCheckAAD))
	if err != nil {
		if errors.Is(err, errOpen) {
			return nil, fmt.Errorf("%w: %s", ErrWrongKey, path)
		}
		return nil, corrupted(path, err)
	}
	if string(plain) != keyCheckPlaintext {
		return nil, corrupted(path, errors.New("unexpected key-check value"))
	}
	return key, nil
}

// createKDF writes a new kdf.json with a random salt. The file is linked
// into place so that concurrent creators agree on a single salt.
func createKDF(dir, path, secret string, params KDFParams) error {
	salt, err := randomBytes(saltSize)
	if err != nil {
		return err
	}
	key := deriveKey(secret, salt, params)
	nonce, check, err := seal(key, []byte(keyCheckPlaintext), []byte(keyCheckAAD))
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(kdfFile{
		Format: kdfFileFormat, Version: fileFormatVersion, Algorithm: kdfAlgorithm,
		Salt: salt, KDFParams: params, CheckNonce: nonce, Check: check,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("iam: file store: encode %s: %w", path, err)
	}
	tmp, err := writeTemp(dir, raw)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Link(tmp, path); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("iam: file store: install %s: %w", path, err)
	}
	syncDir(dir)
	return nil
}

func (f *fileStore) nsDir(ns Namespace) string { return filepath.Join(f.dir, string(ns)) }

func (f *fileStore) entryPath(ns Namespace, key string) string {
	return filepath.Join(f.nsDir(ns), entryFileName(key))
}

// entryFileName is the file name of a key: the hex SHA-256 of the key.
func entryFileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + entryFileExtension
}

func entryAAD(ns Namespace) []byte { return []byte(entryAADPrefix + string(ns)) }

// Get implements Store.
func (f *fileStore) Get(ctx context.Context, ns Namespace, key string) ([]byte, error) {
	if err := validateEntry(ns, key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := f.entryPath(ns, key)
	plain, err := f.readEntry(ns, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s/%s", ErrEntryNotFound, ns, key)
	}
	if err != nil {
		return nil, err
	}
	if plain.Key != key {
		return nil, corrupted(path, errors.New("entry belongs to another key"))
	}
	return plain.Value, nil
}

// Put implements Store.
func (f *fileStore) Put(ctx context.Context, ns Namespace, key string, value []byte) error {
	if err := validateEntry(ns, key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	plain, err := json.Marshal(entryPlain{Key: key, Value: value})
	if err != nil {
		return fmt.Errorf("iam: file store: encode %s/%s: %w", ns, key, err)
	}
	nonce, data, err := seal(f.key, plain, entryAAD(ns))
	if err != nil {
		return err
	}
	raw, err := json.Marshal(entryFile{Format: entryFileFormat, Version: fileFormatVersion, Nonce: nonce, Data: data})
	if err != nil {
		return fmt.Errorf("iam: file store: encode %s/%s: %w", ns, key, err)
	}
	dir := f.nsDir(ns)
	if err := os.MkdirAll(dir, fileStoreDirPerm); err != nil {
		return fmt.Errorf("iam: file store: create %s: %w", dir, err)
	}
	tmp, err := writeTemp(dir, raw)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, f.entryPath(ns, key)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("iam: file store: write %s/%s: %w", ns, key, err)
	}
	syncDir(dir)
	return nil
}

// Delete implements Store.
func (f *fileStore) Delete(ctx context.Context, ns Namespace, key string) error {
	if err := validateEntry(ns, key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(f.entryPath(ns, key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("iam: file store: delete %s/%s: %w", ns, key, err)
	}
	syncDir(f.nsDir(ns))
	return nil
}

// List implements Store.
func (f *fileStore) List(ctx context.Context, ns Namespace) (map[string][]byte, error) {
	if err := validateNamespace(ns); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := f.nsDir(ns)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string][]byte{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("iam: file store: list %s: %w", dir, err)
	}
	out := make(map[string][]byte, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, entryFileExtension) {
			continue
		}
		path := filepath.Join(dir, name)
		plain, err := f.readEntry(ns, path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted concurrently
		}
		if err != nil {
			return nil, err
		}
		if entryFileName(plain.Key) != name {
			return nil, corrupted(path, errors.New("entry belongs to another key"))
		}
		out[plain.Key] = plain.Value
	}
	return out, nil
}

// readEntry reads and decrypts an entry file. A missing file is returned
// as fs.ErrNotExist.
func (f *fileStore) readEntry(ns Namespace, path string) (*entryPlain, error) {
	raw, err := readLimited(path, maxEntryFileSize)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("iam: file store: read %s: %w", path, err)
	}
	var env entryFile
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, corrupted(path, err)
	}
	if env.Format != entryFileFormat {
		return nil, corrupted(path, fmt.Errorf("unknown format %q", env.Format))
	}
	if env.Version != fileFormatVersion {
		return nil, corrupted(path, fmt.Errorf("unsupported version %d", env.Version))
	}
	plainRaw, err := open(f.key, env.Nonce, env.Data, entryAAD(ns))
	if err != nil {
		return nil, corrupted(path, err)
	}
	var plain entryPlain
	if err := json.Unmarshal(plainRaw, &plain); err != nil {
		return nil, corrupted(path, err)
	}
	return &plain, nil
}

// readLimited reads a whole file of at most limit bytes.
func readLimited(path string, limit int64) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	data, err := io.ReadAll(io.LimitReader(fh, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, corrupted(path, fmt.Errorf("file exceeds %d bytes", limit))
	}
	return data, nil
}

// writeTemp writes data to a new synced temporary file in dir and returns
// its path.
func writeTemp(dir string, data []byte) (string, error) {
	fh, err := os.CreateTemp(dir, tempFilePrefix+"*")
	if err != nil {
		return "", fmt.Errorf("iam: file store: create temporary file: %w", err)
	}
	name := fh.Name()
	if err := fh.Chmod(fileStoreFilePerm); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		_ = fh.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("iam: file store: chmod %s: %w", name, err)
	}
	if _, err := fh.Write(data); err != nil {
		_ = fh.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("iam: file store: write %s: %w", name, err)
	}
	if err := fh.Sync(); err != nil {
		_ = fh.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("iam: file store: sync %s: %w", name, err)
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("iam: file store: close %s: %w", name, err)
	}
	return name, nil
}

// syncDir makes a rename durable; failures (e.g. on Windows) are ignored.
func syncDir(dir string) {
	fh, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = fh.Sync()
	_ = fh.Close()
}
