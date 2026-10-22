package app

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/service/tier"
)

// tierHolder is the tier store of the object layer and the tiers of the
// lifecycle (S-STO-12) once the tier service is composed: the object layer
// is built before it.
type tierHolder struct {
	mu  sync.RWMutex
	svc tier.Service
}

var (
	_ object.TierStore = (*tierHolder)(nil)
	_ ilm.Tiers        = (*tierHolder)(nil)
)

func (h *tierHolder) bind(svc tier.Service) {
	h.mu.Lock()
	h.svc = svc
	h.mu.Unlock()
}

func (h *tierHolder) get() (tier.Service, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.svc == nil {
		return nil, fmt.Errorf("%w: the tiers are not ready", domain.ErrServiceUnavailable)
	}
	return h.svc, nil
}

// PutTiered implements object.TierStore.
func (h *tierHolder) PutTiered(ctx context.Context, name string, r io.Reader, size int64, ref object.TierRef) (string, error) {
	svc, err := h.get()
	if err != nil {
		return "", err
	}
	return svc.PutTiered(ctx, name, r, size, ref)
}

// OpenTiered implements object.TierStore.
func (h *tierHolder) OpenTiered(ctx context.Context, name, obj string, offset, length int64) (io.ReadCloser, error) {
	svc, err := h.get()
	if err != nil {
		return nil, err
	}
	return svc.OpenTiered(ctx, name, obj, offset, length)
}

// DeleteTiered implements object.TierStore.
func (h *tierHolder) DeleteTiered(ctx context.Context, name, obj string) error {
	svc, err := h.get()
	if err != nil {
		return err
	}
	return svc.DeleteTiered(ctx, name, obj)
}

// TierNames implements ilm.Tiers.
func (h *tierHolder) TierNames(ctx context.Context) ([]string, error) {
	svc, err := h.get()
	if err != nil {
		return nil, err
	}
	tiers, err := svc.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(tiers))
	for _, t := range tiers {
		out = append(out, t.Name)
	}
	return out, nil
}

// newTiers builds the remote tiers: their settings in the system
// documents, their secrets sealed with a key of the deployment, the sweep
// of their orphaned objects on the leader.
func newTiers(d composeDeps, objects object.Service, docs sysstore.Store) (tier.Service, error) {
	sealer, err := certs.NewSealer(tier.DeriveKey(d.cfg.RootUser, d.cfg.RootPassword, d.deploymentID))
	if err != nil {
		return nil, err
	}
	deps := tier.Deps{Docs: docs, Sealer: sealer, DeploymentID: d.deploymentID, Objects: objects,
		Clock: d.clock, Logger: d.logger.With("component", "tier")}
	if d.f.leader != nil {
		deps.Leader = d.f.leader
	}
	return tier.New(deps)
}
