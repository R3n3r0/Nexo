package cache

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maypok86/otter/v2"

	"github.com/r3n3r0/nexo/internal/domain"
)

// entryKind is what an entry holds.
type entryKind uint8

// Entry kinds.
const (
	// kindObject is an object with its whole content (up to a block).
	kindObject entryKind = iota + 1
	// kindHead is the description of an object cached by blocks.
	kindHead
	// kindInfo is the description of an object only (HEAD).
	kindInfo
	// kindNegative is a missing object or a refused read (404, 403).
	kindNegative
	// kindPass is an object found uncacheable (hit-for-pass).
	kindPass
	// kindBlock is a block of an object cached by blocks.
	kindBlock
	// kindVariant is a compressed variant of an object.
	kindVariant
)

func (k entryKind) String() string {
	switch k {
	case kindObject:
		return "object"
	case kindHead:
		return "head"
	case kindInfo:
		return "info"
	case kindNegative:
		return "negative"
	case kindPass:
		return "pass"
	case kindBlock:
		return "block"
	case kindVariant:
		return "variant"
	}
	return "unknown"
}

// negative describes a cached refusal.
type negative struct {
	// Code is "not-found", "bucket-not-found", "delete-marker" or
	// "access-denied".
	Code      string    `json:"code"`
	VersionID string    `json:"versionId,omitempty"`
	ModTime   time.Time `json:"modTime,omitempty"`
}

// entryMeta describes an entry. It is immutable once stored: a change
// (revalidation, soft purge) stores a new value.
type entryMeta struct {
	Key  string    `json:"key"`
	Kind entryKind `json:"kind"`
	// Bucket and Object are the object the entry belongs to.
	Bucket string `json:"bucket"`
	Object string `json:"object"`
	// Distribution, Host and Path scope the entry: Host is empty for the
	// entries shared by the aliases (cacheKey.includeHost false), Path is
	// the path of the object in the distribution.
	Distribution string `json:"distribution,omitempty"`
	Host         string `json:"host,omitempty"`
	Path         string `json:"path,omitempty"`
	// Scopes are the quota scopes the entry counts in.
	Scopes []string `json:"scopes,omitempty"`
	// Tags are the cache tags (surrogate keys) of the object.
	Tags []string           `json:"tags,omitempty"`
	Info *domain.ObjectInfo `json:"info,omitempty"`
	// ETag is the version of the object a block or a variant was cut
	// from.
	ETag      string    `json:"etag,omitempty"`
	Neg       *negative `json:"neg,omitempty"`
	Block     int64     `json:"block,omitempty"`
	BlockSize int64     `json:"blockSize,omitempty"`
	Encoding  string    `json:"encoding,omitempty"`
	// StoredAt is when the content was read or revalidated, Expires when
	// it stops being fresh; SWR and SIE extend its use after Expires.
	StoredAt time.Time     `json:"storedAt"`
	Expires  time.Time     `json:"expires"`
	SWR      time.Duration `json:"swr,omitempty"`
	SIE      time.Duration `json:"sie,omitempty"`
	// Size and CRC describe the content.
	Size int64  `json:"size"`
	CRC  uint32 `json:"crc"`
	// MemoryOnly keeps the entry off the disk (encrypted objects).
	MemoryOnly bool `json:"memoryOnly,omitempty"`
	// Unverified marks an entry loaded from the disk at startup: it is
	// revalidated with the origin before it is served.
	Unverified bool `json:"-"`
}

// objRef identifies an object.
type objRef struct{ bucket, object string }

// record is the state of a key in the tiers.
type record struct {
	key  string
	meta atomic.Pointer[entryMeta]
	// last is the time of the last access (unix nanoseconds), for the
	// eviction within the quotas.
	last atomic.Int64
	// dead reports a removed record.
	dead atomic.Bool
	// size is the bytes the entry counts in its quotas; inL1, inL2, l1v
	// are guarded by store.mu.
	size     int64
	diskSize int64
	inL1     bool
	inL2     bool
	l1v      *l1value
}

func (r *record) touch(now time.Time) { r.last.Store(now.UnixNano()) }

