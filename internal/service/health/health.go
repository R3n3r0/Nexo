package health

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Check is a named readiness check. It returns nil when healthy.
type Check func(ctx context.Context) error

// ClusterFunc computes the cluster status; nil means single node, healthy.
type ClusterFunc func(ctx context.Context) ClusterStatus

// NodesFunc lists the nodes of the deployment with their drives, the
// unreachable ones reported offline (cluster.Info.Nodes).
type NodesFunc func(ctx context.Context) ([]domain.NodeInfo, error)

// QuorumFunc reports whether every erasure set keeps its write quorum and,
// with maintenance, whether it keeps it once one more node is down, with
// the reason when it does not (object.Service.Health).
type QuorumFunc func(ctx context.Context, maintenance bool) (ok bool, reason string)

// Deps are the dependencies of the health service.
type Deps struct {
	// Readiness are the checks consulted by Ready, keyed by name.
	Readiness map[string]Check
	// Cluster computes the cluster status; optional (it takes precedence
	// over Nodes and Quorum).
	Cluster ClusterFunc
	// Nodes and Quorum compute the cluster status of a deployment: the
	// nodes and drives online, the write quorum and the maintenance
	// margin (both required for it).
	Nodes  NodesFunc
	Quorum QuorumFunc
}

// Option configures the service.
type Option func(*service)

// service is the default health implementation.
type service struct {
	mu      sync.RWMutex
	checks  map[string]Check
	cluster ClusterFunc
	nodes   NodesFunc
	quorum  QuorumFunc
}

var _ Service = (*service)(nil)

// New returns a health service.
func New(deps Deps, opts ...Option) Service {
	s := &service{checks: make(map[string]Check, len(deps.Readiness)), cluster: deps.Cluster, nodes: deps.Nodes, quorum: deps.Quorum}
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
	if s.nodes != nil && s.quorum != nil {
		return s.deployment(ctx)
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

// deployment computes the status of the whole deployment: the readiness of
// this node, the nodes and drives that answer, the write quorum of every
// erasure set and whether one more node may go down (maintenance).
func (s *service) deployment(ctx context.Context) ClusterStatus {
	st := ClusterStatus{Status: s.Ready(ctx)}
	if st.Checks == nil {
		st.Checks = map[string]string{}
	}
	nodes, err := s.nodes(ctx)
	if err != nil {
		st.OK, st.Status.Status = false, "not ready"
		st.Checks["nodes"] = fmt.Sprintf("failed: %v", err)
	}
	for _, n := range nodes {
		st.Nodes++
		if n.State != domain.StateOffline {
			st.NodesOnline++
		}
		for _, d := range n.Disks {
			st.Drives++
			if d.State == domain.StateOnline || d.State == domain.StateHealing {
				st.DrivesOnline++
			}
		}
	}
	var reason string
	st.WriteQuorum, reason = s.quorum(ctx, false)
	if !st.WriteQuorum {
		st.OK, st.Status.Status = false, "not ready"
		st.Checks["writeQuorum"] = "failed: " + reason
	}
	st.Maintenance, _ = s.quorum(ctx, true)
	return st
}
