package cluster_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	clustermock "github.com/r3n3r0/nexo/internal/service/cluster/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// deployment is a 4 nodes x 2 drives topology on 127.0.0.1, whose drives
// are all opened in this process: each test plays one node at a time
// (its layout marks its own drives local) over the same directories.
type deployment struct {
	base  string
	ports []int
	args  []string
}

func newDeployment(t *testing.T) *deployment {
	t.Helper()
	d := &deployment{base: t.TempDir(), ports: []int{19101, 19102, 19103, 19104}}
	for n, port := range d.ports {
		for disk := 1; disk <= 2; disk++ {
			d.args = append(d.args, fmt.Sprintf("http://127.0.0.1:%d%s/n%d/d%d", port, filepath.ToSlash(d.base), n+1, disk))
		}
	}
	return d
}

// layout parses the arguments as node n (0-based) does.
func (d *deployment) layout(t *testing.T, n int, args ...string) *topology.Layout {
	t.Helper()
	if len(args) == 0 {
		args = d.args
	}
	topo := topology.New(topology.Deps{Logger: discard()}, topology.WithLocalPort(d.ports[n]), topology.WithDefaultPort(d.ports[n]))
	l, err := topo.Parse(context.Background(), args)
	require.NoError(t, err)
	return l
}

// drives opens every drive of the layout.
func (d *deployment) drives(t *testing.T, l *topology.Layout) []storage.StorageAPI {
	t.Helper()
	var out []storage.StorageAPI
	for _, ep := range l.Endpoints() {
		out = append(out, openDrive(t, ep))
	}
	return out
}

// peerState is the answer of a mocked bootstrap peer.
type peerState struct {
	online  atomic.Bool
	hash    atomic.Value
	version atomic.Value
}

// peers returns the bootstrap peers of node n, answering like nodes
// running the layout l unless told otherwise.
func (d *deployment) peers(t *testing.T, n int, l *topology.Layout) ([]cluster.BootstrapPeer, []*peerState) {
	t.Helper()
	var out []cluster.BootstrapPeer
	var states []*peerState
	for i, port := range d.ports {
		if i == n {
			continue
		}
		st := &peerState{}
		st.online.Store(true)
		st.hash.Store(l.Hash())
		st.version.Store("v1.2.0")
		id := fmt.Sprintf("127.0.0.1:%d", port)
		p := clustermock.NewBootstrapPeer(t)
		p.EXPECT().NodeID().Return(id).Maybe()
		p.EXPECT().Config(mock.Anything).RunAndReturn(func(context.Context) (cluster.NodeConfig, error) {
			if !st.online.Load() {
				return cluster.NodeConfig{}, domain.ErrNodeOffline
			}
			return cluster.NodeConfig{NodeID: id, TopologyHash: st.hash.Load().(string), Mode: l.Mode,
				Version: st.version.Load().(string), Time: time.Now()}, nil
		}).Maybe()
		out = append(out, p)
		states = append(states, st)
	}
	return out, states
}

func (d *deployment) bootstrapper(t *testing.T, n int, drives []storage.StorageAPI, opts ...cluster.BootstrapOption) (cluster.Bootstrapper, []*peerState) {
	t.Helper()
	l := d.layout(t, n)
	peers, states := d.peers(t, n, l)
	base := []cluster.BootstrapOption{cluster.WithBootstrapBackoff(10*time.Millisecond, 50*time.Millisecond), cluster.WithBootstrapVersion("v1.0.0")}
	topo := topology.New(topology.Deps{Logger: discard()})
	b, err := cluster.NewBootstrapper(cluster.BootstrapDeps{Layout: l, Topology: topo, Drives: drives, Peers: peers, Logger: discard()}, append(base, opts...)...)
	require.NoError(t, err)
	return b, states
}

func formatOf(t *testing.T, d storage.StorageAPI) *topology.FormatV1 {
	t.Helper()
	data, err := d.ReadAll(context.Background(), domain.SystemBucket, storage.FormatFile)
	require.NoError(t, err)
	doc, err := topology.UnmarshalFormat(data)
	require.NoError(t, err)
	return doc
}

