package health_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/health"
)

// covers: S-OPS-03
func TestLiveAlwaysOK(t *testing.T) {
	s := health.New(health.Deps{})
	st := s.Live(context.Background())
	assert.True(t, st.OK)
	assert.Equal(t, "ok", st.Status)
}

// covers: S-OPS-03
func TestReadyAggregatesChecks(t *testing.T) {
	ready := true
	s := health.New(health.Deps{Readiness: map[string]health.Check{
		"storage": func(context.Context) error {
			if ready {
				return nil
			}
			return errors.New("quorum lost")
		},
		"always": func(context.Context) error { return nil },
	}})
	st := s.Ready(context.Background())
	assert.True(t, st.OK)
	assert.Equal(t, map[string]string{"storage": "ok", "always": "ok"}, st.Checks)

	ready = false
	st = s.Ready(context.Background())
	assert.False(t, st.OK)
	assert.Equal(t, "not ready", st.Status)
	assert.Equal(t, "failed: quorum lost", st.Checks["storage"])
	assert.Equal(t, "ok", st.Checks["always"])
}

// covers: S-OPS-03
func TestClusterDefaultsAndCustom(t *testing.T) {
	s := health.New(health.Deps{})
	cs := s.Cluster(context.Background())
	assert.True(t, cs.OK)
	assert.Equal(t, 1, cs.Nodes)
	assert.True(t, cs.WriteQuorum)

	custom := health.New(health.Deps{Cluster: func(context.Context) health.ClusterStatus {
		return health.ClusterStatus{Status: health.Status{OK: false, Status: "degraded"}, Nodes: 4, NodesOnline: 3}
	}})
	cs = custom.Cluster(context.Background())
	assert.False(t, cs.OK)
	assert.Equal(t, 3, cs.NodesOnline)
}

// TestClusterOfADeployment: the cluster status counts the nodes and drives
// that answer and reports the write quorum and the maintenance margin of
// the storage (regression: the server answered a fixed single healthy node
// whatever the deployment).
//
// covers: S-OPS-03
func TestClusterOfADeployment(t *testing.T) {
	ctx := context.Background()
	disks := func(states ...string) []domain.DiskInfo {
		out := make([]domain.DiskInfo, 0, len(states))
		for _, s := range states {
			out = append(out, domain.DiskInfo{State: s})
		}
		return out
	}
	nodes := []domain.NodeInfo{
		{State: domain.StateOnline, Disks: disks(domain.StateOnline, domain.StateHealing)},
		{State: domain.StateOnline, Disks: disks(domain.StateOnline, domain.StateOffline)},
		{State: domain.StateOffline, Disks: disks(domain.StateOffline, domain.StateOffline)},
	}
	quorum, margin := true, false
	s := health.New(health.Deps{
		Readiness: map[string]health.Check{"storage": func(context.Context) error { return nil }},
		Nodes:     func(context.Context) ([]domain.NodeInfo, error) { return nodes, nil },
		Quorum: func(_ context.Context, maintenance bool) (bool, string) {
			if maintenance {
				return margin, "one more node down loses the quorum"
			}
			return quorum, "pool 1 set 1 has 3/6 drives online"
		},
	})
	cs := s.Cluster(ctx)
	assert.True(t, cs.OK)
	assert.Equal(t, 3, cs.Nodes)
	assert.Equal(t, 2, cs.NodesOnline)
	assert.Equal(t, 6, cs.Drives)
	assert.Equal(t, 3, cs.DrivesOnline, "a healing drive is online")
	assert.True(t, cs.WriteQuorum)
	assert.False(t, cs.Maintenance)

	quorum, margin = false, false
	cs = s.Cluster(ctx)
	assert.False(t, cs.OK)
	assert.False(t, cs.WriteQuorum)
	assert.Equal(t, "failed: pool 1 set 1 has 3/6 drives online", cs.Checks["writeQuorum"])

	quorum, margin = true, true
	assert.True(t, s.Cluster(ctx).Maintenance)

	failing := health.New(health.Deps{
		Nodes:  func(context.Context) ([]domain.NodeInfo, error) { return nil, errors.New("peers unreachable") },
		Quorum: func(context.Context, bool) (bool, string) { return true, "" },
	})
	cs = failing.Cluster(ctx)
	assert.False(t, cs.OK)
	assert.Contains(t, cs.Checks["nodes"], "peers unreachable")
}
