package app

import (
	"context"
	"fmt"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/service/update"
)

// updateHolder is the update agent served to the peers (S-OPS-09) once
// the update service is composed: the internode routes are built with the
// foundation, before it.
type updateHolder struct {
	mu  sync.RWMutex
	svc update.Service
}

var _ update.Agent = (*updateHolder)(nil)

func (h *updateHolder) bind(svc update.Service) {
	h.mu.Lock()
	h.svc = svc
	h.mu.Unlock()
}

func (h *updateHolder) get() (update.Service, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.svc == nil {
		return nil, fmt.Errorf("%w: the update agent is not ready", domain.ErrServiceUnavailable)
	}
	return h.svc, nil
}

// NodeID implements update.Agent.
func (h *updateHolder) NodeID() string {
	if svc, err := h.get(); err == nil {
		return svc.NodeID()
	}
	return ""
}

// State implements update.Agent.
func (h *updateHolder) State(ctx context.Context) (update.NodeState, error) {
	svc, err := h.get()
	if err != nil {
		return update.NodeState{}, err
	}
	return svc.State(ctx)
}

// Stage implements update.Agent.
func (h *updateHolder) Stage(ctx context.Context, in update.StageInput) (update.NodeState, error) {
	svc, err := h.get()
	if err != nil {
		return update.NodeState{}, err
	}
	return svc.Stage(ctx, in)
}

// Activate implements update.Agent.
func (h *updateHolder) Activate(ctx context.Context, sha256 string) error {
	svc, err := h.get()
	if err != nil {
		return err
	}
	return svc.Activate(ctx, sha256)
}

// RestartNode implements update.Agent.
func (h *updateHolder) RestartNode(ctx context.Context) error {
	svc, err := h.get()
	if err != nil {
		return err
	}
	return svc.RestartNode(ctx)
}

// StopNode implements update.Agent.
func (h *updateHolder) StopNode(ctx context.Context) error {
	svc, err := h.get()
	if err != nil {
		return err
	}
	return svc.StopNode(ctx)
}

// newUpdates builds the update service of the node: the agents of the
// peers, the control of this process, the health of the storage that
// paces a rolling job, the last job in the system documents.
func newUpdates(d composeDeps, objects object.Service, docs sysstore.Store, proc update.Process) (update.Service, error) {
	return update.New(update.Deps{NodeID: d.f.nodeID, Peers: d.f.updatePeers, Process: proc, Health: objects, Docs: docs,
		Clock: d.clock, Logger: d.logger.With("component", "update")})
}

// process returns the control of the process of the application.
func (d composeDeps) process() processActions {
	return processActions{running: d.running, signal: signalSelf, restart: d.restart, exe: d.exe, exec: d.exec}
}
