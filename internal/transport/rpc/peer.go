package rpc

import (
	"context"
	"fmt"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
)

// PeerRoutesDeps are the dependencies of the peer routes.
type PeerRoutesDeps struct {
	// Notifier delivers the invalidations of the peers to the local
	// subscribers (required).
	Notifier cluster.Notifier
	// Info answers the pings and describes the node (required).
	Info cluster.Info
}

// peerRoutes implements PeerRoutes.
type peerRoutes struct {
	deps PeerRoutesDeps
}

var _ PeerRoutes = (*peerRoutes)(nil)

// NewPeerRoutes exposes the peer service of the node.
func NewPeerRoutes(deps PeerRoutesDeps, opts ...PeerRoutesOption) (PeerRoutes, error) {
	_ = apply(opts)
	if deps.Notifier == nil || deps.Info == nil {
		return nil, fmt.Errorf("%w: rpc: the peer routes need the notifier and the info service", domain.ErrInvalidArgument)
	}
	return &peerRoutes{deps: deps}, nil
}

// Register implements Routes.
func (h *peerRoutes) Register(r chi.Router) {
	r.Route("/peer", func(r chi.Router) {
		r.Post("/ping", unary(func(ctx context.Context, _ empty) (cluster.PingResult, error) {
			return h.deps.Info.Ping(ctx)
		}))
		r.Post("/notify", unary(func(ctx context.Context, e cluster.Event) (empty, error) {
			return empty{}, h.deps.Notifier.Deliver(ctx, e)
		}))
		r.Post("/node-info", unary(func(ctx context.Context, _ empty) (domain.NodeInfo, error) {
			return h.deps.Info.Local(ctx)
		}))
	})
}

// PeerClientDeps are the dependencies of the peer client.
type PeerClientDeps struct {
	// Conn reaches the node (required).
	Conn Conn
}

// peerClient implements cluster.Peer over the peer routes of another node.
type peerClient struct {
	deps PeerClientDeps
}

var _ cluster.Peer = (*peerClient)(nil)

// NewPeerClient returns the peer of another node.
func NewPeerClient(deps PeerClientDeps, opts ...PeerClientOption) (cluster.Peer, error) {
	_ = apply(opts)
	if deps.Conn == nil {
		return nil, fmt.Errorf("%w: rpc: the peer client needs a connection", domain.ErrInvalidArgument)
	}
	return &peerClient{deps: deps}, nil
}

// NodeID implements cluster.Peer.
func (c *peerClient) NodeID() string { return c.deps.Conn.Node() }

// IsOnline implements cluster.Peer.
func (c *peerClient) IsOnline() bool { return c.deps.Conn.IsOnline() }

// Ping implements cluster.Peer.
func (c *peerClient) Ping(ctx context.Context) (cluster.PingResult, error) {
	var res cluster.PingResult
	err := c.deps.Conn.Call(ctx, "peer/ping", empty{}, &res)
	return res, err
}

// Notify implements cluster.Peer.
func (c *peerClient) Notify(ctx context.Context, e cluster.Event) error {
	return c.deps.Conn.Call(ctx, "peer/notify", e, nil)
}

// NodeInfo implements cluster.Peer.
func (c *peerClient) NodeInfo(ctx context.Context) (domain.NodeInfo, error) {
	var ni domain.NodeInfo
	err := c.deps.Conn.Call(ctx, "peer/node-info", empty{}, &ni)
	return ni, err
}

// BootstrapRoutesDeps are the dependencies of the bootstrap routes.
type BootstrapRoutesDeps struct {
	// Bootstrapper announces the configuration of the node (required).
	Bootstrapper cluster.Bootstrapper
}

// bootstrapRoutes implements BootstrapRoutes.
type bootstrapRoutes struct {
	deps BootstrapRoutesDeps
}

var _ BootstrapRoutes = (*bootstrapRoutes)(nil)

// NewBootstrapRoutes exposes the startup configuration of the node.
func NewBootstrapRoutes(deps BootstrapRoutesDeps, opts ...BootstrapRoutesOption) (BootstrapRoutes, error) {
	_ = apply(opts)
	if deps.Bootstrapper == nil {
		return nil, fmt.Errorf("%w: rpc: the bootstrap routes need the bootstrapper", domain.ErrInvalidArgument)
	}
	return &bootstrapRoutes{deps: deps}, nil
}

// Register implements Routes.
func (h *bootstrapRoutes) Register(r chi.Router) {
	r.Route("/bootstrap", func(r chi.Router) {
		r.Post("/config", unary(func(ctx context.Context, _ empty) (cluster.NodeConfig, error) {
			return h.deps.Bootstrapper.LocalConfig(ctx)
		}))
	})
}

// BootstrapClientDeps are the dependencies of the bootstrap client.
type BootstrapClientDeps struct {
	// Conn reaches the node (required).
	Conn Conn
}

// bootstrapClient implements cluster.BootstrapPeer.
type bootstrapClient struct {
	deps BootstrapClientDeps
}

var _ cluster.BootstrapPeer = (*bootstrapClient)(nil)

// NewBootstrapClient returns the bootstrap service of another node.
func NewBootstrapClient(deps BootstrapClientDeps, opts ...BootstrapClientOption) (cluster.BootstrapPeer, error) {
	_ = apply(opts)
	if deps.Conn == nil {
		return nil, fmt.Errorf("%w: rpc: the bootstrap client needs a connection", domain.ErrInvalidArgument)
	}
	return &bootstrapClient{deps: deps}, nil
}

// NodeID implements cluster.BootstrapPeer.
func (c *bootstrapClient) NodeID() string { return c.deps.Conn.Node() }

// Config implements cluster.BootstrapPeer.
func (c *bootstrapClient) Config(ctx context.Context) (cluster.NodeConfig, error) {
	var cfg cluster.NodeConfig
	err := c.deps.Conn.Call(ctx, "bootstrap/config", empty{}, &cfg)
	return cfg, err
}