// l1value is a value of the memory tier.
type l1value struct {
	rec  *record
	body []byte
}

// tier names where an entry was found.
type tier string

const (
	tierMemory tier = "memory"
	tierDisk   tier = "disk"
)

// metaOverhead is the weight of an entry without content in the memory
// tier.
const metaOverhead = 512

// evictionSample is how many entries the eviction within a quota
// compares (approximated LRU).
const evictionSample = 16

// scopeUsage counts the bytes of a quota scope.
type scopeUsage struct {
	bytes int64
	recs  map[string]*record
}

// quota is a quota scope and its limit (0: none).
type quota struct {
	id    string
	limit int64
}

// storeDeps are the dependencies of the store.
type storeDeps struct {
	memorySize int64
	// memoryEntryMax bounds the content of an entry of the memory tier.
	memoryEntryMax int64
	disk           *disk
	now            func() time.Time
	logger         *slog.Logger
	onEvict        func(tier tier)
}

// store holds the entries: the memory tier (W-TinyLFU), the disk tier, the
// indexes of the invalidations and the quotas.
type store struct {
	deps storeDeps
	l1   *otter.Cache[string, *l1value]

	mu       sync.Mutex
	recs     map[string]*record
	byObject map[objRef]map[string]*record
	byBucket map[string]map[objRef]struct{}
	byTag    map[string]map[string]*record
	scopes   map[string]*scopeUsage
	diskRecs map[string]*record
	diskUsed int64

	// Invalidation barriers: a fill that began before the last
	// invalidation of its object (or a broader one) is not stored.
	seq       uint64
	objInv    map[objRef]uint64
	bucketInv map[string]uint64
	globalInv uint64
	inflight  map[uint64]int

	writes chan diskWrite
	// pending counts the queued disk writes not done yet.
	pending atomic.Int64
	wg      sync.WaitGroup
	closed  atomic.Bool
}

// diskWrite is an entry to write to the disk tier.
type diskWrite struct {
	rec  *record
	meta *entryMeta
	body []byte
}

// newStore builds the store.
func newStore(deps storeDeps) (*store, error) {
	s := &store{
		deps: deps, recs: map[string]*record{}, byObject: map[objRef]map[string]*record{}, byBucket: map[string]map[objRef]struct{}{},
		byTag: map[string]map[string]*record{}, scopes: map[string]*scopeUsage{}, diskRecs: map[string]*record{},
		objInv: map[objRef]uint64{}, bucketInv: map[string]uint64{}, inflight: map[uint64]int{},
	}
	l1, err := otter.New(&otter.Options[string, *l1value]{
		MaximumWeight: uint64(deps.memorySize),
		Weigher: func(key string, v *l1value) uint32 {
			return uint32(len(v.body) + len(key) + metaOverhead)
		},
		// The evictions are reported on their own goroutines (the default
		// executor): the handler takes the lock of the store, which is held
		// when the store invalidates entries of the memory tier.
		OnDeletion: s.onL1Deletion,
	})
	if err != nil {
		return nil, err
	}
	s.l1 = l1
	if deps.disk != nil {
		s.writes = make(chan diskWrite, defaultDiskQueue)
		for range defaultDiskWriters {
			s.wg.Add(1)
			go s.diskWriter()
		}
	}
	return s, nil
}

// close stops the disk writers.
func (s *store) close() {
	if s.closed.Swap(true) || s.writes == nil {
		return
	}
	close(s.writes)
	s.wg.Wait()
}

// onL1Deletion follows the evictions of the memory tier.
func (s *store) onL1Deletion(e otter.DeletionEvent[string, *l1value]) {
	if !e.WasEvicted() {
		return
	}
	v := e.Value
	s.mu.Lock()
	rec := v.rec
	if rec.l1v == v {
		rec.inL1, rec.l1v = false, nil
		if !rec.inL2 {
			s.removeLocked(rec)
		}
	}
	s.mu.Unlock()
	if s.deps.onEvict != nil {
		s.deps.onEvict(tierMemory)
	}
}

// --- lookup ---------------------------------------------------------------------------

