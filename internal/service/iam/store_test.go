package iam_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/testutil"
)

func newFileStore(t *testing.T, dir, secret string) iam.Store {
	t.Helper()
	s, err := iam.NewFileStore(context.Background(), dir, secret, iam.WithFileKDFParams(testKDF()))
	require.NoError(t, err)
	return s
}

// storeFactories are the Store implementations under the same contract.
func storeFactories() map[string]func(t *testing.T) iam.Store {
	return map[string]func(t *testing.T) iam.Store{
		"memory": func(*testing.T) iam.Store { return iam.NewMemoryStore() },
		"file":   func(t *testing.T) iam.Store { return newFileStore(t, t.TempDir(), rootSK) },
		"docs":   func(t *testing.T) iam.Store { return newDocsStore(t, newDocs(t, 4), rootSK) },
	}
}

// newDocs returns a system document store over n temporary drives.
func newDocs(t *testing.T, n int) sysstore.Store {
	t.Helper()
	drives := make([]storage.StorageAPI, n)
	for i := range drives {
		drives[i] = testutil.NewLocalDriveIndex(t, "", i)
	}
	docs, err := sysstore.New(sysstore.Deps{Drives: drives, Locks: lock.NewLocal(lock.Deps{})})
	require.NoError(t, err)
	return docs
}

func newDocsStore(t *testing.T, docs sysstore.Store, secret string) iam.Store {
	t.Helper()
	s, err := iam.NewDocsStore(context.Background(), docs, secret, iam.WithFileKDFParams(testKDF()))
	require.NoError(t, err)
	return s
}

// TestDocsStoreSharedByNodes: the stores of several nodes over the same
// documents agree on the key; another root secret is refused.
func TestDocsStoreSharedByNodes(t *testing.T) {
	ctx := context.Background()
	docs := newDocs(t, 4)
	a := newDocsStore(t, docs, rootSK)
	b := newDocsStore(t, docs, rootSK)
	require.NoError(t, a.Put(ctx, iam.NamespaceUsers, "alice", []byte(`{"v":1}`)))
	v, err := b.Get(ctx, iam.NamespaceUsers, "alice")
	require.NoError(t, err)
	assert.JSONEq(t, `{"v":1}`, string(v))

	_, err = iam.NewDocsStore(ctx, docs, "another-root-secret", iam.WithFileKDFParams(testKDF()))
	require.ErrorIs(t, err, iam.ErrWrongKey)
	_, err = iam.NewDocsStore(ctx, nil, rootSK)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = iam.NewDocsStore(ctx, docs, "")
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestStoreContract(t *testing.T) {
	ctx := context.Background()
	for name, factory := range storeFactories() {
		t.Run(name, func(t *testing.T) {
			s := factory(t)

			_, err := s.Get(ctx, iam.NamespaceUsers, "alice")
			assert.ErrorIs(t, err, iam.ErrEntryNotFound)
			assert.ErrorIs(t, err, domain.ErrObjectNotFound)

			list, err := s.List(ctx, iam.NamespaceUsers)
			require.NoError(t, err)
			assert.Empty(t, list)

			require.NoError(t, s.Put(ctx, iam.NamespaceUsers, "alice", []byte(`{"v":1}`)))
			require.NoError(t, s.Put(ctx, iam.NamespaceUsers, "bob/with:odd chars", []byte(`{"v":2}`)))
			require.NoError(t, s.Put(ctx, iam.NamespaceGroups, "alice", []byte(`{"g":1}`)))
			require.NoError(t, s.Put(ctx, iam.NamespaceUsers, "alice", []byte(`{"v":3}`)), "put replaces")

			v, err := s.Get(ctx, iam.NamespaceUsers, "alice")
			require.NoError(t, err)
			assert.JSONEq(t, `{"v":3}`, string(v))
			v[0] = 'X'
			v2, _ := s.Get(ctx, iam.NamespaceUsers, "alice")
			assert.JSONEq(t, `{"v":3}`, string(v2), "returned values are copies")

			list, err = s.List(ctx, iam.NamespaceUsers)
			require.NoError(t, err)
			assert.Equal(t, map[string][]byte{"alice": []byte(`{"v":3}`), "bob/with:odd chars": []byte(`{"v":2}`)}, list)

			require.NoError(t, s.Delete(ctx, iam.NamespaceUsers, "alice"))
			require.NoError(t, s.Delete(ctx, iam.NamespaceUsers, "alice"), "deleting a missing key is not an error")
			_, err = s.Get(ctx, iam.NamespaceUsers, "alice")
			assert.ErrorIs(t, err, iam.ErrEntryNotFound)
			g, err := s.Get(ctx, iam.NamespaceGroups, "alice")
			require.NoError(t, err, "namespaces are independent")
			assert.JSONEq(t, `{"g":1}`, string(g))
		})
	}
}

func TestStoreValidation(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		ns   iam.Namespace
		key  string
	}{
		{"empty namespace", "", "k"},
		{"uppercase namespace", "Users", "k"},
		{"namespace with slash", "a/b", "k"},
		{"namespace too long", iam.Namespace(strings.Repeat("a", 65)), "k"},
		{"empty key", iam.NamespaceUsers, ""},
		{"key too long", iam.NamespaceUsers, strings.Repeat("k", 1025)},
		{"key with NUL", iam.NamespaceUsers, "a\x00b"},
		{"key not UTF-8", iam.NamespaceUsers, "\xff"},
	}
	for name, factory := range storeFactories() {
		s := factory(t)
		for _, tt := range tests {
			t.Run(name+"/"+tt.name, func(t *testing.T) {
				_, err := s.Get(ctx, tt.ns, tt.key)
				assert.ErrorIs(t, err, domain.ErrInvalidArgument)
				assert.ErrorIs(t, s.Put(ctx, tt.ns, tt.key, []byte("x")), domain.ErrInvalidArgument)
				assert.ErrorIs(t, s.Delete(ctx, tt.ns, tt.key), domain.ErrInvalidArgument)
				if tt.key != "" && !strings.HasPrefix(tt.name, "key") {
					_, err := s.List(ctx, tt.ns)
					assert.ErrorIs(t, err, domain.ErrInvalidArgument)
				}
			})
		}
	}
}

