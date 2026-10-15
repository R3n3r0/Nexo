package rpc

import (
	"context"
	"fmt"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
)

// CacheRoutes exposes the cache of the node to its peers ("/cache/..."):
// the invalidations and their sequence, the pieces of the keys this node
// owns in the shared mode, the presence of keys, the statistics and the
// warm-ups (docs/11 §11.4).
type CacheRoutes interface{ Routes }

type (
	// CacheRoutesOption configures the cache routes.
	CacheRoutesOption func(*cacheRoutesOptions)
	// CacheClientOption configures the cache client of a peer.
	CacheClientOption func(*cacheClientOptions)

	cacheRoutesOptions struct{}
	cacheClientOptions struct{}
)

// CacheRoutesDeps are the dependencies of the cache routes.
type CacheRoutesDeps struct {
	// Cache is the internode side of the cache of the node (required): it
	// may answer cache.ErrNotReady until the services are composed.
	Cache cache.Node
}

// cacheRoutes implements CacheRoutes.
type cacheRoutes struct {
	deps CacheRoutesDeps
}

var _ CacheRoutes = (*cacheRoutes)(nil)

// NewCacheRoutes exposes the cache of the node.
func NewCacheRoutes(deps CacheRoutesDeps, opts ...CacheRoutesOption) (CacheRoutes, error) {
	_ = apply(opts)
	if deps.Cache == nil {
		return nil, fmt.Errorf("%w: rpc: the cache routes need the cache", domain.ErrInvalidArgument)
	}
	return &cacheRoutes{deps: deps}, nil
}

// Register implements Routes.
func (h *cacheRoutes) Register(r chi.Router) {
	r.Route("/cache", func(r chi.Router) {
		r.Post("/invalidate", unary(func(ctx context.Context, b cache.InvalidationBatch) (cache.InvalidationAck, error) {
			return h.deps.Cache.ApplyInvalidations(ctx, b)
		}))
		r.Post("/status", unary(func(ctx context.Context, req cache.SyncRequest) (cache.SyncStatus, error) {
			if c, ok := CallerFromContext(ctx); ok && c.NodeID != "" {
				// The verified caller, not what it claims.
				req.Node = c.NodeID
			}
			return h.deps.Cache.SyncStatus(ctx, req)
		}))
		r.Post("/fetch", unary(func(ctx context.Context, req cache.PieceRequest) (cache.PieceReply, error) {
			return h.deps.Cache.FetchPiece(ctx, req)
		}))
		r.Post("/presence", unary(func(ctx context.Context, req cache.PresenceRequest) (cache.PresenceReply, error) {
			return h.deps.Cache.Presence(ctx, req)
		}))
		r.Post("/stats", unary(func(ctx context.Context, q cache.StatsQuery) (cache.NodeStats, error) {
			return h.deps.Cache.NodeStats(ctx, q)
		}))
		r.Post("/warm", unary(func(ctx context.Context, job cache.WarmJob) (empty, error) {
			return empty{}, h.deps.Cache.StartWarm(ctx, job)
		}))
	})
}

// CacheClientDeps are the dependencies of the cache client.
type CacheClientDeps struct {
	// Conn reaches the node (required).
	Conn Conn
}

// cacheClient implements cache.Peer over the cache routes of another
// node.
type cacheClient struct {
	deps CacheClientDeps
}

var _ cache.Peer = (*cacheClient)(nil)

// NewCacheClient returns the cache of another node.
func NewCacheClient(deps CacheClientDeps, opts ...CacheClientOption) (cache.Peer, error) {
	_ = apply(opts)
	if deps.Conn == nil {
		return nil, fmt.Errorf("%w: rpc: the cache client needs a connection", domain.ErrInvalidArgument)
	}
	return &cacheClient{deps: deps}, nil
}

// NodeID implements cache.Peer.
func (c *cacheClient) NodeID() string { return c.deps.Conn.Node() }

// IsOnline implements cache.Peer.
func (c *cacheClient) IsOnline() bool { return c.deps.Conn.IsOnline() }

// ApplyInvalidations implements cache.Node.
func (c *cacheClient) ApplyInvalidations(ctx context.Context, b cache.InvalidationBatch) (cache.InvalidationAck, error) {
	var ack cache.InvalidationAck
	err := c.deps.Conn.Call(ctx, "cache/invalidate", b, &ack)
	return ack, err
}

// SyncStatus implements cache.Node.
func (c *cacheClient) SyncStatus(ctx context.Context, req cache.SyncRequest) (cache.SyncStatus, error) {
	var st cache.SyncStatus
	err := c.deps.Conn.Call(ctx, "cache/status", req, &st)
	return st, err
}

// FetchPiece implements cache.Node.
func (c *cacheClient) FetchPiece(ctx context.Context, req cache.PieceRequest) (cache.PieceReply, error) {
	var rep cache.PieceReply
	err := c.deps.Conn.Call(ctx, "cache/fetch", req, &rep)
	return rep, err
}

// Presence implements cache.Node.
func (c *cacheClient) Presence(ctx context.Context, req cache.PresenceRequest) (cache.PresenceReply, error) {
	var rep cache.PresenceReply
	err := c.deps.Conn.Call(ctx, "cache/presence", req, &rep)
	return rep, err
}

// NodeStats implements cache.Node.
func (c *cacheClient) NodeStats(ctx context.Context, q cache.StatsQuery) (cache.NodeStats, error) {
	var st cache.NodeStats
	err := c.deps.Conn.Call(ctx, "cache/stats", q, &st)
	return st, err
}

// StartWarm implements cache.Node.
func (c *cacheClient) StartWarm(ctx context.Context, job cache.WarmJob) error {
	return c.deps.Conn.Call(ctx, "cache/warm", job, nil)
}
