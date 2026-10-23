package rpc

import (
	"context"
	"fmt"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/update"
)

// UpdateRoutes expose the update agent of the node to the node
// orchestrating an update or a coordinated restart ("/update/...",
// S-OPS-09).
type UpdateRoutes interface{ Routes }

// stageTimeout bounds the staging call: the node downloads the binary.
const stageTimeout = 20 * time.Minute

// UpdateRoutesDeps are the dependencies of the update routes.
type UpdateRoutesDeps struct {
	// Agent is the update agent of the node (required; the composition
	// root may hand a holder answering domain.ErrNotImplemented until the
	// services are composed).
	Agent update.Agent
}

type updateRoutes struct {
	deps UpdateRoutesDeps
}

var _ UpdateRoutes = (*updateRoutes)(nil)

// NewUpdateRoutes exposes the update agent of the node.
func NewUpdateRoutes(deps UpdateRoutesDeps) (UpdateRoutes, error) {
	if deps.Agent == nil {
		return nil, fmt.Errorf("%w: rpc: the update routes need the update agent", domain.ErrInvalidArgument)
	}
	return &updateRoutes{deps: deps}, nil
}

// activateArgs are the arguments of update/activate.
type activateArgs struct {
	SHA256 string
}

// Register implements Routes.
func (h *updateRoutes) Register(r chi.Router) {
	a := h.deps.Agent
	r.Route("/update", func(r chi.Router) {
		r.Post("/state", unary(func(ctx context.Context, _ empty) (update.NodeState, error) {
			return a.State(ctx)
		}))
		r.Post("/stage", unary(func(ctx context.Context, in update.StageInput) (update.NodeState, error) {
			return a.Stage(ctx, in)
		}))
		r.Post("/activate", unary(func(ctx context.Context, in activateArgs) (empty, error) {
			return empty{}, a.Activate(ctx, in.SHA256)
		}))
		r.Post("/restart", unary(func(ctx context.Context, _ empty) (empty, error) {
			return empty{}, a.RestartNode(ctx)
		}))
		r.Post("/stop", unary(func(ctx context.Context, _ empty) (empty, error) {
			return empty{}, a.StopNode(ctx)
		}))
	})
}

// UpdateClientDeps are the dependencies of the update client.
type UpdateClientDeps struct {
	// Conn reaches the node (required).
	Conn Conn
}

// updateClient implements update.Agent over the update routes of another
// node.
type updateClient struct {
	deps UpdateClientDeps
}

var _ update.Agent = (*updateClient)(nil)

// NewUpdateClient returns the update agent of another node.
func NewUpdateClient(deps UpdateClientDeps) (update.Agent, error) {
	if deps.Conn == nil {
		return nil, fmt.Errorf("%w: rpc: the update client needs a connection", domain.ErrInvalidArgument)
	}
	return &updateClient{deps: deps}, nil
}

// NodeID implements update.Agent.
func (c *updateClient) NodeID() string { return c.deps.Conn.Node() }

// State implements update.Agent.
func (c *updateClient) State(ctx context.Context) (update.NodeState, error) {
	var st update.NodeState
	err := c.deps.Conn.Call(ctx, "update/state", empty{}, &st)
	return st, err
}

// Stage implements update.Agent.
func (c *updateClient) Stage(ctx context.Context, in update.StageInput) (update.NodeState, error) {
	var st update.NodeState
	err := c.deps.Conn.Call(ctx, "update/stage", in, &st, WithTimeout(stageTimeout))
	return st, err
}

// Activate implements update.Agent.
func (c *updateClient) Activate(ctx context.Context, sha256 string) error {
	return c.deps.Conn.Call(ctx, "update/activate", activateArgs{SHA256: sha256}, nil)
}

// RestartNode implements update.Agent.
func (c *updateClient) RestartNode(ctx context.Context) error {
	return c.deps.Conn.Call(ctx, "update/restart", empty{}, nil)
}

// StopNode implements update.Agent.
func (c *updateClient) StopNode(ctx context.Context) error {
	return c.deps.Conn.Call(ctx, "update/stop", empty{}, nil)
}
