package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// Limits of a manual invalidation.
const (
	maxInvalidationItems = 1000
)

// --- automatic invalidations --------------------------------------------------------

// ObjectChanged implements Invalidator.
func (s *service) ObjectChanged(ctx context.Context, change ObjectChange) error {
	if change.Bucket == "" || (change.Key == "" && !change.BucketWide) {
		return nil
	}
	removed := s.applyChange(ctx, change, true)
	s.countInvalidation(ctx, "auto")
	errs := s.broadcast(ctx, func(uint64) InvalidationMessage { return InvalidationMessage{Change: &change} })
	if !change.BucketWide {
		s.afterChange(ctx, change, removed)
	}
	if len(errs) == 0 {
		return nil
	}
	var all []error
	for _, e := range errs {
		all = append(all, e)
	}
	return fmt.Errorf("cache: invalidation of %s/%s not delivered to every peer (retried): %w", change.Bucket, change.Key, errors.Join(all...))
}

// applyChange drops the entries of a changed object on this node (the
// entries of the distributions that disabled the automatic invalidation
// are kept) and warms it again when a distribution asks for it.
func (s *service) applyChange(ctx context.Context, change ObjectChange, origin bool) []*entryMeta {
	if change.BucketWide {
		s.store.removeBucket(change.Bucket)
		return nil
	}
	sn := s.snap.Load()
	keep := func(m *entryMeta) bool {
		return m.Distribution != "" && !sn.resolveFor(ctx, m.Distribution, "", "/").eff.AutoInvalidate
	}
	removed := s.store.removeObject(change.Bucket, change.Key, keep)
	if origin || !s.shared() {
		// Each node warms its own cache (local mode); in the shared mode
		// the node that saw the write warms the owners.
		s.warmOnWrite(ctx, change)
	}
	return removed
}

// applyMessage applies an invalidation of a peer; it returns the entries
// dropped (or marked).
func (s *service) applyMessage(ctx context.Context, m InvalidationMessage) int {
	switch {
	case m.FlushAll:
		s.flushAll(ctx, "the queue of a peer overflowed")
		return 0
	case m.Change != nil:
		return len(s.applyChange(ctx, *m.Change, false))
	case m.Request != nil:
		n, err := s.applyRequest(ctx, *m.Request)
		if err != nil {
			// A request this node cannot apply exactly is applied
			// broadly: the whole cache of the distribution.
			s.deps.Logger.WarnContext(ctx, "cache: invalidation not understood, dropping the distribution", "id", m.ID, "err", err)
			dist := m.Request.Distribution
			return s.store.removeWhere(func(e *entryMeta) bool { return dist == "" || e.Distribution == dist }, false)
		}
		return n
	}
	return 0
}

// afterChange queues the purge webhooks of the distributions serving a
// changed object.
func (s *service) afterChange(ctx context.Context, change ObjectChange, removed []*entryMeta) {
	sn := s.snap.Load()
	for _, d := range s.distributionsOf(change.Bucket) {
		prefix := keyPrefixOf(d)
		if !strings.HasPrefix(change.Key, prefix) {
			continue
		}
		if !sn.resolveFor(ctx, d.ID, "", "/").eff.AutoInvalidate {
			continue
		}
		rel := strings.TrimPrefix(change.Key, prefix)
		paths := []string{"/" + rel}
		if idx := d.DefaultBehavior.IndexDocument; idx != "" && (rel == idx || strings.HasSuffix(rel, "/"+idx)) {
			paths = append(paths, "/"+strings.TrimSuffix(rel, idx))
		}
		tags := slices.Clone(change.Tags)
		for _, m := range removed {
			if m.Distribution == d.ID {
				tags = append(tags, m.Tags...)
			}
		}
		s.queueWebhooks(ctx, sn, d, purgeJob{Distribution: d.ID, Paths: paths, Tags: splitTags(tags...), Automatic: true})
	}
}

// keyPrefixOf is the key prefix of the origin of a distribution.
func keyPrefixOf(d distribution.Distribution) string {
	if p := strings.Trim(d.Origin.PathPrefix, "/"); p != "" {
		return p + "/"
	}
	return ""
}

// Refreshes of the index of the distributions: at most one per
// distRefreshMin on the requests, one per distRefreshEvery in the
// background (the index serves the webhooks and the warm-up on write).
const (
	distRefreshMin   = time.Second
	distRefreshEvery = 30 * time.Second
)

