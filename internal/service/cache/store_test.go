package cache_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
)

// diskFiles lists the entry files of the disk tier.
func diskFiles(t *testing.T, s cache.Service) []string {
	t.Helper()
	var out []string
	dir := cache.DiskDir(s)
	require.NotEmpty(t, dir)
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".nxc" {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// TestDomainQuota is TCC-13: a domain stays within its quota by evicting
// its least recently used entries; the other domains keep theirs.
//
// covers: S-DST-07
func TestDomainQuota(t *testing.T) {
	e := newEnv(t, envOptions{})
	const objSize = 10 << 10
	e.setDist(cache.DistributionConfig{Distribution: "web", Domains: []cache.DomainSettings{
		{Host: "www.example.com", Cache: cache.Settings{Quota: size(4 * (objSize + 1024))}},
	}})
	for i := range 10 {
		e.origin.put("web", "o"+strconv.Itoa(i), payload(objSize, byte(i)))
	}
	for i := range 10 {
		require.NoError(t, e.get("www.example.com", "/o"+strconv.Itoa(i)).err)
		require.NoError(t, e.get("example.com", "/o"+strconv.Itoa(i)).err)
	}
	used := cache.ScopeBytes(e.svc, "h:web|www.example.com")
	assert.LessOrEqual(t, used, int64(4*(objSize+1024)), "within the quota")
	assert.Positive(t, used)
	// The most recent ones stayed, the other domain kept everything.
	assert.True(t, e.cached("www.example.com", "/o9"))
	assert.False(t, e.cached("www.example.com", "/o0"))
	for i := range 10 {
		assert.True(t, e.cached("example.com", "/o"+strconv.Itoa(i)), "example.com/o%d", i)
	}
	// An object larger than the quota is served, not cached.
	e.origin.put("web", "huge", payload(8*objSize, 1))
	r := e.get("www.example.com", "/huge")
	require.NoError(t, r.err)
	assert.Len(t, r.body, 8*objSize)
	assert.False(t, e.cached("www.example.com", "/huge"))
}

// TestDistributionQuota bounds the whole distribution.
func TestDistributionQuota(t *testing.T) {
	e := newEnv(t, envOptions{})
	const objSize = 8 << 10
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{Quota: size(3 * (objSize + 1024))}})
	for i := range 6 {
		e.origin.put("web", "o"+strconv.Itoa(i), payload(objSize, byte(i)))
		require.NoError(t, e.get("www.example.com", "/o"+strconv.Itoa(i)).err)
		require.NoError(t, e.get("example.com", "/o"+strconv.Itoa(i)).err)
	}
	assert.LessOrEqual(t, cache.ScopeBytes(e.svc, "d:web"), int64(3*(objSize+1024)))
}

// TestMemoryTierEviction keeps the memory tier within its size
// (W-TinyLFU): nothing fails when it is full.
func TestMemoryTierEviction(t *testing.T) {
	e := newEnv(t, envOptions{opts: []cache.Option{cache.WithMemorySize(1 << 20)}})
	for i := range 64 {
		e.origin.put("web", "o"+strconv.Itoa(i), payload(64<<10, byte(i)))
	}
	for round := range 2 {
		for i := range 64 {
			r := e.get("www.example.com", "/o"+strconv.Itoa(i))
			require.NoError(t, r.err, "round %d", round)
			require.Len(t, r.body, 64<<10)
		}
	}
	st, err := e.svc.NodeStats(t.Context(), cache.StatsQuery{})
	require.NoError(t, err)
	assert.LessOrEqual(t, int64(st.MemorySize), int64(1<<20))
	assert.Positive(t, st.Evictions+int64(64-st.Entries))
}

// TestStoreIsCollected checks that a store nothing references any more is
// garbage collected with its memory tier: the deletion handler of the
// memory tier must not keep the store reachable from the cleanup otter
// attaches to the cache, or every cache built in a process (one per node
// started by the tests) leaks with everything it references.
func TestStoreIsCollected(t *testing.T) {
	ref, err := cache.NewMemoryStore(1<<20, 16)
	require.NoError(t, err)
	assert.Eventually(t, func() bool {
		runtime.GC()
		return ref.Value() == nil
	}, 5*time.Second, 10*time.Millisecond, "the store is still reachable")
}

