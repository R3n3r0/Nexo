package rpc

import (
	"context"
	"fmt"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
)

// LockRoutesDeps are the dependencies of the lock routes.
type LockRoutesDeps struct {
	// Server is the in-memory lock server of the node (required).
	Server lock.Server
}

// lockRoutes implements LockRoutes.
type lockRoutes struct {
	deps LockRoutesDeps
}

var _ LockRoutes = (*lockRoutes)(nil)

// NewLockRoutes exposes the lock server of the node to the distributed
// lockers of the other nodes.
func NewLockRoutes(deps LockRoutesDeps, opts ...LockRoutesOption) (LockRoutes, error) {
	_ = apply(opts)
	if deps.Server == nil {
		return nil, fmt.Errorf("%w: rpc: the lock routes need the lock server", domain.ErrInvalidArgument)
	}
	return &lockRoutes{deps: deps}, nil
}

// Register implements Routes.
func (h *lockRoutes) Register(r chi.Router) {
	s := h.deps.Server
	r.Route("/lock", func(r chi.Router) {
		r.Post("/lock", unary(s.Lock))
		r.Post("/rlock", unary(s.RLock))
		r.Post("/unlock", unary(s.Unlock))
		r.Post("/runlock", unary(s.RUnlock))
		r.Post("/refresh", unary(s.Refresh))
		r.Post("/force-unlock", unary(s.ForceUnlock))
		r.Post("/list", unary(func(ctx context.Context, _ empty) ([]lock.Info, error) { return s.List(ctx) }))
	})
}

// LockClientDeps are the dependencies of the lock client.
type LockClientDeps struct {
	// Conn reaches the node of the lock server (required).
	Conn Conn
}

// lockClient implements lock.Server over the lock routes of another node.
type lockClient struct {
	deps LockClientDeps
}

var _ lock.Server = (*lockClient)(nil)

// NewLockClient returns the lock server of another node, for the
// distributed locker.
func NewLockClient(deps LockClientDeps, opts ...LockClientOption) (lock.Server, error) {
	_ = apply(opts)
	if deps.Conn == nil {
		return nil, fmt.Errorf("%w: rpc: the lock client needs a connection", domain.ErrInvalidArgument)
	}
	return &lockClient{deps: deps}, nil
}

func (c *lockClient) call(ctx context.Context, method string, args lock.Args) (bool, error) {
	var ok bool
	if err := c.deps.Conn.Call(ctx, "lock/"+method, args, &ok); err != nil {
		return false, err
	}
	return ok, nil
}

// Lock implements lock.Server.
func (c *lockClient) Lock(ctx context.Context, args lock.Args) (bool, error) {
	return c.call(ctx, "lock", args)
}

// RLock implements lock.Server.
func (c *lockClient) RLock(ctx context.Context, args lock.Args) (bool, error) {
	return c.call(ctx, "rlock", args)
}

// Unlock implements lock.Server.
func (c *lockClient) Unlock(ctx context.Context, args lock.Args) (bool, error) {
	return c.call(ctx, "unlock", args)
}

// RUnlock implements lock.Server.
func (c *lockClient) RUnlock(ctx context.Context, args lock.Args) (bool, error) {
	return c.call(ctx, "runlock", args)
}

// Refresh implements lock.Server.
func (c *lockClient) Refresh(ctx context.Context, args lock.Args) (bool, error) {
	return c.call(ctx, "refresh", args)
}

// ForceUnlock implements lock.Server.
func (c *lockClient) ForceUnlock(ctx context.Context, args lock.Args) (bool, error) {
	return c.call(ctx, "force-unlock", args)
}

// List implements lock.Server.
func (c *lockClient) List(ctx context.Context) ([]lock.Info, error) {
	var infos []lock.Info
	if err := c.deps.Conn.Call(ctx, "lock/list", empty{}, &infos); err != nil {
		return nil, err
	}
	return infos, nil
}