// --- manual invalidations -------------------------------------------------------------

// newID returns a time-ordered id ("inv-<hex time>-<random>").
func (s *service) newID(prefix string) string {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	return prefix + "-" + strconv.FormatInt(s.now().UnixNano(), 16) + "-" + hex.EncodeToString(rnd[:])
}

// validateInvalidation checks a manual invalidation and expands its
// paths (a directory path also drops its index document).
func (s *service) validateInvalidation(ctx context.Context, req *InvalidationRequest) (*distribution.Distribution, error) {
	if len(req.Paths)+len(req.Patterns)+len(req.Tags) > maxInvalidationItems {
		return nil, fmt.Errorf("%w: at most %d paths, patterns and tags", ErrInvalid, maxInvalidationItems)
	}
	if req.Distribution == "" {
		if !req.All || req.Domain != "" || len(req.Paths)+len(req.Patterns)+len(req.Tags) > 0 {
			return nil, fmt.Errorf("%w: an invalidation names its distribution (only the global purge, all, does not)", ErrInvalid)
		}
		return nil, nil
	}
	d, err := s.deps.Distributions.Get(ctx, req.Distribution)
	if err != nil {
		return nil, err
	}
	if req.Domain != "" {
		req.Domain = normalizeHost(req.Domain)
		if !domainOf(d, req.Domain) {
			return nil, fmt.Errorf("%w: %s is not a domain of %s", ErrInvalid, req.Domain, d.ID)
		}
	}
	if !req.All && req.Domain == "" && len(req.Paths)+len(req.Patterns)+len(req.Tags) == 0 {
		return nil, fmt.Errorf("%w: nothing to invalidate (paths, patterns, tags, domain or all)", ErrInvalid)
	}
	var paths []string
	for _, p := range req.Paths {
		if !strings.HasPrefix(p, "/") {
			return nil, fmt.Errorf("%w: path %q must start with /", ErrInvalid, p)
		}
		paths = append(paths, p)
		if idx := d.DefaultBehavior.IndexDocument; idx != "" && strings.HasSuffix(p, "/") {
			paths = append(paths, p+idx)
		}
	}
	req.Paths = paths
	if _, err := compilePatterns(ctx, s.deps.Matcher, req.Patterns, "patterns"); err != nil {
		return nil, err
	}
	for _, t := range req.Tags {
		if t == "" || len(t) > maxTagLength {
			return nil, fmt.Errorf("%w: invalid tag %q", ErrInvalid, t)
		}
	}
	return &d, nil
}

// applyRequest applies a manual invalidation on this node.
func (s *service) applyRequest(ctx context.Context, req InvalidationRequest) (int, error) {
	scope := func(m *entryMeta) bool {
		if req.Distribution != "" && m.Distribution != req.Distribution {
			return false
		}
		return req.Domain == "" || m.Host == "" || hostMatches(req.Domain, m.Host)
	}
	if req.All || (req.Domain != "" && len(req.Paths)+len(req.Patterns)+len(req.Tags) == 0) {
		return s.store.removeWhere(scope, req.Soft), nil
	}
	n := 0
	if len(req.Tags) > 0 {
		n += s.store.removeTagged(req.Tags, scope, req.Soft)
	}
	if len(req.Paths)+len(req.Patterns) > 0 {
		patterns, err := compilePatterns(ctx, s.deps.Matcher, req.Patterns, "patterns")
		if err != nil {
			return n, err
		}
		paths := map[string]bool{}
		for _, p := range req.Paths {
			paths[p] = true
		}
		n += s.store.removeWhere(func(m *entryMeta) bool {
			if !scope(m) || m.Path == "" {
				return false
			}
			return paths[m.Path] || patterns.any(ctx, m.Path)
		}, req.Soft)
	}
	return n, nil
}

