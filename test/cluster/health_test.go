//go:build cluster

package cluster_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
)

// clusterHealth is the answer of /nexo/health/cluster.
type clusterHealth struct {
	OK           bool              `json:"ok"`
	Checks       map[string]string `json:"checks"`
	Nodes        int               `json:"nodes"`
	NodesOnline  int               `json:"nodesOnline"`
	Drives       int               `json:"drives"`
	DrivesOnline int               `json:"drivesOnline"`
	WriteQuorum  bool              `json:"writeQuorum"`
	Maintenance  bool              `json:"maintenance"`
}

// clusterHealthOf reads /nexo/health/cluster of node n.
func clusterHealthOf(t *testing.T, n *node) (int, clusterHealth) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+n.S3+domain.HealthClusterPath, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var h clusterHealth
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&h))
	return resp.StatusCode, h
}

// TestClusterHealth: /nexo/health/cluster of any node reports the nodes
// and drives of the deployment that answer, the write quorum of the sets
// and the maintenance margin (whether one more node may go down), as the
// rolling upgrade and the drive replacement of the user guide check them
// (regression: a fixed healthy single node with one drive was reported
// whatever the deployment).
//
// covers: S-OPS-03, S-STO-06
func TestClusterHealth(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 4)
	c.start()

	status, h := clusterHealthOf(t, c.node(2))
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, clusterHealth{OK: true, Checks: map[string]string{"storage": "ok"}, Nodes: 4, NodesOnline: 4, Drives: 8, DrivesOnline: 8,
		WriteQuorum: true, Maintenance: true}, h, "every node up: one may be stopped")

	// EC 4+4 (write quorum 5) over 4 nodes of 2 drives: without a node the
	// sets keep their write quorum, but no other node may go down.
	c.node(4).kill()
	eventually(t, 30*time.Second, "node 4 reported offline", func() bool {
		_, h = clusterHealthOf(t, c.node(1))
		return h.NodesOnline == 3
	})
	assert.Equal(t, clusterHealth{OK: true, Checks: map[string]string{"storage": "ok"}, Nodes: 4, NodesOnline: 3, Drives: 8, DrivesOnline: 6,
		WriteQuorum: true}, h)

	c.node(3).kill()
	eventually(t, 30*time.Second, "node 3 reported offline", func() bool {
		_, h = clusterHealthOf(t, c.node(1))
		return h.NodesOnline == 2
	})
	status, h = clusterHealthOf(t, c.node(1))
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.False(t, h.OK)
	assert.Equal(t, 4, h.Nodes)
	assert.Equal(t, 4, h.DrivesOnline)
	assert.False(t, h.WriteQuorum, "4 of 8 drives online, write quorum 5")
	assert.False(t, h.Maintenance)
	assert.Contains(t, h.Checks["writeQuorum"], "4/8 drives online")
}