// get returns the entry of key and its content (nil for the entries
// without content).
func (s *store) get(ctx context.Context, key string) (*entryMeta, []byte, tier, bool) {
	now := s.deps.now()
	if v, ok := s.l1.GetIfPresent(key); ok && !v.rec.dead.Load() {
		v.rec.touch(now)
		return v.rec.meta.Load(), v.body, tierMemory, true
	}
	s.mu.Lock()
	rec := s.recs[key]
	onDisk := rec != nil && rec.inL2
	s.mu.Unlock()
	if !onDisk || s.deps.disk == nil {
		return nil, nil, "", false
	}
	meta := rec.meta.Load()
	body, err := s.deps.disk.read(key, meta)
	if err != nil {
		s.deps.logger.WarnContext(ctx, "cache: disk entry dropped", "key", key, "err", err)
		s.mu.Lock()
		if s.recs[key] == rec {
			s.removeLocked(rec)
		}
		s.mu.Unlock()
		return nil, nil, "", false
	}
	rec.touch(now)
	s.promote(rec, body)
	return meta, body, tierDisk, true
}

// promote copies an entry of the disk tier to the memory tier.
func (s *store) promote(rec *record, body []byte) {
	if int64(len(body)) > s.deps.memoryEntryMax {
		return
	}
	v := &l1value{rec: rec, body: body}
	s.mu.Lock()
	if rec.dead.Load() || rec.inL1 {
		s.mu.Unlock()
		return
	}
	rec.inL1, rec.l1v = true, v
	s.mu.Unlock()
	s.l1.Set(rec.key, v)
}

// peek returns the entry of key without reading its content.
func (s *store) peek(key string) (*entryMeta, tier, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.recs[key]
	if rec == nil {
		return nil, "", false
	}
	t := tierDisk
	if rec.inL1 {
		t = tierMemory
	}
	return rec.meta.Load(), t, true
}

// keysOf returns the keys of the entries of an object.
func (s *store) keysOf(bucket, object string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.byObject[objRef{bucket, object}] {
		out = append(out, k)
	}
	return out
}

// --- fills ----------------------------------------------------------------------------

// begin registers a fill and returns its barrier.
func (s *store) begin() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.seq
	s.inflight[seq]++
	return seq
}

// end unregisters a fill.
func (s *store) end(seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[seq]--; s.inflight[seq] <= 0 {
		delete(s.inflight, seq)
	}
	s.pruneLocked()
}

// pruneLocked forgets the invalidations older than every fill in flight.
func (s *store) pruneLocked() {
	if len(s.objInv) < 1024 {
		return
	}
	oldest := s.seq
	for seq := range s.inflight {
		oldest = min(oldest, seq)
	}
	for ref, seq := range s.objInv {
		if seq <= oldest {
			delete(s.objInv, ref)
		}
	}
	for b, seq := range s.bucketInv {
		if seq <= oldest {
			delete(s.bucketInv, b)
		}
	}
}

// invalidatedSince reports an invalidation of the object after the
// barrier since.
func (s *store) invalidatedSince(ref objRef, since uint64) bool {
	return s.globalInv > since || s.bucketInv[ref.bucket] > since || s.objInv[ref] > since
}

