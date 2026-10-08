package object

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// TestSetPlacementIsUniform is TS-U-15: SipHash-2-4 spreads keys evenly
// over the sets (chi-square test at p = 0.001 over 50k keys).
func TestSetPlacementIsUniform(t *testing.T) {
	// Critical values of the chi-square distribution at p = 0.001 by
	// degrees of freedom (sets - 1).
	critical := map[int]float64{1: 10.83, 2: 13.82, 3: 16.27, 7: 24.32, 15: 37.70, 31: 61.10}
	const keys = 50000
	for _, sets := range []int{2, 3, 4, 8, 16, 32} {
		for _, dep := range []string{"deployment-a", "deployment-b"} {
			t.Run(fmt.Sprintf("%d sets/%s", sets, dep), func(t *testing.T) {
				k0, k1 := sipKeys(dep)
				counts := make([]int, sets)
				for i := 0; i < keys; i++ {
					counts[setIndex(k0, k1, fmt.Sprintf("bucket/photos/%d/img-%06d.jpg", i%97, i), sets)]++
				}
				expected := float64(keys) / float64(sets)
				var chi2 float64
				for _, c := range counts {
					d := float64(c) - expected
					chi2 += d * d / expected
				}
				assert.Less(t, chi2, critical[sets-1], "counts %v", counts)
			})
		}
	}
}

// TestPlacementIsStable pins the set selection and the shard distribution:
// they are part of the on-disk layout and must never change between
// versions (an existing object would become unreachable).
func TestPlacementIsStable(t *testing.T) {
	k0, k1 := sipKeys("deployment-test")
	tests := []struct {
		key                 string
		sets4, sets16, set7 int
		dist4               []int
		dist6               []int
	}{
		{key: "alpha", sets4: 3, sets16: 3, set7: 2, dist4: []int{3, 4, 1, 2}, dist6: []int{5, 6, 1, 2, 3, 4}},
		{key: "beta", sets4: 0, sets16: 12, set7: 5, dist4: []int{4, 1, 2, 3}, dist6: []int{2, 3, 4, 5, 6, 1}},
		{key: "photos/2024/a.jpg", sets4: 2, sets16: 10, set7: 1, dist4: []int{3, 4, 1, 2}, dist6: []int{5, 6, 1, 2, 3, 4}},
		{key: "", sets4: 0, sets16: 8, set7: 5, dist4: []int{1, 2, 3, 4}, dist6: []int{1, 2, 3, 4, 5, 6}},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.sets4, setIndex(k0, k1, tt.key, 4), "%q over 4 sets", tt.key)
		assert.Equal(t, tt.sets16, setIndex(k0, k1, tt.key, 16), "%q over 16 sets", tt.key)
		assert.Equal(t, tt.set7, setIndex(k0, k1, tt.key, 7), "%q over 7 sets", tt.key)
		assert.Equal(t, tt.dist4, hashOrder(tt.key, 4), "%q distribution over 4 drives", tt.key)
		assert.Equal(t, tt.dist6, hashOrder(tt.key, 6), "%q distribution over 6 drives", tt.key)
	}
	assert.Equal(t, 0, setIndex(k0, k1, "anything", 1))
	other0, other1 := sipKeys("another-deployment")
	differs := 0
	for i := 0; i < 100; i++ {
		k := fmt.Sprintf("k%d", i)
		if setIndex(k0, k1, k, 16) != setIndex(other0, other1, k, 16) {
			differs++
		}
	}
	assert.Greater(t, differs, 50, "the placement depends on the deployment id")
	for n := 1; n <= 16; n++ {
		d := hashOrder("some/key", n)
		sorted := slices.Clone(d)
		slices.Sort(sorted)
		for i, v := range sorted {
			require.Equal(t, i+1, v, "hashOrder over %d drives is a permutation", n)
		}
	}
	assert.Nil(t, hashOrder("k", 0))
}

func TestQuorums(t *testing.T) {
	tests := []struct{ data, parity, read, write int }{
		{data: 2, parity: 2, read: 2, write: 3},
		{data: 1, parity: 1, read: 1, write: 2},
		{data: 3, parity: 1, read: 3, write: 3},
		{data: 4, parity: 4, read: 4, write: 5},
		{data: 12, parity: 4, read: 12, write: 12},
	}
	for _, tt := range tests {
		r, w := quorums(tt.data, tt.parity)
		assert.Equal(t, tt.read, r, "%d+%d", tt.data, tt.parity)
		assert.Equal(t, tt.write, w, "%d+%d", tt.data, tt.parity)
	}
}