func TestStoreConcurrency(t *testing.T) {
	ctx := context.Background()
	for name, factory := range storeFactories() {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			var wg sync.WaitGroup
			for i := range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					key := fmt.Sprintf("k%d", i)
					for j := range 5 {
						assert.NoError(t, s.Put(ctx, iam.NamespaceSTS, key, []byte(fmt.Sprint(j))))
						_, err := s.Get(ctx, iam.NamespaceSTS, key)
						assert.NoError(t, err)
						_, err = s.List(ctx, iam.NamespaceSTS)
						assert.NoError(t, err)
					}
				}()
			}
			wg.Wait()
			list, err := s.List(ctx, iam.NamespaceSTS)
			require.NoError(t, err)
			assert.Len(t, list, 16)
			assert.Equal(t, []byte("4"), list["k3"])
		})
	}
}

func TestFileStoreEncryptedRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := newFileStore(t, dir, rootSK)
	secret := []byte(`{"secretKey":"very-secret-value"}`)
	require.NoError(t, s.Put(ctx, iam.NamespaceUsers, "alice", secret))

	// Nothing on disk reveals the key or the value; the salt is in clear.
	var files []string
	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		files = append(files, path)
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.False(t, bytes.Contains(raw, []byte("very-secret-value")), path)
		assert.False(t, bytes.Contains(raw, []byte("alice")), path)
		info, err := d.Info()
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), path)
		return nil
	}))
	require.Len(t, files, 2, "kdf.json and one entry, no temporary files")
	var kdf map[string]any
	raw, err := os.ReadFile(filepath.Join(dir, "kdf.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &kdf))
	assert.Equal(t, "argon2id", kdf["algorithm"])
	assert.EqualValues(t, 1, kdf["version"])
	assert.NotEmpty(t, kdf["salt"])

	// Reopening with the same secret reads the data back.
	again := newFileStore(t, dir, rootSK)
	v, err := again.Get(ctx, iam.NamespaceUsers, "alice")
	require.NoError(t, err)
	assert.Equal(t, secret, v)
	list, err := again.List(ctx, iam.NamespaceUsers)
	require.NoError(t, err)
	assert.Equal(t, map[string][]byte{"alice": secret}, list)
}

func TestFileStoreWrongKey(t *testing.T) {
	dir := t.TempDir()
	s := newFileStore(t, dir, rootSK)
	require.NoError(t, s.Put(context.Background(), iam.NamespaceUsers, "alice", []byte("{}")))

	_, err := iam.NewFileStore(context.Background(), dir, "another-root-secret", iam.WithFileKDFParams(testKDF()))
	assert.ErrorIs(t, err, iam.ErrWrongKey)
	assert.ErrorIs(t, err, domain.ErrFormatMismatch)
	assert.Contains(t, err.Error(), "root credentials")
}

func TestFileStoreCorruption(t *testing.T) {
	ctx := context.Background()
	entryPath := func(t *testing.T, dir string) string {
		matches, err := filepath.Glob(filepath.Join(dir, "users", "*.json"))
		require.NoError(t, err)
		require.Len(t, matches, 1)
		return matches[0]
	}
	rewrite := func(t *testing.T, path string, change func(m map[string]any)) {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		change(m)
		raw, err = json.Marshal(m)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, raw, 0o600))
	}
	tests := []struct {
		name    string
		corrupt func(t *testing.T, dir string)
	}{
		{"not JSON", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(entryPath(t, dir), []byte("garbage"), 0o600))
		}},
		{"truncated", func(t *testing.T, dir string) {
			p := entryPath(t, dir)
			raw, err := os.ReadFile(p)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(p, raw[:len(raw)/2], 0o600))
		}},
		{"unknown format", func(t *testing.T, dir string) {
			rewrite(t, entryPath(t, dir), func(m map[string]any) { m["format"] = "other" })
		}},
		{"future version", func(t *testing.T, dir string) {
			rewrite(t, entryPath(t, dir), func(m map[string]any) { m["version"] = 2 })
		}},
		{"tampered ciphertext", func(t *testing.T, dir string) {
			rewrite(t, entryPath(t, dir), func(m map[string]any) {
				data := []byte(m["data"].(string))
				data[2] ^= 0x01
				m["data"] = string(data)
			})
		}},
		{"bad nonce", func(t *testing.T, dir string) {
			rewrite(t, entryPath(t, dir), func(m map[string]any) { m["nonce"] = "AAAA" })
		}},
		{"moved to another key", func(t *testing.T, dir string) {
			p := entryPath(t, dir)
			require.NoError(t, os.Rename(p, filepath.Join(filepath.Dir(p), strings.Repeat("0", 64)+".json")))
		}},
		{"moved to another namespace", func(t *testing.T, dir string) {
			p := entryPath(t, dir)
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "groups"), 0o700))
			raw, err := os.ReadFile(p)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(p, []byte("{}"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "groups", filepath.Base(p)), raw, 0o600))
		}},
		{"oversized", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(entryPath(t, dir), bytes.Repeat([]byte(" "), 5<<20), 0o600))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			s := newFileStore(t, dir, rootSK)
			require.NoError(t, s.Put(ctx, iam.NamespaceUsers, "alice", []byte(`{"v":1}`)))
			tt.corrupt(t, dir)

			_, err := s.List(ctx, iam.NamespaceUsers)
			if tt.name == "moved to another namespace" {
				_, err = s.List(ctx, iam.NamespaceGroups)
			}
			assert.ErrorIs(t, err, domain.ErrInvalidFormat)
			assert.Contains(t, err.Error(), "corrupted")
			if tt.name != "moved to another key" && tt.name != "moved to another namespace" {
				_, err = s.Get(ctx, iam.NamespaceUsers, "alice")
				assert.ErrorIs(t, err, domain.ErrInvalidFormat)
			}
		})
	}
}