// put stores an entry filled since the barrier; quotas are the scopes it
// counts in. It reports whether the entry was stored.
func (s *store) put(meta *entryMeta, body []byte, since uint64, quotas []quota) bool {
	toMemory := int64(len(body)) <= s.deps.memoryEntryMax
	toDisk := s.deps.disk != nil && !meta.MemoryOnly && meta.Kind != kindNegative && meta.Kind != kindPass
	if !toMemory && !toDisk {
		return false
	}
	size := int64(len(body)) + metaOverhead
	ref := objRef{meta.Bucket, meta.Object}
	s.mu.Lock()
	if s.invalidatedSince(ref, since) {
		s.mu.Unlock()
		return false
	}
	for _, q := range quotas {
		if q.limit > 0 && size > q.limit {
			s.mu.Unlock()
			return false
		}
	}
	if old := s.recs[meta.Key]; old != nil {
		s.removeLocked(old)
	}
	for _, q := range quotas {
		if q.limit > 0 {
			s.makeRoomLocked(q, size)
		}
	}
	rec := &record{key: meta.Key, size: size}
	rec.meta.Store(meta)
	rec.touch(s.deps.now())
	s.recs[meta.Key] = rec
	objs := s.byObject[ref]
	if objs == nil {
		objs = map[string]*record{}
		s.byObject[ref] = objs
		if s.byBucket[ref.bucket] == nil {
			s.byBucket[ref.bucket] = map[objRef]struct{}{}
		}
		s.byBucket[ref.bucket][ref] = struct{}{}
	}
	objs[meta.Key] = rec
	for _, t := range meta.Tags {
		if s.byTag[t] == nil {
			s.byTag[t] = map[string]*record{}
		}
		s.byTag[t][meta.Key] = rec
	}
	for _, q := range quotas {
		u := s.scopes[q.id]
		if u == nil {
			u = &scopeUsage{recs: map[string]*record{}}
			s.scopes[q.id] = u
		}
		u.bytes += size
		u.recs[meta.Key] = rec
	}
	var v *l1value
	if toMemory {
		v = &l1value{rec: rec, body: body}
		rec.inL1, rec.l1v = true, v
	}
	s.mu.Unlock()
	if v != nil {
		s.l1.Set(meta.Key, v)
	}
	if toDisk {
		s.queueDisk(diskWrite{rec: rec, meta: meta, body: body})
	}
	return true
}

// makeRoomLocked evicts the least recently used entries of a scope (by
// sampling) until size fits in its quota.
func (s *store) makeRoomLocked(q quota, size int64) {
	u := s.scopes[q.id]
	for u != nil && u.bytes+size > q.limit && len(u.recs) > 0 {
		var victim *record
		n := 0
		for _, r := range u.recs {
			if victim == nil || r.last.Load() < victim.last.Load() {
				victim = r
			}
			if n++; n >= evictionSample {
				break
			}
		}
		s.removeLocked(victim)
		if s.deps.onEvict != nil {
			go s.deps.onEvict(tierMemory)
		}
	}
}

// update replaces the description of an entry (revalidation, soft
// purge), keeping its content.
func (s *store) update(meta *entryMeta) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.recs[meta.Key]
	if rec == nil {
		return false
	}
	rec.meta.Store(meta)
	return true
}

// --- disk writes -------------------------------------------------------------------

// queueDisk queues an entry for the disk tier; a full queue drops it (the
// entry stays in memory).
func (s *store) queueDisk(w diskWrite) {
	if s.closed.Load() {
		return
	}
	defer func() {
		if recover() != nil { // the queue closed meanwhile
			s.pending.Add(-1)
		}
	}()
	s.pending.Add(1)
	select {
	case s.writes <- w:
	default:
		s.pending.Add(-1)
	}
}

// diskWriter writes the queued entries.
func (s *store) diskWriter() {
	defer s.wg.Done()
	for w := range s.writes {
		s.writeDisk(w)
		s.pending.Add(-1)
	}
}

