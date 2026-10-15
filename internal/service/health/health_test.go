package health_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

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