// version builds the FileInfo of a version on a 2+2 set.
func version(id string, mod int64, dataDir string, size int64) storage.FileInfo {
	return storage.FileInfo{
		VersionID: id, ModTime: time.Unix(mod, 0).UTC(), DataDir: dataDir, Size: size,
		Metadata: map[string]string{metaETag: fmt.Sprintf("etag-%s-%d", id, mod)},
		Erasure:  storage.ErasureInfo{DataBlocks: 2, ParityBlocks: 2, BlockSize: 1024, Distribution: []int{1, 2, 3, 4}},
	}
}

func withMeta(fi storage.FileInfo, k, v string) storage.FileInfo {
	m := map[string]string{}
	for key, val := range fi.Metadata {
		m[key] = val
	}
	m[k] = v
	fi.Metadata = m
	return fi
}

func withIndex(fi storage.FileInfo, i int) storage.FileInfo {
	fi.Erasure.Index = i
	return fi
}

// TestPickValidFileInfo is TS-U-06: the version a read quorum of drives
// agree on is chosen when drives diverge (modification time, data
// directory, corrupt or missing metadata).
func TestPickValidFileInfo(t *testing.T) {
	cur := version("v2", 200, "d2", 10)
	old := version("v1", 100, "d1", 5)
	notFound := fmt.Errorf("%w: k", domain.ErrObjectNotFound)
	noVersion := fmt.Errorf("%w: v9", domain.ErrNoSuchVersion)
	offline := fmt.Errorf("%w: drive", domain.ErrDiskNotFound)
	corrupt := fmt.Errorf("%w: bad crc", storage.ErrFileCorrupt)
	rrs := cur
	rrs.Erasure.DataBlocks, rrs.Erasure.ParityBlocks = 3, 1
	marker := storage.FileInfo{VersionID: "m", ModTime: time.Unix(300, 0), Deleted: true, Metadata: map[string]string{}}

	tests := []struct {
		name     string
		infos    []storage.FileInfo
		errs     []error
		want     *storage.FileInfo
		wantErr  error
		agreeing []bool
	}{
		{
			name:  "all agree (per-drive index ignored)",
			infos: []storage.FileInfo{withIndex(cur, 1), withIndex(cur, 2), withIndex(cur, 3), withIndex(cur, 4)},
			errs:  []error{nil, nil, nil, nil}, want: &cur, agreeing: []bool{true, true, true, true},
		},
		{
			name:  "one drive missed the last write",
			infos: []storage.FileInfo{cur, old, cur, cur},
			errs:  []error{nil, nil, nil, nil}, want: &cur, agreeing: []bool{true, false, true, true},
		},
		{
			name:  "different data directory is another version",
			infos: []storage.FileInfo{cur, version("v2", 200, "dX", 10), cur, {}},
			errs:  []error{nil, nil, nil, offline}, want: &cur, agreeing: []bool{true, false, true, false},
		},
		{
			name:  "corrupt metadata on one drive",
			infos: []storage.FileInfo{{}, cur, cur, cur},
			errs:  []error{corrupt, nil, nil, nil}, want: &cur, agreeing: []bool{false, true, true, true},
		},
		{
			name:  "two against two: the newest",
			infos: []storage.FileInfo{old, cur, old, cur},
			errs:  []error{nil, nil, nil, nil}, want: &cur, agreeing: []bool{false, true, false, true},
		},
		{
			name:  "metadata updated on most drives",
			infos: []storage.FileInfo{withMeta(cur, metaTagging, "a=1"), withMeta(cur, metaTagging, "a=1"), cur, withMeta(cur, metaTagging, "a=1")},
			errs:  []error{nil, nil, nil, nil}, want: ptr(withMeta(cur, metaTagging, "a=1")), agreeing: []bool{true, true, true, true},
		},
		{
			name:  "delete marker with the default quorum",
			infos: []storage.FileInfo{marker, marker, {}, {}},
			errs:  []error{nil, nil, offline, offline}, want: &marker, agreeing: []bool{true, true, false, false},
		},
		{
			name:    "missing on a quorum of drives",
			infos:   make([]storage.FileInfo, 4),
			errs:    []error{notFound, notFound, notFound, offline},
			wantErr: domain.ErrObjectNotFound,
		},
		{
			name:    "version missing on a quorum of drives",
			infos:   []storage.FileInfo{{}, {}, {}, {}},
			errs:    []error{noVersion, notFound, noVersion, offline},
			wantErr: domain.ErrNoSuchVersion,
		},
		{
			name:    "one copy, one miss, two offline: undecidable",
			infos:   []storage.FileInfo{cur, {}, {}, {}},
			errs:    []error{nil, notFound, offline, offline},
			wantErr: domain.ErrReadQuorum,
		},
		{
			name:    "every drive offline",
			infos:   make([]storage.FileInfo, 4),
			errs:    []error{offline, offline, offline, offline},
			wantErr: domain.ErrReadQuorum,
		},
		{
			name:    "a 3+1 version needs three drives",
			infos:   []storage.FileInfo{rrs, rrs, {}, {}},
			errs:    []error{nil, nil, offline, offline},
			wantErr: domain.ErrReadQuorum,
		},
		{
			name:    "three divergent versions",
			infos:   []storage.FileInfo{cur, old, version("v3", 300, "d3", 1), {}},
			errs:    []error{nil, nil, nil, offline},
			wantErr: domain.ErrReadQuorum,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, agreeing, err := pickValidFileInfo(tt.infos, tt.errs, 2)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, versionKey(*tt.want), versionKey(got))
			assert.Equal(t, tt.want.Metadata, got.Metadata)
			assert.Equal(t, tt.agreeing, agreeing)
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestMergeVersions(t *testing.T) {
	v1, v2, v3 := version("v1", 100, "d1", 1), version("v2", 200, "d2", 2), version("v3", 300, "d3", 3)
	tests := []struct {
		name     string
		perDrive [][]storage.FileInfo
		want     []string
	}{
		{name: "agreement", perDrive: [][]storage.FileInfo{{v3, v2, v1}, {v3, v2, v1}, {v3, v2, v1}}, want: []string{"v3", "v2", "v1"}},
		{name: "newest version on one drive only", perDrive: [][]storage.FileInfo{{v3, v2, v1}, {v2, v1}, {v2, v1}}, want: []string{"v2", "v1"}},
		{name: "old version purged on most drives", perDrive: [][]storage.FileInfo{{v3, v2}, {v3, v2, v1}, {v3, v2}}, want: []string{"v3", "v2"}},
		{name: "nothing agreed", perDrive: [][]storage.FileInfo{{v3}, {v2}, {v1}}, want: nil},
		{
			name:     "a null version being replaced is listed once",
			perDrive: [][]storage.FileInfo{{version("", 300, "dn", 3)}, {version("", 300, "dn", 3)}, {version("", 100, "do", 1)}, {version("", 100, "do", 1)}},
			want:     []string{""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeVersions(tt.perDrive, 2)
			var ids []string
			for i, fi := range got {
				ids = append(ids, fi.VersionID)
				assert.Equal(t, i == 0, fi.IsLatest)
				assert.Equal(t, len(got), fi.NumVersions)
			}
			assert.Equal(t, tt.want, ids)
		})
	}
	// A single drive (fs, or a 1+1 set) is trusted as is.
	assert.Equal(t, []storage.FileInfo{v1}, mergeVersions([][]storage.FileInfo{{v1}}, 1))
}

func TestReduceErrs(t *testing.T) {
	offline := fmt.Errorf("%w: d", domain.ErrDiskNotFound)
	err := reduceErrs([]error{offline, offline, offline, nil}, 2, domain.ErrReadQuorum)
	assert.ErrorIs(t, err, domain.ErrReadQuorum, "offline drives agreeing mean a lost quorum")
	assert.ErrorIs(t, err, domain.ErrDiskNotFound)
	full := fmt.Errorf("%w: d", domain.ErrStorageFull)
	assert.ErrorIs(t, reduceErrs([]error{full, full, full, nil}, 3, domain.ErrWriteQuorum), domain.ErrStorageFull)
	// Ties are broken by the first error met.
	err = reduceErrs([]error{domain.ErrBucketNotFound, offline, domain.ErrBucketNotFound, offline}, 2, nil)
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
}

func TestStorageClasses(t *testing.T) {
	tests := []struct {
		name             string
		size, poolParity int
		classes          map[string]string
		want             storageClasses
		wantErr          bool
	}{
		{name: "defaults of 4 drives", size: 4, want: storageClasses{standard: 2, rrs: 1}},
		{name: "defaults of 6 drives", size: 6, want: storageClasses{standard: 3, rrs: 1}},
		{name: "defaults of 16 drives", size: 16, want: storageClasses{standard: 4, rrs: 1}},
		{name: "defaults of 2 drives", size: 2, want: storageClasses{standard: 1, rrs: 1}},
		{name: "pool parity", size: 8, poolParity: 2, want: storageClasses{standard: 2, rrs: 1}},
		{name: "configured", size: 8, classes: map[string]string{domain.StorageClassStandard: "EC:3", domain.StorageClassReducedRedundancy: "ec:2"}, want: storageClasses{standard: 3, rrs: 2}},
		{name: "standard too high", size: 4, classes: map[string]string{domain.StorageClassStandard: "EC:3"}, wantErr: true},
		{name: "standard zero", size: 4, classes: map[string]string{domain.StorageClassStandard: "EC:0"}, wantErr: true},
		{name: "rrs above standard", size: 8, classes: map[string]string{domain.StorageClassReducedRedundancy: "EC:5"}, wantErr: true},
		{name: "malformed", size: 4, classes: map[string]string{domain.StorageClassStandard: "EC2"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveStorageClasses(tt.size, tt.poolParity, options{storageClasses: tt.classes})
			if tt.wantErr {
				assert.ErrorIs(t, err, domain.ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.want.rrs, got.parity("reduced_redundancy"))
			assert.Equal(t, tt.want.standard, got.parity(""))
			assert.Equal(t, tt.want.standard, got.parity("GLACIER"))
		})
	}
	for _, bad := range []string{"", "EC:", "EC:-1", "XX:2", "EC:two"} {
		_, err := ParseStorageClass(bad)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, bad)
	}
	n, err := ParseStorageClass(" EC:4 ")
	require.NoError(t, err)
	assert.Equal(t, 4, n)
}

// TestPickPool checks the weighted choice of the pool of a new object
// with a deterministic random source.
func TestPickPool(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	newPool := func(idx int, total, free uint64, decom bool) *pool {
		return &pool{idx: idx, decommissioning: decom, space: poolSpace{total: total, free: free}, spaceAt: now, hasSpace: true}
	}
	tests := []struct {
		name    string
		pools   []*pool
		draw    uint64
		size    int64
		want    int
		wantErr error
	}{
		{name: "first slot", pools: []*pool{newPool(0, 100, 10, false), newPool(1, 100, 30, false)}, draw: 9, want: 0},
		{name: "second slot", pools: []*pool{newPool(0, 100, 10, false), newPool(1, 100, 30, false)}, draw: 10, want: 1},
		{name: "full pool skipped", pools: []*pool{newPool(0, 100, 4, false), newPool(1, 100, 30, false)}, draw: 0, want: 1},
		{name: "decommissioning skipped", pools: []*pool{newPool(0, 100, 90, true), newPool(1, 100, 30, false)}, draw: 0, want: 1},
		{name: "object larger than the free space", pools: []*pool{newPool(0, 1000, 100, false), newPool(1, 1000, 900, false)}, size: 500, draw: 0, want: 1},
		{name: "unknown capacity", pools: []*pool{newPool(0, 0, 0, false), newPool(1, 0, 0, false)}, wantErr: domain.ErrStorageFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var drawn uint64
			p := &poolPlacement{pools: tt.pools, spaceTTL: time.Hour, now: func() time.Time { return now },
				randN: func(n uint64) uint64 { drawn = n; return tt.draw }}
			got, err := p.pickPool(context.Background(), tt.size)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.NotZero(t, drawn)
		})
	}
	assert.True(t, errors.Is(fmt.Errorf("x: %w", domain.ErrStorageFull), domain.ErrStorageFull))
}