// writeDisk writes an entry to the disk tier; an entry removed meanwhile
// is not kept.
func (s *store) writeDisk(w diskWrite) {
	if w.rec.dead.Load() {
		return
	}
	n, err := s.deps.disk.write(w.meta, w.body)
	if err != nil {
		s.deps.logger.Warn("cache: disk entry not written", "key", w.meta.Key, "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if w.rec.dead.Load() || s.recs[w.meta.Key] != w.rec {
		s.deps.disk.remove(w.meta.Key)
		return
	}
	w.rec.inL2, w.rec.diskSize = true, n
	s.diskRecs[w.meta.Key] = w.rec
	s.diskUsed += n
	s.evictDiskLocked()
}

// evictDiskLocked keeps the disk tier within its size.
func (s *store) evictDiskLocked() {
	d := s.deps.disk
	for s.diskUsed > d.max && len(s.diskRecs) > 0 {
		var victim *record
		n := 0
		for _, r := range s.diskRecs {
			if victim == nil || r.last.Load() < victim.last.Load() {
				victim = r
			}
			if n++; n >= evictionSample {
				break
			}
		}
		s.dropDiskLocked(victim)
		if !victim.inL1 {
			s.removeLocked(victim)
		}
		if s.deps.onEvict != nil {
			go s.deps.onEvict(tierDisk)
		}
	}
}

// dropDiskLocked removes the disk copy of a record.
func (s *store) dropDiskLocked(rec *record) {
	if !rec.inL2 {
		return
	}
	rec.inL2 = false
	delete(s.diskRecs, rec.key)
	s.diskUsed -= rec.diskSize
	s.deps.disk.remove(rec.key)
}

// adopt indexes an entry found on the disk tier at startup: it is
// unverified until the origin confirms it.
func (s *store) adopt(meta *entryMeta, diskSize int64) {
	meta.Unverified = true
	ref := objRef{meta.Bucket, meta.Object}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.recs[meta.Key]; exists {
		return
	}
	rec := &record{key: meta.Key, size: meta.Size + metaOverhead, diskSize: diskSize, inL2: true}
	rec.meta.Store(meta)
	rec.last.Store(meta.StoredAt.UnixNano())
	s.recs[meta.Key] = rec
	s.diskRecs[meta.Key] = rec
	s.diskUsed += diskSize
	objs := s.byObject[ref]
	if objs == nil {
		objs = map[string]*record{}
		s.byObject[ref] = objs
		if s.byBucket[ref.bucket] == nil {
			s.byBucket[ref.bucket] = map[objRef]struct{}{}
		}
		s.byBucket[ref.bucket][ref] = struct{}{}
	}
	objs[meta.Key] = rec
	for _, t := range meta.Tags {
		if s.byTag[t] == nil {
			s.byTag[t] = map[string]*record{}
		}
		s.byTag[t][meta.Key] = rec
	}
	for _, id := range meta.Scopes {
		u := s.scopes[id]
		if u == nil {
			u = &scopeUsage{recs: map[string]*record{}}
			s.scopes[id] = u
		}
		u.bytes += rec.size
		u.recs[meta.Key] = rec
	}
	s.evictDiskLocked()
}

// --- removal ---------------------------------------------------------------------------

// removeLocked removes a record from every tier and index.
func (s *store) removeLocked(rec *record) {
	if rec.dead.Swap(true) {
		return
	}
	meta := rec.meta.Load()
	if s.recs[rec.key] == rec {
		delete(s.recs, rec.key)
	}
	ref := objRef{meta.Bucket, meta.Object}
	if objs := s.byObject[ref]; objs != nil {
		delete(objs, rec.key)
		if len(objs) == 0 {
			delete(s.byObject, ref)
			if b := s.byBucket[ref.bucket]; b != nil {
				delete(b, ref)
				if len(b) == 0 {
					delete(s.byBucket, ref.bucket)
				}
			}
		}
	}
	for _, t := range meta.Tags {
		if m := s.byTag[t]; m != nil {
			delete(m, rec.key)
			if len(m) == 0 {
				delete(s.byTag, t)
			}
		}
	}
	for _, id := range meta.Scopes {
		if u := s.scopes[id]; u != nil {
			if _, ok := u.recs[rec.key]; ok {
				delete(u.recs, rec.key)
				u.bytes -= rec.size
			}
			if len(u.recs) == 0 {
				delete(s.scopes, id)
			}
		}
	}
	if rec.inL1 {
		rec.inL1, rec.l1v = false, nil
		// The deletion listener ignores the invalidations.
		s.l1.Invalidate(rec.key)
	}
	s.dropDiskLocked(rec)
}

// removeObject drops the entries of an object (keep spares the entries
// it selects) and returns their descriptions.
func (s *store) removeObject(bucket, object string, keep func(*entryMeta) bool) []*entryMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := objRef{bucket, object}
	s.seq++
	s.objInv[ref] = s.seq
	var out []*entryMeta
	for _, rec := range s.byObject[ref] {
		m := rec.meta.Load()
		if keep != nil && keep(m) {
			continue
		}
		out = append(out, m)
		s.removeLocked(rec)
	}
	return out
}