// TestBootstrapFreshDeployment: the node of the first endpoint formats
// every drive; another node then finds the same deployment.
func TestBootstrapFreshDeployment(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t)
	drives := d.drives(t, d.layout(t, 0))
	first, _ := d.bootstrapper(t, 0, drives)
	cfg, err := first.LocalConfig(ctx)
	require.NoError(t, err)
	assert.Empty(t, cfg.DeploymentID)

	res, err := first.Bootstrap(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, res.DeploymentID)
	assert.Empty(t, res.Offline)
	assert.Empty(t, res.Healing)
	ids := map[string]bool{}
	for i, drive := range drives {
		doc := formatOf(t, drive)
		assert.Equal(t, res.DeploymentID, doc.ID)
		set, disk, err := doc.Position()
		require.NoError(t, err)
		assert.Equal(t, [2]int{0, i}, [2]int{set, disk})
		ids[doc.Erasure.This] = true
	}
	assert.Len(t, ids, 8, "one drive id per drive")
	cfg, err = first.LocalConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, res.DeploymentID, cfg.DeploymentID)
	assert.Equal(t, domain.ModeDistributed, cfg.Mode)

	third, _ := d.bootstrapper(t, 2, d.drives(t, d.layout(t, 2)))
	res3, err := third.Bootstrap(ctx)
	require.NoError(t, err)
	assert.Equal(t, res.DeploymentID, res3.DeploymentID)
}

// TestBootstrapBindsTheDeployment: the bootstrap binds the internode
// authentication to the deployment as soon as it knows its id: the node
// formatting a fresh deployment before it writes any format (its peers
// then see its tokens carry the id of the formats it writes), the others
// once they read the formats.
func TestBootstrapBindsTheDeployment(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t)
	drives := d.drives(t, d.layout(t, 0))
	formatted := func() int {
		n := 0
		for _, drive := range drives {
			if _, err := drive.ReadAll(ctx, domain.SystemBucket, storage.FormatFile); err == nil {
				n++
			}
		}
		return n
	}
	var bound []string
	var formatsAtBind []int
	binder := clustermock.NewDeploymentBinder(t)
	binder.EXPECT().SetDeploymentID(mock.Anything, mock.Anything).Run(func(_ context.Context, id string) {
		bound = append(bound, id)
		formatsAtBind = append(formatsAtBind, formatted())
	})
	newNode := func(n int, drives []storage.StorageAPI) cluster.Bootstrapper {
		l := d.layout(t, n)
		peers, _ := d.peers(t, n, l)
		b, err := cluster.NewBootstrapper(cluster.BootstrapDeps{Layout: l, Topology: topology.New(topology.Deps{Logger: discard()}),
			Drives: drives, Peers: peers, Binder: binder, Logger: discard()},
			cluster.WithBootstrapBackoff(10*time.Millisecond, 50*time.Millisecond), cluster.WithBootstrapVersion("v1.0.0"))
		require.NoError(t, err)
		return b
	}
	res, err := newNode(0, drives).Bootstrap(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, bound)
	assert.Equal(t, res.DeploymentID, bound[0])
	assert.Zero(t, formatsAtBind[0], "bound before any format is written")

	bound = nil
	res3, err := newNode(2, d.drives(t, d.layout(t, 2))).Bootstrap(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, bound)
	assert.Equal(t, res.DeploymentID, res3.DeploymentID)
	for _, id := range bound {
		assert.Equal(t, res.DeploymentID, id, "a node reading the formats binds to their deployment")
	}
}

// TestBootstrapWaitsForTheFirstNode: the other nodes do not format a fresh
// deployment, they wait for the node of the first endpoint.
func TestBootstrapWaitsForTheFirstNode(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t)
	second, _ := d.bootstrapper(t, 1, d.drives(t, d.layout(t, 1)))
	type outcome struct {
		res cluster.BootstrapResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := second.Bootstrap(ctx)
		done <- outcome{res, err}
	}()
	select {
	case <-done:
		t.Fatal("a node other than the first formatted the deployment")
	case <-time.After(200 * time.Millisecond):
	}
	first, _ := d.bootstrapper(t, 0, d.drives(t, d.layout(t, 0)))
	res, err := first.Bootstrap(ctx)
	require.NoError(t, err)
	select {
	case o := <-done:
		require.NoError(t, o.err)
		assert.Equal(t, res.DeploymentID, o.res.DeploymentID)
		assert.Empty(t, o.res.Healing, "drives formatted by the first node are not healed")
	case <-time.After(10 * time.Second):
		t.Fatal("the second node did not complete its bootstrap")
	}
}

