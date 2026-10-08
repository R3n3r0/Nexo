package object

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sync"
	"time"

	"github.com/dchest/siphash"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// placement decides which store (erasure set) holds a key: the set of the
// key in its pool (SipHash of the key, docs/03 §3.2), the pool of the
// key's existing versions or, for a new key, a pool chosen by free space.
// With a single pool no lookup is needed: every method answers without
// I/O.
type placement interface {
	// home returns the store of key in the first pool (no I/O): the store
	// asked about buckets and missing keys.
	home(key string) store
	// forRead returns the store holding the latest version of
	// bucket/key, the newest across pools, or home(key) when no pool has it.
	forRead(ctx context.Context, bucket, key string) (store, error)
	// forVersion returns the store holding version versionID of
	// bucket/key ("" is the latest version), or home(key).
	forVersion(ctx context.Context, bucket, key, versionID string) (store, error)
	// forWrite returns the store a new version of bucket/key of size
	// bytes (-1 when unknown) goes to: the pool of the existing key, or a
	// pool picked at random weighted by its free space.
	forWrite(ctx context.Context, bucket, key string, size int64) (store, error)
	// forUpload returns the store holding the multipart upload uploadID
	// of bucket/key: the uploads stay in the pool chosen at their creation.
	forUpload(ctx context.Context, bucket, key, uploadID string) (store, error)
	// others returns the stores of key in the pools other than the one of
	// st (none with a single pool).
	others(st store, key string) []store
}

// fixedPlacement is the placement of a single store (fs).
type fixedPlacement struct{ st store }

var _ placement = fixedPlacement{}

func (f fixedPlacement) home(string) store { return f.st }

func (f fixedPlacement) forRead(context.Context, string, string) (store, error) { return f.st, nil }

func (f fixedPlacement) forVersion(context.Context, string, string, string) (store, error) {
	return f.st, nil
}

func (f fixedPlacement) forWrite(context.Context, string, string, int64) (store, error) {
	return f.st, nil
}

func (f fixedPlacement) forUpload(context.Context, string, string, string) (store, error) {
	return f.st, nil
}

func (f fixedPlacement) others(store, string) []store { return nil }

// sipKeys derives the SipHash-2-4 keys of the set selection from the
// deployment id: a key maps to the same set on every node and across
// restarts, and differently in another deployment.
func sipKeys(deploymentID string) (uint64, uint64) {
	sum := sha256.Sum256([]byte(deploymentID))
	return binary.LittleEndian.Uint64(sum[:8]), binary.LittleEndian.Uint64(sum[8:16])
}

// setIndex returns the erasure set of key among n sets:
// SipHash-2-4(key, deploymentID) mod n. It is part of the on-disk layout:
// changing it makes existing objects unreachable.
func setIndex(k0, k1 uint64, key string, n int) int {
	if n <= 1 {
		return 0
	}
	return int(siphash.Hash(k0, k1, []byte(key)) % uint64(n))
}

// hashOrder returns the distribution of the shards of key over n drives:
// a rotation of 1..n starting at crc32(key) mod n. distribution[i] is the
// 1-based shard index stored on drive i, so that data and parity shards of
// different objects land on different drives.
func hashOrder(key string, n int) []int {
	if n <= 0 {
		return nil
	}
	start := int(crc32.ChecksumIEEE([]byte(key)) % uint32(n))
	out := make([]int, n)
	for i := range out {
		out[i] = 1 + (start+i)%n
	}
	return out
}

// pool is one server pool: its erasure sets and the state of its space.
type pool struct {
	idx             int
	sets            []*erasureSet
	k0, k1          uint64
	decommissioning bool

	mu       sync.Mutex
	space    poolSpace
	spaceAt  time.Time
	hasSpace bool
}

// poolSpace is the capacity of the online drives of a pool.
type poolSpace struct {
	total, free uint64
}

// usedRatio is the used fraction of the pool (1 when unknown).
func (s poolSpace) usedRatio() float64 {
	if s.total == 0 {
		return 1
	}
	return 1 - float64(s.free)/float64(s.total)
}

func (p *pool) setFor(key string) *erasureSet { return p.sets[setIndex(p.k0, p.k1, key, len(p.sets))] }

// poolPlacement places keys over one or more pools of erasure sets.
type poolPlacement struct {
	pools []*pool
	// spaceTTL bounds the age of the cached pool capacity.
	spaceTTL time.Duration
	now      func() time.Time
	// randN returns a uniform random number in [0, n).
	randN func(n uint64) uint64
}

var _ placement = (*poolPlacement)(nil)

func (p *poolPlacement) home(key string) store { return p.pools[0].setFor(key) }

func (p *poolPlacement) single() bool { return len(p.pools) == 1 }

// lookup reads version versionID ("" the latest) of bucket/key in every
// pool and returns, per pool, the version found (ok) or the error.
func (p *poolPlacement) lookup(ctx context.Context, bucket, key, versionID string) ([]storage.FileInfo, []error) {
	infos := make([]storage.FileInfo, len(p.pools))
	errs := parallel(len(p.pools), func(i int) error {
		fi, err := p.pools[i].setFor(key).ReadVersion(ctx, bucket, key, versionID, false)
		infos[i] = fi
		return err
	})
	return infos, errs
}

// absent reports the errors that mean "not in this pool".
func absent(err error) bool {
	return isNotFound(err) || errors.Is(err, domain.ErrNoSuchVersion) || errors.Is(err, domain.ErrBucketNotFound)
}