// removeBucket drops the entries of a bucket.
func (s *store) removeBucket(bucket string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	s.bucketInv[bucket] = s.seq
	n := 0
	for ref := range s.byBucket[bucket] {
		for _, rec := range s.byObject[ref] {
			s.removeLocked(rec)
			n++
		}
	}
	return n
}

// removeWhere drops (or, soft, marks expired) the entries fn selects.
func (s *store) removeWhere(fn func(*entryMeta) bool, soft bool) int {
	return s.removeRecs(func(yield func(*record) bool) {
		for _, r := range s.recs {
			if !yield(r) {
				return
			}
		}
	}, fn, soft)
}

// removeTagged drops (or marks) the entries carrying one of the tags that
// fn selects.
func (s *store) removeTagged(tags []string, fn func(*entryMeta) bool, soft bool) int {
	return s.removeRecs(func(yield func(*record) bool) {
		for _, t := range tags {
			for _, r := range s.byTag[t] {
				if !yield(r) {
					return
				}
			}
		}
	}, fn, soft)
}

// removeRecs applies a broad invalidation to the records of seq: it is a
// barrier for every fill in flight.
func (s *store) removeRecs(seq func(yield func(*record) bool), fn func(*entryMeta) bool, soft bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	s.globalInv = s.seq
	var victims []*record
	seq(func(r *record) bool {
		if fn(r.meta.Load()) {
			victims = append(victims, r)
		}
		return true
	})
	now := s.deps.now()
	for _, r := range victims {
		if soft {
			m := *r.meta.Load()
			if m.Kind == kindBlock || m.Kind == kindVariant {
				continue
			}
			if m.Expires.After(now) {
				m.Expires = now
			}
			r.meta.Store(&m)
			continue
		}
		s.removeLocked(r)
	}
	return len(victims)
}

// clear drops every entry.
func (s *store) clear() int {
	return s.removeWhere(func(*entryMeta) bool { return true }, false)
}

// --- statistics --------------------------------------------------------------------

// usage are the entries and sizes of the store.
type usage struct {
	entries int
	memory  int64
	disk    int64
	// hosts are the sizes per distribution and host.
	hosts map[[2]string]*hostUsage
}

type hostUsage struct {
	entries int
	memory  int64
	disk    int64
}

// usage computes the sizes of the tiers, per host.
func (s *store) usage() usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := usage{entries: len(s.recs), disk: s.diskUsed, hosts: map[[2]string]*hostUsage{}}
	for _, r := range s.recs {
		m := r.meta.Load()
		k := [2]string{m.Distribution, m.Host}
		h := u.hosts[k]
		if h == nil {
			h = &hostUsage{}
			u.hosts[k] = h
		}
		h.entries++
		if r.inL1 {
			h.memory += r.size
		}
		if r.inL2 {
			h.disk += r.size
		}
	}
	u.memory = int64(s.l1.WeightedSize())
	return u
}

// scopeBytes returns the bytes of a quota scope.
func (s *store) scopeBytes(id string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u := s.scopes[id]; u != nil {
		return u.bytes
	}
	return 0
}

// presence reports a key and the variants and blocks of its object.
func (s *store) presence(key string, now time.Time) Presence {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := Presence{Key: key}
	rec := s.recs[key]
	if rec == nil {
		return p
	}
	m := rec.meta.Load()
	p.Present, p.Kind, p.Size, p.ETag = true, m.Kind.String(), Size(m.Size), m.ETag
	if m.Info != nil {
		p.ETag = m.Info.ETag
	}
	p.Tier = string(tierDisk)
	if rec.inL1 {
		p.Tier = string(tierMemory)
	}
	p.Age = Duration(now.Sub(m.StoredAt))
	p.ExpiresIn = Duration(m.Expires.Sub(now))
	p.Stale = !m.Expires.After(now) || m.Unverified
	for k, r := range s.byObject[objRef{m.Bucket, m.Object}] {
		rm := r.meta.Load()
		switch {
		case rm.Kind == kindVariant && strings.HasPrefix(k, key+"#e:"):
			p.Encodings = append(p.Encodings, rm.Encoding)
		case rm.Kind == kindBlock && strings.HasPrefix(k, key+"#b"):
			p.Blocks++
		}
	}
	return p
}
