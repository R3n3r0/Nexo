package cluster_test

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	clustermock "github.com/r3n3r0/nexo/internal/service/cluster/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

func openDrive(t *testing.T, ep domain.Endpoint) storage.StorageAPI {
	t.Helper()
	ep.IsLocal = true
	d, err := storage.NewLocal(storage.Deps{Logger: discard()}, ep, storage.WithoutFsync())
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}

func TestInfo(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	ep := func(port, disk int) domain.Endpoint {
		return domain.Endpoint{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port), Path: filepath.Join(base, "n"+strconv.Itoa(port), "d"+strconv.Itoa(disk)), DiskIdx: disk}
	}
	nodes := []cluster.Node{
		{ID: "127.0.0.1:1", Local: true, Endpoints: []domain.Endpoint{ep(1, 0), ep(1, 1)}},
		{ID: "127.0.0.1:2", Endpoints: []domain.Endpoint{ep(2, 0), ep(2, 1)}},
		{ID: "127.0.0.1:3", Endpoints: []domain.Endpoint{ep(3, 0), ep(3, 1)}},
	}
	drives := []storage.StorageAPI{openDrive(t, nodes[0].Endpoints[0]), openDrive(t, nodes[0].Endpoints[1])}

	// Peer 3 answers, a few seconds ahead; peer 2 is down.
	p2 := clustermock.NewPeer(t)
	p2.EXPECT().NodeID().Return("127.0.0.1:2").Maybe()
	p2.EXPECT().NodeInfo(mock.Anything).Return(domain.NodeInfo{}, domain.ErrNodeOffline).Maybe()
	p2.EXPECT().Ping(mock.Anything).Return(cluster.PingResult{}, domain.ErrNodeOffline).Maybe()
	p3 := clustermock.NewPeer(t)
	p3.EXPECT().NodeID().Return("127.0.0.1:3").Maybe()
	p3.EXPECT().NodeInfo(mock.Anything).Return(domain.NodeInfo{Endpoint: "127.0.0.1:3", State: domain.StateOnline, Version: "v1.0.0"}, nil).Maybe()
	p3.EXPECT().Ping(mock.Anything).RunAndReturn(func(context.Context) (cluster.PingResult, error) {
		return cluster.PingResult{NodeID: "127.0.0.1:3", Time: clk.Now().Add(5 * time.Second)}, nil
	}).Maybe()

	info := cluster.NewInfo(cluster.InfoDeps{NodeID: "127.0.0.1:1", Nodes: nodes, Drives: drives, Peers: []cluster.Peer{p3, p2}, Clock: clk, Logger: discard()},
		cluster.WithBuildInfo("v1.0.0", "abc"), cluster.WithSkewThreshold(2*time.Second), cluster.WithInfoTimeout(time.Second))
	clk.Advance(time.Hour)

	ping, err := info.Ping(ctx)
	require.NoError(t, err)
	assert.Equal(t, cluster.PingResult{NodeID: "127.0.0.1:1", Version: "v1.0.0", Time: start.Add(time.Hour), Uptime: time.Hour}, ping)

	local, err := info.Local(ctx)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:1", local.Endpoint)
	assert.Equal(t, domain.StateOnline, local.State)
	assert.Equal(t, "abc", local.Commit)
	assert.Equal(t, time.Hour, local.Uptime)
	require.Len(t, local.Disks, 2)
	assert.Equal(t, domain.StateOnline, local.Disks[0].State)
	assert.Equal(t, 1, local.Disks[1].DiskIdx)

	all, err := info.Nodes(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, []string{"127.0.0.1:1", "127.0.0.1:2", "127.0.0.1:3"}, []string{all[0].Endpoint, all[1].Endpoint, all[2].Endpoint}, "topology order")
	assert.Equal(t, domain.StateOffline, all[1].State)
	require.Len(t, all[1].Disks, 2, "the drives of an offline node are reported offline")
	assert.Equal(t, domain.StateOffline, all[1].Disks[0].State)
	assert.Equal(t, domain.StateOnline, all[2].State)

	// A drive gone under the node is reported offline.
	require.NoError(t, drives[1].Close(ctx))
	local, err = info.Local(ctx)
	require.NoError(t, err)
	assert.Equal(t, domain.StateOffline, local.Disks[1].State)
	assert.NotEmpty(t, local.Disks[1].Error)

	// Clock skew (TC-13): the peer 5 s ahead is reported.
	skews, err := info.ClockSkew(ctx)
	require.NoError(t, err)
	require.Len(t, skews, 2)
	assert.Equal(t, "127.0.0.1:3", skews[0].NodeID)
	assert.Equal(t, 5*time.Second, skews[0].Offset)
	assert.True(t, skews[0].Exceeded)
	assert.NotEmpty(t, skews[1].Err)
	assert.False(t, skews[1].Exceeded)
}

// TestInfoSingleNode: without peers the info describes the local node only.
func TestInfoSingleNode(t *testing.T) {
	ctx := context.Background()
	info := cluster.NewInfo(cluster.InfoDeps{Logger: discard()})
	all, err := info.Nodes(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, cluster.LocalNodeID, all[0].Endpoint)
	skews, err := info.ClockSkew(ctx)
	require.NoError(t, err)
	assert.Empty(t, skews)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = info.Ping(canceled)
	assert.ErrorIs(t, err, context.Canceled)
}
