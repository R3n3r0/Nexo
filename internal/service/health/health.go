package health

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Check is a named readiness check. It returns nil when healthy.
type Check func(ctx context.Context) error

// ClusterFunc computes the cluster status; nil means single node, healthy.
type ClusterFunc func(ctx context.Context) ClusterStatus

// Deps are the dependencies of the health service.
type Deps struct {
	// Readiness are the checks consulted by Ready, keyed by name.
	Readiness map[string]Check
	// Cluster computes the cluster status; optional.
	Cluster ClusterFunc
}

// Option configures the service.
type Option func(*service)

// service is the default health implementation.
type service struct {
	mu      sync.RWMutex
	checks  map[string]Check
	cluster ClusterFunc
}

var _ Service = (*service)(nil)

// New returns a health service.
func New(deps Deps, opts ...Option) Service {
	s := &service{checks: make(map[string]Check, len(deps.Readiness)), cluster: deps.Cluster}
	for k, v := range deps.Readiness {
		s.checks[k] = v
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Live implements Service.
func (s *service) Live(context.Context) Status {
	return Status{OK: true, Status: "ok"}
}

// Ready implements Service.
func (s *service) Ready(ctx context.Context) Status {
	s.mu.RLock()
	names := make([]string, 0, len(s.checks))
	for n := range s.checks {
		names = append(names, n)
	}
	checks := make(map[string]Check, len(s.checks))
	for k, v := range s.checks {
		checks[k] = v
	}
	s.mu.RUnlock()
	sort.Strings(names)

	st := Status{OK: true, Status: "ok", Checks: make(map[string]string, len(names))}
	for _, n := range names {
		if err := checks[n](ctx); err != nil {
			st.OK = false
			st.Status = "not ready"
			st.Checks[n] = fmt.Sprintf("failed: %v", err)
			continue
		}
		st.Checks[n] = "ok"
	}
	return st
}

// Cluster implements Service.
func (s *service) Cluster(ctx context.Context) ClusterStatus {
	s.mu.RLock()
	fn := s.cluster
	s.mu.RUnlock()
	if fn != nil {
		return fn(ctx)
	}
	return ClusterStatus{
		Status:       s.Ready(ctx),
		Nodes:        1,
		NodesOnline:  1,
		Drives:       1,
		DrivesOnline: 1,
		WriteQuorum:  true,
	}
}