func TestFileStoreCorruptedKDF(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"not JSON", "{"},
		{"unknown format", `{"format":"x","version":1,"algorithm":"argon2id"}`},
		{"unknown algorithm", `{"format":"nexo-iam-kdf","version":1,"algorithm":"scrypt"}`},
		{"future version", `{"format":"nexo-iam-kdf","version":9,"algorithm":"argon2id"}`},
		{"short salt", `{"format":"nexo-iam-kdf","version":1,"algorithm":"argon2id","salt":"AAAA","time":1,"memoryKiB":64,"threads":1}`},
		{"absurd memory", `{"format":"nexo-iam-kdf","version":1,"algorithm":"argon2id","salt":"AAAAAAAAAAAAAAAAAAAAAA==","time":1,"memoryKiB":4294967295,"threads":1}`},
		{"bad check", `{"format":"nexo-iam-kdf","version":1,"algorithm":"argon2id","salt":"AAAAAAAAAAAAAAAAAAAAAA==","time":1,"memoryKiB":64,"threads":1,"checkNonce":"AAAA","check":"AAAA"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "kdf.json"), []byte(tt.content), 0o600))
			_, err := iam.NewFileStore(context.Background(), dir, rootSK)
			assert.ErrorIs(t, err, domain.ErrInvalidFormat)
		})
	}
}

func TestNewFileStoreArguments(t *testing.T) {
	ctx := context.Background()
	_, err := iam.NewFileStore(ctx, "", rootSK)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = iam.NewFileStore(ctx, t.TempDir(), "")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = iam.NewFileStore(ctx, t.TempDir(), rootSK, iam.WithFileKDFParams(iam.KDFParams{Time: 99, MemoryKiB: 64, Threads: 1}))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = iam.NewFileStore(ctx, t.TempDir(), rootSK, iam.WithFileKDFParams(iam.KDFParams{Time: 1, MemoryKiB: 4, Threads: 1}))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = iam.NewFileStore(ctx, t.TempDir(), rootSK, iam.WithFileKDFParams(iam.KDFParams{Time: 1, MemoryKiB: 64, Threads: 0}))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	// The directory cannot be created below a file.
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	_, err = iam.NewFileStore(ctx, filepath.Join(file, "iam"), rootSK, iam.WithFileKDFParams(testKDF()))
	assert.Error(t, err)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = iam.NewFileStore(canceled, t.TempDir(), rootSK, iam.WithFileKDFParams(testKDF()))
	assert.ErrorIs(t, err, context.Canceled)
	s := newFileStore(t, t.TempDir(), rootSK)
	_, err = s.Get(canceled, iam.NamespaceUsers, "a")
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, s.Put(canceled, iam.NamespaceUsers, "a", nil), context.Canceled)
	assert.ErrorIs(t, s.Delete(canceled, iam.NamespaceUsers, "a"), context.Canceled)
	_, err = s.List(canceled, iam.NamespaceUsers)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestFileStoreIgnoresForeignFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := newFileStore(t, dir, rootSK)
	require.NoError(t, s.Put(ctx, iam.NamespaceUsers, "alice", []byte("1")))
	// Leftovers of an interrupted write, sub-directories and other files.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "users", ".tmp-123"), []byte("partial"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "users", "README.txt"), []byte("hi"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "users", "sub.json"), 0o700))
	list, err := s.List(ctx, iam.NamespaceUsers)
	require.NoError(t, err)
	assert.Equal(t, map[string][]byte{"alice": []byte("1")}, list)
}

func TestFileStoreDefaultKDF(t *testing.T) {
	if testing.Short() {
		t.Skip("default Argon2id parameters use 64 MiB")
	}
	dir := t.TempDir()
	s, err := iam.NewFileStore(context.Background(), dir, rootSK)
	require.NoError(t, err)
	require.NoError(t, s.Put(context.Background(), iam.NamespaceUsers, "a", []byte("1")))
	raw, err := os.ReadFile(filepath.Join(dir, "kdf.json"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"memoryKiB": 65536`)
}
