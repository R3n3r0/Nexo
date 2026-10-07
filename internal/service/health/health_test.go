package health

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLiveAlwaysOK(t *testing.T) {
	s := New(Deps{})
	st := s.Live(context.Background())
	assert.True(t, st.OK)
	assert.Equal(t, "ok", st.Status)
}

func TestReadyAggregatesChecks(t *testing.T) {
	ready := true
	s := New(Deps{Readiness: map[string]Check{
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

func TestClusterDefaultsAndCustom(t *testing.T) {
	s := New(Deps{})
	cs := s.Cluster(context.Background())
	assert.True(t, cs.OK)
	assert.Equal(t, 1, cs.Nodes)
	assert.True(t, cs.WriteQuorum)

	custom := New(Deps{Cluster: func(context.Context) ClusterStatus {
		return ClusterStatus{Status: Status{OK: false, Status: "degraded"}, Nodes: 4, NodesOnline: 3}
	}})
	cs = custom.Cluster(context.Background())
	assert.False(t, cs.OK)
	assert.Equal(t, 3, cs.NodesOnline)
}