// Invalidate implements Operations.
func (s *service) Invalidate(ctx context.Context, req InvalidationRequest) (Invalidation, error) {
	d, err := s.validateInvalidation(ctx, &req)
	if err != nil {
		return Invalidation{}, err
	}
	inv := &Invalidation{ID: s.newID("inv"), Request: req, CreatedAt: s.now().UTC(), State: InvalidationInProgress}
	inv.Nodes = append(inv.Nodes, NodeStatus{Node: s.deps.NodeID, State: NodePending})
	for _, p := range s.deps.Peers {
		inv.Nodes = append(inv.Nodes, NodeStatus{Node: p.NodeID(), State: NodePending})
	}
	s.histMu.Lock()
	s.history[inv.ID] = inv
	s.histMu.Unlock()
	removed, err := s.applyRequest(ctx, req)
	if err != nil {
		return Invalidation{}, err
	}
	s.countInvalidation(ctx, "manual")
	s.setNodeStatus(inv.ID, s.deps.NodeID, removed, "")
	errs := s.broadcast(ctx, func(uint64) InvalidationMessage { return InvalidationMessage{ID: inv.ID, Request: &req} })
	for node, e := range errs {
		s.setNodeStatus(inv.ID, node, -1, e.Error())
	}
	if d != nil {
		sn := s.snap.Load()
		s.queueWebhooks(ctx, sn, *d, purgeJob{Distribution: d.ID, Domain: req.Domain, Paths: req.Paths, Patterns: req.Patterns,
			Tags: req.Tags, All: req.All, Soft: req.Soft, InvalidationID: inv.ID})
	}
	out := s.saveInvalidation(ctx, inv.ID)
	s.deps.Logger.InfoContext(ctx, "cache: invalidation", "id", inv.ID, "distribution", req.Distribution, "domain", req.Domain,
		"paths", len(req.Paths), "patterns", len(req.Patterns), "tags", len(req.Tags), "all", req.All, "soft", req.Soft, "removed", removed)
	return out, nil
}

// setNodeStatus records the state of an invalidation on a node: applied
// (removed >= 0) or pending (removed < 0, with the error).
func (s *service) setNodeStatus(id, node string, removed int, errMsg string) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	inv := s.history[id]
	if inv == nil {
		return
	}
	for i := range inv.Nodes {
		if inv.Nodes[i].Node != node {
			continue
		}
		if removed >= 0 {
			inv.Nodes[i] = NodeStatus{Node: node, State: NodeCompleted, At: s.now().UTC(), Removed: removed}
		} else if inv.Nodes[i].State != NodeCompleted {
			inv.Nodes[i].Error = errMsg
		}
	}
	inv.State = InvalidationCompleted
	for _, n := range inv.Nodes {
		if n.State != NodeCompleted {
			inv.State = InvalidationInProgress
		}
	}
}

// invalidationsApplied records the acknowledgement of manual
// invalidations by a peer (and saves the ones already stored).
func (s *service) invalidationsApplied(ctx context.Context, node string, ids map[string]int) {
	for id, removed := range ids {
		s.setNodeStatus(id, node, removed, "")
		s.histMu.Lock()
		saved := s.saved[id]
		s.histMu.Unlock()
		if saved {
			s.saveInvalidation(ctx, id)
		}
	}
}

// historyScope is the directory of the history of a distribution.
func historyScope(dist string) string {
	if dist == "" {
		return invalidationsDir + "/" + globalScope
	}
	return invalidationsDir + "/" + dist
}

// saveInvalidation stores the record of an invalidation (the history is
// shared by the nodes) and returns it; the record leaves the memory of
// this node once every node applied it.
func (s *service) saveInvalidation(ctx context.Context, id string) Invalidation {
	s.histMu.Lock()
	inv := s.history[id]
	if inv == nil {
		s.histMu.Unlock()
		return Invalidation{}
	}
	cp := *inv
	cp.Nodes = slices.Clone(inv.Nodes)
	s.saved[id] = true
	if cp.State == InvalidationCompleted {
		delete(s.history, id)
		delete(s.saved, id)
	}
	s.histMu.Unlock()
	dir := historyScope(cp.Request.Distribution)
	if _, err := s.putDoc(context.WithoutCancel(ctx), docPath(dir, id), cp); err != nil {
		s.deps.Logger.WarnContext(ctx, "cache: invalidation record not stored", "id", id, "err", err)
	}
	s.pruneHistory(ctx, dir)
	return cp
}