// TestDiskTier checks the disk tier: entries written atomically, read
// back after the memory tier lost them, reloaded unverified after a
// restart (revalidated before they are served), the encrypted objects
// never written.
//
// covers: S-DST-07
func TestDiskTier(t *testing.T) {
	dir := t.TempDir()
	docs := &memDocs{docs: map[string][]byte{}}
	eo := envOptions{docs: docs, opts: []cache.Option{cache.WithDir(dir), cache.WithMemorySize(64 << 10)}, noStop: true}
	e := newEnv(t, eo)
	e.origin.put("web", "a.bin", payload(32<<10, 1))
	e.origin.put("web", "secret.bin", payload(1<<10, 2), func(i *domain.ObjectInfo) { i.Encryption = domain.EncryptionInfo{Type: domain.SSES3} })
	require.NoError(t, e.get("www.example.com", "/a.bin").err)
	require.NoError(t, e.get("www.example.com", "/secret.bin").err)
	cache.WaitDisk(e.svc)
	files := diskFiles(t, e.svc)
	require.Len(t, files, 1, "the encrypted object stays in memory")
	// Evict the memory tier: the entry comes back from the disk.
	for i := range 8 {
		e.origin.put("web", "fill"+strconv.Itoa(i), payload(16<<10, byte(i)))
		require.NoError(t, e.get("www.example.com", "/fill"+strconv.Itoa(i)).err)
	}
	reads := e.origin.gets.Load()
	r := e.get("www.example.com", "/a.bin")
	assert.Equal(t, "HIT", r.xcache())
	assert.Equal(t, payload(32<<10, 1), r.body)
	assert.Equal(t, reads, e.origin.gets.Load())
	cache.WaitDisk(e.svc)
	require.NoError(t, e.svc.Stop(t.Context()))

	// Restart on the same directory: the entries are revalidated.
	eo.origin, eo.clk, eo.dists = e.origin, e.clk, e.dists
	e2 := newEnv(t, eo)
	t.Cleanup(func() { _ = e2.svc.Stop(t.Context()) })
	require.Eventually(t, func() bool { return cache.Entries(e2.svc) > 0 }, timeout, tick)
	r = e2.get("www.example.com", "/a.bin")
	assert.Equal(t, "REVALIDATED", r.xcache())
	assert.Equal(t, payload(32<<10, 1), r.body)
	assert.Equal(t, "HIT", e2.get("www.example.com", "/a.bin").xcache())
	// Changed while the node was down: read again, never the old one.
	e2.origin.put("web", "fill0", payload(16<<10, 99))
	r = e2.get("www.example.com", "/fill0")
	assert.Equal(t, payload(16<<10, 99), r.body)
}

// TestDiskCorruption is TCC-14: a corrupt or missing file is dropped and
// the object served from the origin; a cache directory removed or made
// unwritable at runtime never fails a request.
//
// covers: S-DST-07
func TestDiskCorruption(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, envOptions{opts: []cache.Option{cache.WithDir(dir), cache.WithMemorySize(64 << 10)}})
	data := payload(40<<10, 5)
	e.origin.put("web", "big.bin", data)
	require.NoError(t, e.get("www.example.com", "/big.bin").err)
	cache.WaitDisk(e.svc)
	files := diskFiles(t, e.svc)
	require.Len(t, files, 1)
	// Flip a byte of the content.
	raw, err := os.ReadFile(files[0])
	require.NoError(t, err)
	raw[len(raw)-10] ^= 0xff
	require.NoError(t, os.WriteFile(files[0], raw, 0o600))
	reads := e.origin.gets.Load()
	r := e.get("www.example.com", "/big.bin")
	require.NoError(t, r.err)
	assert.Equal(t, data, r.body, "never the corrupt content")
	assert.Equal(t, "MISS", r.xcache())
	assert.Equal(t, reads+1, e.origin.gets.Load())
	cache.WaitDisk(e.svc)
	// The directory removed at runtime.
	require.NoError(t, os.RemoveAll(cache.DiskDir(e.svc)))
	r = e.get("www.example.com", "/big.bin")
	require.NoError(t, r.err)
	assert.Equal(t, data, r.body)
	cache.WaitDisk(e.svc)
	// The directory replaced with a file: the writes fail, the requests
	// do not.
	require.NoError(t, os.RemoveAll(cache.DiskDir(e.svc)))
	require.NoError(t, os.WriteFile(cache.DiskDir(e.svc), []byte("not a directory"), 0o600))
	e.origin.put("web", "other.bin", payload(40<<10, 6))
	for range 3 {
		r = e.get("www.example.com", "/other.bin")
		require.NoError(t, r.err)
		assert.Equal(t, payload(40<<10, 6), r.body)
	}
	cache.WaitDisk(e.svc)
}

// TestDiskQuota keeps the disk tier within --cache-size.
func TestDiskQuota(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, envOptions{opts: []cache.Option{cache.WithDir(dir), cache.WithDiskSize(100 << 10), cache.WithMemorySize(64 << 10)}})
	for i := range 20 {
		e.origin.put("web", "o"+strconv.Itoa(i), payload(20<<10, byte(i)))
		require.NoError(t, e.get("www.example.com", "/o"+strconv.Itoa(i)).err)
		cache.WaitDisk(e.svc)
	}
	var total int64
	for _, f := range diskFiles(t, e.svc) {
		st, err := os.Stat(f)
		require.NoError(t, err)
		total += st.Size()
	}
	assert.LessOrEqual(t, total, int64(100<<10))
	st, err := e.svc.NodeStats(t.Context(), cache.StatsQuery{})
	require.NoError(t, err)
	assert.Equal(t, cache.Size(total), st.DiskSize)
}