// TestBootstrapReplacementDrive: an empty local drive in an existing
// deployment receives the format of its position and is marked for
// healing; the drives of other nodes are left to their node.
func TestBootstrapReplacementDrive(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t)
	first, _ := d.bootstrapper(t, 0, d.drives(t, d.layout(t, 0)))
	res, err := first.Bootstrap(ctx)
	require.NoError(t, err)

	l := d.layout(t, 1)
	drives := d.drives(t, l)
	// One drive of node 2 (this node) and one of another node are
	// replaced.
	local, remote := -1, -1
	for i, ep := range l.Endpoints() {
		switch {
		case ep.IsLocal && local < 0:
			local = i
		case !ep.IsLocal && remote < 0:
			remote = i
		}
	}
	before := formatOf(t, drives[local])
	// The deployment holds data: a bucket on every drive.
	for _, dr := range drives {
		require.NoError(t, dr.MakeVol(ctx, "photos"))
	}
	for _, i := range []int{local, remote} {
		require.NoError(t, drives[i].Delete(ctx, domain.SystemBucket, storage.FormatFile, storage.DeleteOptions{}))
		require.NoError(t, drives[i].DeleteVol(ctx, "photos", true))
	}
	second, _ := d.bootstrapper(t, 1, drives)
	res2, err := second.Bootstrap(ctx)
	require.NoError(t, err)
	assert.Equal(t, res.DeploymentID, res2.DeploymentID)
	require.Len(t, res2.Healing, 1)
	assert.Equal(t, l.Endpoints()[local].String(), res2.Healing[0].String())
	after := formatOf(t, drives[local])
	assert.Equal(t, before.Erasure.This, after.Erasure.This, "the drive takes the id of its position")
	tracker := drives[local].Healing()
	require.NotNil(t, tracker, "the drive is marked for healing")
	assert.Equal(t, after.Erasure.This, tracker.ID)
	assert.Equal(t, local, tracker.DiskIndex)
	require.Len(t, res2.Offline, 1, "the drive of the other node waits for its node")
	_, err = drives[remote].ReadAll(ctx, domain.SystemBucket, storage.FormatFile)
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
}

// TestBootstrapUnformattedDriveWithoutData: a drive of a fresh deployment
// that its first format missed (a remote write that failed under load) is
// formatted by its node without being marked for healing: its set holds
// no data, there is nothing to heal.
func TestBootstrapUnformattedDriveWithoutData(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t)
	first, _ := d.bootstrapper(t, 0, d.drives(t, d.layout(t, 0)))
	_, err := first.Bootstrap(ctx)
	require.NoError(t, err)
	l := d.layout(t, 1)
	drives := d.drives(t, l)
	local := -1
	for i, ep := range l.Endpoints() {
		if ep.IsLocal {
			local = i
			break
		}
	}
	require.NoError(t, drives[local].Delete(ctx, domain.SystemBucket, storage.FormatFile, storage.DeleteOptions{}))
	second, _ := d.bootstrapper(t, 1, drives)
	res, err := second.Bootstrap(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Healing, "nothing to heal in a deployment without data")
	assert.Nil(t, drives[local].Healing())
	formatOf(t, drives[local])
}

// TestBootstrapTopologyMismatch (TC-02): a node whose topology differs
// from the majority refuses to start; the majority goes on without it.
func TestBootstrapTopologyMismatch(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t)
	odd, states := d.bootstrapper(t, 3, d.drives(t, d.layout(t, 3)))
	for _, st := range states {
		st.hash.Store("another-topology")
	}
	_, err := odd.Bootstrap(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, cluster.ErrTopologyMismatch)
	assert.ErrorIs(t, err, domain.ErrInvalidLayout)
	assert.Contains(t, err.Error(), "127.0.0.1:19101")

	first, states := d.bootstrapper(t, 0, d.drives(t, d.layout(t, 0)))
	states[2].hash.Store("another-topology")
	_, err = first.Bootstrap(ctx)
	require.NoError(t, err, "3 of 4 nodes agree")
}

func TestBootstrapIncompatibleVersion(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t)
	b, states := d.bootstrapper(t, 0, d.drives(t, d.layout(t, 0)))
	for _, st := range states {
		st.version.Store("v2.0.0")
	}
	_, err := b.Bootstrap(ctx)
	assert.ErrorIs(t, err, cluster.ErrIncompatibleVersion)
}

// TestBootstrapWaitsForPeers: without a quorum of verified nodes the
// bootstrap waits (until its context ends), and proceeds when they come.
func TestBootstrapWaitsForPeers(t *testing.T) {
	d := newDeployment(t)
	b, states := d.bootstrapper(t, 0, d.drives(t, d.layout(t, 0)))
	for _, st := range states {
		st.online.Store(false)
	}
	short, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := b.Bootstrap(short)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	go func() {
		time.Sleep(100 * time.Millisecond)
		states[0].online.Store(true)
		states[2].online.Store(true)
	}()
	res, err := b.Bootstrap(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, res.DeploymentID)
}

