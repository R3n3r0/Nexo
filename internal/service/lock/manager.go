package lock

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// ManagerDeps are the dependencies of the lock manager.
type ManagerDeps struct {
	// Servers are the lock servers of the deployment. None in the
	// single-node modes, whose local locker keeps no inspectable entries:
	// List is then empty and ForceUnlock a no-op.
	Servers []Server
	Logger  *slog.Logger
}

// ManagerOption configures the lock manager.
type ManagerOption func(*manager)

// WithManagerTimeout bounds the calls to each server (default 5s).
func WithManagerTimeout(d time.Duration) ManagerOption {
	return func(m *manager) {
		if d > 0 {
			m.timeout = d
		}
	}
}

// manager aggregates the entries of the lock servers.
type manager struct {
	deps    ManagerDeps
	timeout time.Duration
}

var _ Manager = (*manager)(nil)

// NewManager returns the lock diagnostics over the servers of the deployment.
func NewManager(deps ManagerDeps, opts ...ManagerOption) Manager {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &manager{deps: deps, timeout: defaultCallTimeout}
	for _, o := range opts {
		o(m)
	}
	return m
}

// List implements Manager. Unreachable servers are skipped; the call
// fails only when no server answered.
func (m *manager) List(ctx context.Context) ([]Status, error) {
	if len(m.deps.Servers) == 0 {
		return nil, ctx.Err()
	}
	type key struct{ resource, uid string }
	var (
		mu     sync.Mutex
		byKey  = map[key]*Status{}
		errs   []error
		answer int
	)
	m.each(ctx, func(ctx context.Context, i int, s Server) {
		infos, err := s.List(ctx)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("lock server %d: %w", i, err))
			return
		}
		answer++
		for _, info := range infos {
			k := key{info.Resource, info.UID}
			st, ok := byKey[k]
			if !ok {
				st = &Status{Info: info, Total: len(m.deps.Servers)}
				byKey[k] = st
			}
			st.Servers++
			if info.Refreshed.After(st.Refreshed) {
				st.Refreshed = info.Refreshed
			}
		}
	})
	if answer == 0 {
		return nil, fmt.Errorf("lock: no lock server answered: %w", errors.Join(errs...))
	}
	out := make([]Status, 0, len(byKey))
	for _, st := range byKey {
		out = append(out, *st)
	}
	slices.SortFunc(out, func(a, b Status) int {
		return cmp.Or(cmp.Compare(a.Resource, b.Resource), cmp.Compare(a.UID, b.UID))
	})
	return out, nil
}

// ForceUnlock implements Manager.
func (m *manager) ForceUnlock(ctx context.Context, resources ...string) error {
	if len(resources) == 0 {
		return fmt.Errorf("%w: lock: no resource to unlock", domain.ErrInvalidArgument)
	}
	var (
		mu   sync.Mutex
		errs []error
	)
	m.each(ctx, func(ctx context.Context, i int, s Server) {
		if _, err := s.ForceUnlock(ctx, Args{Resources: resources}); err != nil {
			mu.Lock()
			errs = append(errs, fmt.Errorf("lock server %d: %w", i, err))
			mu.Unlock()
		}
	})
	m.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "lock: resources force-unlocked by an administrator",
		slog.Any("resources", resources), slog.Int("servers", len(m.deps.Servers)), slog.Int("failed", len(errs)))
	return errors.Join(errs...)
}

// each calls fn for every server in parallel, with the per-server timeout.
func (m *manager) each(ctx context.Context, fn func(ctx context.Context, i int, s Server)) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	var wg sync.WaitGroup
	for i, s := range m.deps.Servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(ctx, i, s)
		}()
	}
	wg.Wait()
}