// pruneHistory keeps the newest records of a history.
func (s *service) pruneHistory(ctx context.Context, dir string) {
	docs, err := s.deps.Store.List(ctx, dir)
	if err != nil || len(docs) <= s.opts.history {
		return
	}
	names := make([]string, 0, len(docs))
	for name := range docs {
		names = append(names, name)
	}
	// The ids are time-ordered.
	slices.Sort(names)
	for _, name := range names[:len(names)-s.opts.history] {
		_ = s.deps.Store.Delete(ctx, dir+"/"+name)
	}
}

// GetInvalidation implements Operations.
func (s *service) GetInvalidation(ctx context.Context, dist, id string) (Invalidation, error) {
	s.histMu.Lock()
	if inv := s.history[id]; inv != nil && inv.Request.Distribution == dist {
		cp := *inv
		cp.Nodes = slices.Clone(inv.Nodes)
		s.histMu.Unlock()
		return cp, nil
	}
	s.histMu.Unlock()
	if !validHistoryID(id) {
		return Invalidation{}, fmt.Errorf("%w: %q", ErrInvalidationNotFound, id)
	}
	data, err := s.deps.Store.Get(ctx, docPath(historyScope(dist), id))
	if errors.Is(err, sysstore.ErrNotFound) {
		return Invalidation{}, fmt.Errorf("%w: %s", ErrInvalidationNotFound, id)
	}
	if err != nil {
		return Invalidation{}, fmt.Errorf("cache: read invalidation %s: %w", id, err)
	}
	var inv Invalidation
	if err := json.Unmarshal(data, &inv); err != nil {
		return Invalidation{}, fmt.Errorf("cache: decode invalidation %s: %w", id, err)
	}
	return inv, nil
}

// validHistoryID reports an id of the history ("inv-..." or "warm-...").
func validHistoryID(id string) bool {
	if len(id) > 64 || id == "" {
		return false
	}
	for _, c := range id {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// ListInvalidations implements Operations.
func (s *service) ListInvalidations(ctx context.Context, dist string) ([]Invalidation, error) {
	docs, err := s.deps.Store.List(ctx, historyScope(dist))
	if err != nil {
		return nil, fmt.Errorf("cache: list invalidations: %w", err)
	}
	byID := map[string]Invalidation{}
	for _, data := range docs {
		var inv Invalidation
		if json.Unmarshal(data, &inv) == nil && inv.ID != "" {
			byID[inv.ID] = inv
		}
	}
	s.histMu.Lock()
	for id, inv := range s.history {
		if inv.Request.Distribution == dist {
			cp := *inv
			cp.Nodes = slices.Clone(inv.Nodes)
			byID[id] = cp
		}
	}
	s.histMu.Unlock()
	out := make([]Invalidation, 0, len(byID))
	for _, inv := range byID {
		out = append(out, inv)
	}
	slices.SortFunc(out, func(a, b Invalidation) int { return strings.Compare(b.ID, a.ID) })
	return out, nil
}

// --- distributions index ------------------------------------------------------------

// distIndex maps the buckets to the distributions serving them.
type distIndex struct {
	byBucket map[string][]distribution.Distribution
	byID     map[string]distribution.Distribution
}

// distributionsOf returns the distributions serving a bucket.
func (s *service) distributionsOf(bucket string) []distribution.Distribution {
	if idx := s.dists.Load(); idx != nil {
		return idx.byBucket[bucket]
	}
	return nil
}

// refreshDistributions reloads the index of the distributions (used by
// the webhooks and the warm-up on write, not by the cache itself).
func (s *service) refreshDistributions(ctx context.Context) {
	s.distMu.Lock()
	defer s.distMu.Unlock()
	s.distRefreshed.Store(time.Now().UnixNano())
	list, err := s.deps.Distributions.List(ctx)
	if err != nil {
		s.deps.Logger.DebugContext(ctx, "cache: distributions not listed", "err", err)
		return
	}
	idx := &distIndex{byBucket: map[string][]distribution.Distribution{}, byID: map[string]distribution.Distribution{}}
	for _, d := range list {
		idx.byID[d.ID] = d
		if d.Enabled {
			idx.byBucket[d.Origin.Bucket] = append(idx.byBucket[d.Origin.Bucket], d)
		}
	}
	s.dists.Store(idx)
}

// histState is the in-memory history of the invalidations of this node.
type histState struct {
	histMu  sync.Mutex
	history map[string]*Invalidation
	saved   map[string]bool
}