// TestBootstrapDriveQuorum: a fresh deployment is formatted only with a
// write quorum of drives in every set; an existing one starts with a read
// quorum and reports the missing drives.
func TestBootstrapDriveQuorum(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t)
	l := d.layout(t, 0)
	drives := d.drives(t, l)
	// Nodes 3 and 4 are down: 4 of 8 drives, the write quorum is 5.
	down := d.drives(t, l)
	partial := append(append([]storage.StorageAPI{}, drives[:4]...), down[4:]...)
	for _, dr := range down[4:] {
		require.NoError(t, dr.Close(ctx))
	}
	b, _ := d.bootstrapper(t, 0, partial)
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, err := b.Bootstrap(short)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "a fresh deployment waits for the write quorum")
	_, err = drives[0].ReadAll(ctx, domain.SystemBucket, storage.FormatFile)
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "nothing was formatted")

	// Formatted with every drive, the deployment restarts with half of them.
	full, _ := d.bootstrapper(t, 0, drives)
	res, err := full.Bootstrap(ctx)
	require.NoError(t, err)
	again, _ := d.bootstrapper(t, 0, partial)
	res2, err := again.Bootstrap(ctx)
	require.NoError(t, err)
	assert.Equal(t, res.DeploymentID, res2.DeploymentID)
	assert.Len(t, res2.Offline, 4)
}

// TestBootstrapFormatMismatch: drives of another deployment, or moved to
// another position, stop the bootstrap with a clear error.
func TestBootstrapFormatMismatch(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t)
	l := d.layout(t, 0)
	drives := d.drives(t, l)
	b, _ := d.bootstrapper(t, 0, drives)
	_, err := b.Bootstrap(ctx)
	require.NoError(t, err)

	// Swap the documents of two drives.
	a, err := drives[0].ReadAll(ctx, domain.SystemBucket, storage.FormatFile)
	require.NoError(t, err)
	c, err := drives[1].ReadAll(ctx, domain.SystemBucket, storage.FormatFile)
	require.NoError(t, err)
	require.NoError(t, drives[0].WriteAll(ctx, domain.SystemBucket, storage.FormatFile, c))
	require.NoError(t, drives[1].WriteAll(ctx, domain.SystemBucket, storage.FormatFile, a))
	b2, _ := d.bootstrapper(t, 0, drives)
	_, err = b2.Bootstrap(ctx)
	assert.ErrorIs(t, err, domain.ErrFormatMismatch)

	// A drive of another deployment.
	other := topology.FormatV1{Version: topology.FormatVersion, ID: "other", Mode: topology.FormatModeErasure,
		Erasure: topology.FormatErasure{This: "x1", Sets: [][]string{{"x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8"}}, DistributionAlgo: topology.DistributionAlgoV1}}
	raw, err := other.Marshal()
	require.NoError(t, err)
	require.NoError(t, drives[0].WriteAll(ctx, domain.SystemBucket, storage.FormatFile, raw))
	require.NoError(t, drives[1].WriteAll(ctx, domain.SystemBucket, storage.FormatFile, c))
	b3, _ := d.bootstrapper(t, 0, drives)
	_, err = b3.Bootstrap(ctx)
	assert.ErrorIs(t, err, domain.ErrFormatMismatch)
}

func TestNewBootstrapperValidation(t *testing.T) {
	d := newDeployment(t)
	l := d.layout(t, 0)
	_, err := cluster.NewBootstrapper(cluster.BootstrapDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	topo := topology.New(topology.Deps{Logger: discard()})
	_, err = cluster.NewBootstrapper(cluster.BootstrapDeps{Layout: l, Drives: d.drives(t, l)})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "the topology service is injected, never built here")
	_, err = cluster.NewBootstrapper(cluster.BootstrapDeps{Layout: l, Topology: topo, Drives: d.drives(t, l)[:3]})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "every endpoint needs a drive")
	_, err = cluster.NewLocalBootstrapper(cluster.BootstrapDeps{Layout: l, Topology: topo, Drives: d.drives(t, l)})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "the formatter is injected, never built here")
}

// TestLocalBootstrapper: a single-node deployment formats its drives with
// the storage formatter.
func TestLocalBootstrapper(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	topo := topology.New(topology.Deps{Logger: discard()})
	l, err := topo.Parse(ctx, []string{base + "/d{1...4}"})
	require.NoError(t, err)
	var drives []storage.StorageAPI
	for _, ep := range l.Endpoints() {
		drives = append(drives, openDrive(t, ep))
	}
	formatter := storage.NewFormatter(storage.FormatterDeps{Topology: topo, Logger: discard()})
	b, err := cluster.NewLocalBootstrapper(cluster.BootstrapDeps{Layout: l, Topology: topo, Formatter: formatter, Drives: drives, Logger: discard()})
	require.NoError(t, err)
	res, err := b.Bootstrap(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, res.DeploymentID)
	cfg, err := b.LocalConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, res.DeploymentID, cfg.DeploymentID)
	assert.Equal(t, cluster.LocalNodeID, cfg.NodeID)
	assert.Equal(t, domain.ModeSNMD, cfg.Mode)
	assert.Equal(t, res.DeploymentID, formatOf(t, drives[2]).ID)
	nodes := cluster.NodesOf(l)
	require.Len(t, nodes, 1)
	assert.Equal(t, cluster.LocalNodeID, nodes[0].ID)
	assert.True(t, nodes[0].Local)
}