func (p *poolPlacement) forRead(ctx context.Context, bucket, key string) (store, error) {
	if p.single() {
		return p.home(key), nil
	}
	idx, err := p.existing(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	if idx < 0 {
		return p.home(key), nil
	}
	return p.pools[idx].setFor(key), nil
}

// existing returns the pool holding the newest latest version of
// bucket/key, -1 when no pool has it. A pool that cannot answer (no
// quorum) makes the answer unknown: its error is returned.
func (p *poolPlacement) existing(ctx context.Context, bucket, key string) (int, error) {
	infos, errs := p.lookup(ctx, bucket, key, "")
	best := -1
	var failure error
	for i, err := range errs {
		switch {
		case err == nil:
			if best < 0 || infos[i].ModTime.After(infos[best].ModTime) {
				best = i
			}
		case absent(err):
		case failure == nil:
			failure = fmt.Errorf("object: pool %d: %w", i+1, err)
		}
	}
	if best < 0 && failure != nil {
		return -1, failure
	}
	return best, nil
}

func (p *poolPlacement) forVersion(ctx context.Context, bucket, key, versionID string) (store, error) {
	if versionID == "" || p.single() {
		return p.forRead(ctx, bucket, key)
	}
	_, errs := p.lookup(ctx, bucket, key, versionID)
	var failure error
	for i, err := range errs {
		switch {
		case err == nil:
			return p.pools[i].setFor(key), nil
		case absent(err):
		case failure == nil:
			failure = fmt.Errorf("object: pool %d: %w", i+1, err)
		}
	}
	if failure != nil {
		return nil, failure
	}
	return p.home(key), nil
}

func (p *poolPlacement) forWrite(ctx context.Context, bucket, key string, size int64) (store, error) {
	if p.single() {
		return p.home(key), nil
	}
	idx, err := p.existing(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	if idx < 0 {
		if idx, err = p.pickPool(ctx, size); err != nil {
			return nil, err
		}
	}
	return p.pools[idx].setFor(key), nil
}

func (p *poolPlacement) forUpload(ctx context.Context, bucket, key, uploadID string) (store, error) {
	if p.single() || !validUploadID(uploadID) {
		return p.home(key), nil
	}
	path := uploadDir(bucket, uploadID) + "/" + uploadFile
	errs := parallel(len(p.pools), func(i int) error {
		_, err := p.pools[i].setFor(key).ReadAll(ctx, path)
		return err
	})
	var failure error
	for i, err := range errs {
		switch {
		case err == nil:
			return p.pools[i].setFor(key), nil
		case absent(err):
		case failure == nil:
			failure = fmt.Errorf("object: pool %d: %w", i+1, err)
		}
	}
	if failure != nil {
		return nil, failure
	}
	return p.home(key), nil
}

func (p *poolPlacement) others(st store, key string) []store {
	if p.single() {
		return nil
	}
	out := make([]store, 0, len(p.pools)-1)
	for _, pl := range p.pools {
		if s := pl.setFor(key); s != st {
			out = append(out, s)
		}
	}
	return out
}

// maxPoolUsage is the used fraction above which a pool receives no new
// objects (docs/03 §3.2).
const maxPoolUsage = 0.95

// pickPool chooses the pool of a new object: at random, weighted by free
// space, among the pools that are not being decommissioned, are at most
// 95% full and have room for size bytes. It fails with
// domain.ErrStorageFull when no pool qualifies.
func (p *poolPlacement) pickPool(ctx context.Context, size int64) (int, error) {
	spaces := make([]poolSpace, len(p.pools))
	_ = parallel(len(p.pools), func(i int) error {
		spaces[i] = p.pools[i].capacity(ctx, p.now(), p.spaceTTL)
		return nil
	})
	weights := make([]uint64, len(p.pools))
	var total uint64
	for i, pl := range p.pools {
		s := spaces[i]
		if pl.decommissioning || s.usedRatio() > maxPoolUsage || (size > 0 && s.free < uint64(size)) {
			continue
		}
		weights[i] = s.free
		total += s.free
	}
	if total == 0 {
		return -1, fmt.Errorf("%w: no pool has room for a new object", domain.ErrStorageFull)
	}
	r := p.randN(total)
	for i, w := range weights {
		if r < w {
			return i, nil
		}
		r -= w
	}
	return len(weights) - 1, nil
}

// capacity returns the capacity of the online drives of the pool, cached
// for ttl.
func (pl *pool) capacity(ctx context.Context, now time.Time, ttl time.Duration) poolSpace {
	pl.mu.Lock()
	if pl.hasSpace && now.Sub(pl.spaceAt) < ttl {
		s := pl.space
		pl.mu.Unlock()
		return s
	}
	pl.mu.Unlock()
	var drives []storage.StorageAPI
	for _, set := range pl.sets {
		drives = append(drives, set.drives...)
	}
	infos := make([]domain.DiskInfo, len(drives))
	errs := parallel(len(drives), func(i int) error {
		d := drives[i]
		if d == nil || !d.IsOnline() {
			return domain.ErrDiskNotFound
		}
		info, err := d.DiskInfo(ctx, storage.DiskInfoOptions{})
		infos[i] = info
		return err
	})
	var s poolSpace
	for i, err := range errs {
		if err == nil {
			s.total += infos[i].Total
			s.free += infos[i].Free
		}
	}
	pl.mu.Lock()
	pl.space, pl.spaceAt, pl.hasSpace = s, now, true
	pl.mu.Unlock()
	return s
}
