package app

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync/atomic"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// cacheWebhooksDir is the directory of the queue of the purge webhooks on
// a local drive (without --cache-dir), under the system volume.
const cacheWebhooksDir = "cache-webhooks"

// cacheDeps are the inputs of newCache.
type cacheDeps struct {
	cfg      config.Static
	f        *foundation
	docs     sysstore.Store
	objects  object.Service
	dist     *distStack
	matcher  match.Service
	registry metrics.Registry
	clock    clock.Clock
	logger   *slog.Logger
}

// newCache builds the cache of the distributions (docs/11) on the system
// documents (in SNSD, a system store on the single drive) and installs it
// behind the holders built before it: the event hook of the object layer
// and the internode routes of a cluster node.
func newCache(d cacheDeps) (cache.Service, error) {
	logger := d.logger.With("component", "cache")
	docs := d.docs
	if docs == nil {
		if len(d.f.local) == 0 {
			return nil, fmt.Errorf("%w: cache: no local drive", domain.ErrInvalidArgument)
		}
		var err error
		if docs, err = sysstore.New(sysstore.Deps{Drives: d.f.local[:1], Locks: d.f.locks, Clock: d.clock, Logger: logger}); err != nil {
			return nil, err
		}
	}
	var opts []cache.Option
	if d.cfg.CacheMode != "" {
		opts = append(opts, cache.WithMode(cache.Mode(d.cfg.CacheMode)))
	}
	memory := d.cfg.CacheMemory
	if d.cfg.CacheDir != "" {
		// Every node of a cluster has its own disk tier (the nodes of the
		// tests share their file system).
		dir := d.cfg.CacheDir
		if d.f.distributed() {
			dir = filepath.Join(dir, hex.EncodeToString([]byte(d.f.nodeID)))
		}
		opts = append(opts, cache.WithDir(dir), cache.WithDiskSize(d.cfg.CacheSize))
	} else {
		if memory == 0 {
			memory = d.cfg.CacheSize
		}
		if root := eventQueueRoot(d.f); root != "" {
			opts = append(opts, cache.WithWebhookQueue(filepath.Join(filepath.Dir(root), cacheWebhooksDir)))
		}
	}
	opts = append(opts, cache.WithMemorySize(memory))
	deps := cache.Deps{
		Origin: d.objects, Store: docs, Locks: d.f.locks, Notifier: d.f.notifier, Distributions: d.dist.raw,
		Peers: d.f.cachePeers, NodeID: d.f.nodeID, Secrets: secretSource{store: accessrule.NewStaticSecrets(d.cfg.Secrets)},
		Registry: d.registry, Clock: d.clock, Logger: logger,
	}
	if d.matcher != nil {
		deps.Matcher = matchAdapter{m: d.matcher}
	}
	svc, err := cache.New(deps, opts...)
	if err != nil {
		return nil, err
	}
	if d.f.cacheNode != nil {
		d.f.cacheNode.set(svc)
	}
	return svc, nil
}

// cacheHook is the invalidator of the event hook of the object layer,
// built before the cache: the writes made before the cache exists have
// nothing to invalidate.
type cacheHook struct {
	inv atomic.Pointer[cache.Invalidator]
}

var _ cache.Invalidator = (*cacheHook)(nil)

// set installs the cache.
func (h *cacheHook) set(inv cache.Invalidator) { h.inv.Store(&inv) }

// ObjectChanged implements cache.Invalidator.
func (h *cacheHook) ObjectChanged(ctx context.Context, change cache.ObjectChange) error {
	if inv := h.inv.Load(); inv != nil {
		return (*inv).ObjectChanged(ctx, change)
	}
	return nil
}

// cacheNodeHolder is the cache of a cluster node served to its peers by
// the internode RPC, which starts before the services are composed: until
// then the calls fail with cache.ErrNotReady (the peers retry).
type cacheNodeHolder struct {
	node atomic.Pointer[cache.Node]
}

var _ cache.Node = (*cacheNodeHolder)(nil)

// set installs the cache.
func (h *cacheNodeHolder) set(n cache.Node) { h.node.Store(&n) }

func (h *cacheNodeHolder) get() (cache.Node, error) {
	if n := h.node.Load(); n != nil {
		return *n, nil
	}
	return nil, cache.ErrNotReady
}

// ApplyInvalidations implements cache.Node.
func (h *cacheNodeHolder) ApplyInvalidations(ctx context.Context, b cache.InvalidationBatch) (cache.InvalidationAck, error) {
	n, err := h.get()
	if err != nil {
		return cache.InvalidationAck{}, err
	}
	return n.ApplyInvalidations(ctx, b)
}

// SyncStatus implements cache.Node.
func (h *cacheNodeHolder) SyncStatus(ctx context.Context) (cache.SyncStatus, error) {
	n, err := h.get()
	if err != nil {
		return cache.SyncStatus{}, err
	}
	return n.SyncStatus(ctx)
}

// FetchPiece implements cache.Node.
func (h *cacheNodeHolder) FetchPiece(ctx context.Context, req cache.PieceRequest) (cache.PieceReply, error) {
	n, err := h.get()
	if err != nil {
		return cache.PieceReply{}, err
	}
	return n.FetchPiece(ctx, req)
}

// Presence implements cache.Node.
func (h *cacheNodeHolder) Presence(ctx context.Context, req cache.PresenceRequest) (cache.PresenceReply, error) {
	n, err := h.get()
	if err != nil {
		return cache.PresenceReply{}, err
	}
	return n.Presence(ctx, req)
}

// NodeStats implements cache.Node.
func (h *cacheNodeHolder) NodeStats(ctx context.Context, q cache.StatsQuery) (cache.NodeStats, error) {
	n, err := h.get()
	if err != nil {
		return cache.NodeStats{}, err
	}
	return n.NodeStats(ctx, q)
}

// StartWarm implements cache.Node.
func (h *cacheNodeHolder) StartWarm(ctx context.Context, job cache.WarmJob) error {
	n, err := h.get()
	if err != nil {
		return err
	}
	return n.StartWarm(ctx, job)
}

// Cache returns the cache of the distributions (the Admin API, the CLI
// and the integration tests manage it through it).
func (a *App) Cache() cache.Service {
	if s := a.services(); s != nil {
		return s.cache
	}
	return nil
}
