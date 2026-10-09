package admin_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

// TestHealContract runs the healing endpoints against a real SNMD server
// (4 drives), every exchange validated against api/openapi.yaml: a job
// repairing an object a drive lost, its status and the list of the jobs,
// the usage of the scanner and the drives of /info.
func TestHealContract(t *testing.T) {
	srv := testutil.StartServer(t, testutil.Drives(4))
	c := newConsole(t, srv, srv.RootUser, srv.RootPassword)
	c.call(http.MethodPost, "/buckets", map[string]any{"name": "docs"}, http.StatusCreated)
	resp := c.request(http.MethodPost, "/buckets/docs/objects/upload"+q("dir/a.txt"), strings.NewReader(strings.Repeat("x", 300<<10)), http.Header{"Content-Type": {"text/plain"}})
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drives := srv.DriveDirs()
	require.Len(t, drives, 4)
	require.NoError(t, os.RemoveAll(filepath.Join(drives[3], "docs", "dir")))

	job := decodeAs[admin.HealJobResponse](t, c.call(http.MethodPost, "/heal", map[string]any{"bucket": "docs", "recursive": true, "deep": true}, http.StatusAccepted))
	require.NotEmpty(t, job.ID)
	require.Eventually(t, func() bool {
		job = decodeAs[admin.HealJobResponse](t, c.call(http.MethodGet, "/heal/"+job.ID, nil, http.StatusOK))
		return job.Finished
	}, 10*time.Second, 10*time.Millisecond)
	assert.Equal(t, "done", job.State)
	assert.Equal(t, uint64(2), job.Scanned, "the bucket and its object")
	assert.Equal(t, uint64(1), job.Healed)
	require.Len(t, job.Drives, 4)
	assert.Equal(t, uint64(1), job.Drives[3].Missing)
	assert.Equal(t, uint64(1), job.Drives[3].Healed)
	require.Len(t, job.Items, 1)
	assert.Equal(t, "dir/a.txt", job.Items[0].Object)
	_, err := os.Stat(filepath.Join(drives[3], "docs", "dir", "a.txt", "xl.meta"))
	assert.NoError(t, err, "the drive got the object back")

	list := decodeAs[admin.HealJobListResponse](t, c.call(http.MethodGet, "/heal", nil, http.StatusOK))
	require.NotEmpty(t, list.Items)
	assert.Equal(t, job.ID, list.Items[0].ID)
	c.call(http.MethodGet, "/heal/no-such-job", nil, http.StatusNotFound)
	c.call(http.MethodPost, "/heal", map[string]any{"prefix": "x"}, http.StatusBadRequest)

	info := decodeAs[admin.ServerInfoResponse](t, c.call(http.MethodGet, "/info", nil, http.StatusOK))
	assert.Equal(t, "snmd", info.Mode)
	require.Len(t, info.Nodes, 1)
	node := info.Nodes[0]
	assert.True(t, node.Local)
	assert.Nil(t, node.ClockSkew, "the reference clock")
	require.NotNil(t, node.HealQueue, "the background healing of an erasure-coded node")
	// 4 drives with parity 2: D == P, 2 to read and 3 to write.
	require.Len(t, info.Pools, 1)
	assert.Equal(t, 2, info.Pools[0].ReadQuorum)
	assert.Equal(t, 3, info.Pools[0].WriteQuorum)
	assert.Equal(t, []admin.ErasureSetResponse{{Index: 0, Drives: 4, Online: 4, State: "ok"}}, info.Pools[0].ErasureSets)
	c.call(http.MethodGet, "/usage", nil, http.StatusOK)
}

// TestClusterInfoContract: the server information of a cluster (4 nodes
// of 2 drives) lists every node with its clock offset and background
// healing, then a stopped node and its drives offline and the erasure
// set degraded, every exchange validated against api/openapi.yaml.
func TestClusterInfoContract(t *testing.T) {
	srv := testutil.StartCluster(t, 4, 2)
	c := newConsoleAt(t, srv.Nodes[0].ConsoleURL, srv.RootUser, srv.RootPassword)

	info := decodeAs[admin.ServerInfoResponse](t, c.call(http.MethodGet, "/info", nil, http.StatusOK))
	assert.Equal(t, "distributed", info.Mode)
	require.Len(t, info.Nodes, 4)
	for i, n := range info.Nodes {
		assert.Equal(t, i == 0, n.Local, "node %d", i)
		assert.Equal(t, "online", n.State, "node %d", i)
		assert.Len(t, n.Disks, 2, "node %d", i)
		assert.NotNil(t, n.HealQueue, "node %d", i)
		assert.Equal(t, i > 0, n.ClockSkew != nil, "node %d: the peers are measured", i)
	}
	require.Len(t, info.Pools, 1)
	assert.Equal(t, []admin.ErasureSetResponse{{Index: 0, Drives: 8, Online: 8, State: "ok"}}, info.Pools[0].ErasureSets)

	srv.StopNode(t, 3)
	info = decodeAs[admin.ServerInfoResponse](t, c.call(http.MethodGet, "/info", nil, http.StatusOK))
	require.Len(t, info.Nodes, 4)
	down := info.Nodes[3]
	assert.Equal(t, "offline", down.State)
	assert.Nil(t, down.ClockSkew)
	assert.Nil(t, down.HealQueue)
	require.Len(t, down.Disks, 2)
	for _, d := range down.Disks {
		assert.Equal(t, "offline", d.State)
	}
	// 8 drives with parity 4: 4 to read, 5 to write; 6 answer.
	assert.Equal(t, 4, info.Pools[0].ReadQuorum)
	assert.Equal(t, 5, info.Pools[0].WriteQuorum)
	assert.Equal(t, []admin.ErasureSetResponse{{Index: 0, Drives: 8, Online: 6, Offline: 2, State: "degraded"}}, info.Pools[0].ErasureSets)
}
