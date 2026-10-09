package topology_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

func TestLayout_HashStable(t *testing.T) {
	t.Parallel()
	// Golden value: changing the canonical form is a wire-compatibility change
	// for the bootstrap check between nodes.
	const goldenHash = "bf8a6310d0d7f0f2312ae5ae1ea24244a57a7ffd831db00dcc86cf1989f82498"
	layout := parseLayout(t, "http://node{1...4}:9000/data{1...4}")
	h := layout.Hash()
	assert.Len(t, h, 64)
	assert.Equal(t, h, parseLayout(t, "http://node{1...4}:9000/data{1...4}").Hash())
	if h != goldenHash {
		t.Logf("canonical form:\n%s", topology.Canonical(layout))
		t.Logf("hash %s (update goldenHash if the canonical form changed on purpose)", h)
	}
	assert.Equal(t, goldenHash, h)
}

func TestLayout_HashIndependentOfLocalNode(t *testing.T) {
	t.Parallel()
	args := []string{"http://node{1...4}:9000/data{1...4}"}
	ctx := context.Background()
	a, err := newTestService(t).Parse(ctx, args)
	require.NoError(t, err)
	b, err := topology.New(topology.Deps{Hosts: newResolver(t, nodeHost(3)), Logger: quietLogger()}).Parse(ctx, args)
	require.NoError(t, err)
	assert.Equal(t, a.Hash(), b.Hash())
	assert.NotEqual(t, a.LocalEndpoints(), b.LocalEndpoints())
}

func TestLayout_HashChanges(t *testing.T) {
	t.Parallel()
	base := parseLayout(t, "/data{1...4}").Hash()
	assert.NotEqual(t, base, parseLayout(t, "/data{1...8}").Hash(), "drive count")
	assert.NotEqual(t, base, parseLayout(t, "/disk{1...4}").Hash(), "paths")
	assert.NotEqual(t, base, parseLayout(t, "/data").Hash(), "mode")
	assert.NotEqual(t, base, parseLayout(t, "http://node{1...4}:9000/data").Hash(), "distributed")
	assert.NotEqual(t, base, parseLayout(t, "/data{1...2}", "/data{3...4}").Hash(), "pools")
	forced, err := newTestService(t, topology.WithParity(1)).Parse(context.Background(), []string{"/data{1...4}"})
	require.NoError(t, err)
	assert.NotEqual(t, base, forced.Hash(), "parity")
	assert.Equal(t, parseLayout(t, "/d1", "/d2", "/d3", "/d4").Hash(), parseLayout(t, "/d{1...4}").Hash(), "same drives, same hash")
}

func TestLayout_String(t *testing.T) {
	t.Parallel()
	s := parseLayout(t, "/data{1...4}").String()
	assert.Contains(t, s, "mode=snmd pools=1 nodes=1 drives=4")
	assert.Contains(t, s, "pool 1: 1 node(s) x 4 drive(s), 1 set(s) x 4 drive(s), EC:2 (data 2)")
	assert.Contains(t, s, "set 1: /data1 /data2 /data3 /data4")
	assert.Contains(t, s, "node local: 4 drive(s) [local]")

	s = parseLayout(t, "http://node{1...4}:9000/data{1...2}").String()
	assert.Contains(t, s, "mode=distributed pools=1 nodes=4 drives=8")
	assert.Contains(t, s, "http://node1:9000/data1(local)")
	assert.Contains(t, s, "http://node2:9000/data1 ")
	assert.Contains(t, s, "node node1:9000: 2 drive(s) [local]")
	assert.Contains(t, s, "node node4:9000: 2 drive(s)\n")
	assert.Equal(t, 1+1+1+4, strings.Count(s, "\n"))

	s = parseLayout(t, "/data").String()
	assert.Contains(t, s, "mode=snsd")
	assert.Contains(t, s, "EC:0 (data 1)")
}

func TestLayout_Helpers(t *testing.T) {
	t.Parallel()
	layout := parseLayout(t, "http://node{1...4}:9000/data{1...4}", "http://node{5...8}:9000/data{1...4}")
	assert.Equal(t, 32, layout.DriveCount())
	assert.Len(t, layout.Endpoints(), 32)
	assert.Equal(t, layout.Pools[1].Endpoints[0], layout.Endpoints()[16])

	p := layout.Pools[0]
	assert.Nil(t, p.Set(-1))
	assert.Nil(t, p.Set(1))
	assert.Len(t, p.Set(0), 16)
	assert.Len(t, p.Sets(), 1)
	assert.Equal(t, 12, p.DataShards())
	assert.Equal(t, 16, p.DriveCount())
	assert.Nil(t, topology.Pool{}.Set(0))
	assert.Nil(t, topology.Pool{SetCount: 2, SetDriveCount: 4, Endpoints: make([]domain.Endpoint, 4)}.Set(1))

	var empty topology.Layout
	assert.Equal(t, 0, empty.DriveCount())
	assert.Empty(t, empty.Endpoints())
	assert.Empty(t, empty.LocalEndpoints())
	_, ok := empty.LocalNode()
	assert.False(t, ok)
}

func TestLayout_Validate(t *testing.T) {
	t.Parallel()
	t.Run("parsed layouts are valid", func(t *testing.T) {
		for _, args := range [][]string{
			{"/data"},
			{"/data{1...4}"},
			{"/d1", "/d2"},
			{"http://node{1...4}:9000/data{1...4}"},
			{"http://node{1...4}:9000/data{1...4}", "http://node{5...8}:9000/data{1...8}"},
			{"http://node{1...32}:9000/data"},
		} {
			assert.NoError(t, parseLayout(t, args...).Validate(), "%v", args)
		}
	})

	var nilLayout *topology.Layout
	assert.ErrorIs(t, nilLayout.Validate(), topology.ErrInvalidLayout)

	tamper := func(name string, args []string, f func(l *topology.Layout), want error) {
		t.Run(name, func(t *testing.T) {
			l := parseLayout(t, args...)
			f(l)
			err := l.Validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, want)
		})
	}
	snmd := []string{"/data{1...4}"}
	dist := []string{"http://node{1...4}:9000/data{1...4}"}

	tamper("unknown mode", snmd, func(l *topology.Layout) { l.Mode = domain.ModeUnknown }, topology.ErrInvalidLayout)
	tamper("bogus mode", snmd, func(l *topology.Layout) { l.Mode = domain.Mode(42) }, topology.ErrInvalidLayout)
	tamper("no pools", snmd, func(l *topology.Layout) { l.Pools = nil }, topology.ErrInvalidLayout)
	tamper("snsd with many drives", snmd, func(l *topology.Layout) { l.Mode = domain.ModeSNSD }, topology.ErrInvalidLayout)
	tamper("snsd set size", []string{"/data"}, func(l *topology.Layout) { l.Pools[0].SetDriveCount = 2; l.Pools[0].SetCount = 1 }, topology.ErrInvalidLayout)
	tamper("pool index", snmd, func(l *topology.Layout) { l.Pools[0].Index = 3 }, topology.ErrInvalidLayout)
	tamper("pool without drives", snmd, func(l *topology.Layout) { l.Pools[0].Endpoints = nil }, topology.ErrInvalidLayout)
	tamper("set count mismatch", snmd, func(l *topology.Layout) { l.Pools[0].SetCount = 2 }, topology.ErrInvalidLayout)
	tamper("set size too big", []string{"/data{1...32}"}, func(l *topology.Layout) { l.Pools[0].SetCount = 1; l.Pools[0].SetDriveCount = 32 }, topology.ErrInvalidLayout)
	tamper("parity too high", snmd, func(l *topology.Layout) { l.Pools[0].Parity = 3 }, topology.ErrInvalidParity)
	tamper("parity zero", snmd, func(l *topology.Layout) { l.Pools[0].Parity = 0 }, topology.ErrInvalidParity)
	tamper("snsd parity", []string{"/data"}, func(l *topology.Layout) { l.Pools[0].Parity = 1 }, topology.ErrInvalidParity)
	tamper("relative endpoint", snmd, func(l *topology.Layout) { l.Pools[0].Endpoints[1].Path = "data2" }, topology.ErrRelativePath)
	tamper("root endpoint", snmd, func(l *topology.Layout) { l.Pools[0].Endpoints[1].Path = "/" }, topology.ErrRootPath)
	tamper("empty endpoint", snmd, func(l *topology.Layout) { l.Pools[0].Endpoints[1].Path = "" }, topology.ErrInvalidEndpoint)
	tamper("local path with host", snmd, func(l *topology.Layout) { l.Pools[0].Endpoints[1].Host = "x" }, topology.ErrInvalidEndpoint)
	tamper("url in snmd", snmd, func(l *topology.Layout) {
		l.Pools[0].Endpoints[1] = domain.Endpoint{Scheme: "http", Host: "node1:9000", Path: "/x", IsLocal: true, SetIdx: 0, DiskIdx: 1}
	}, topology.ErrInvalidLayout)
	tamper("path in distributed", dist, func(l *topology.Layout) {
		l.Pools[0].Endpoints[1] = domain.Endpoint{Path: "/x", IsLocal: true, SetIdx: 0, DiskIdx: 1}
	}, topology.ErrInvalidLayout)
	tamper("bad scheme", dist, func(l *topology.Layout) { l.Pools[0].Endpoints[0].Scheme = "ftp" }, topology.ErrInvalidScheme)
	tamper("mixed schemes", dist, func(l *topology.Layout) {
		l.Pools[0].Endpoints[0].Scheme = "https"
		l.Nodes = topology.BuildNodes(l)
	}, topology.ErrMixedSchemes)
	tamper("missing host", dist, func(l *topology.Layout) { l.Pools[0].Endpoints[0].Host = "" }, topology.ErrInvalidEndpoint)
	tamper("bad port", dist, func(l *topology.Layout) { l.Pools[0].Endpoints[0].Host = "node1:70000" }, topology.ErrInvalidEndpoint)
	tamper("missing port", dist, func(l *topology.Layout) { l.Pools[0].Endpoints[0].Host = "node1" }, topology.ErrInvalidEndpoint)
	tamper("placement swapped", snmd, func(l *topology.Layout) {
		l.Pools[0].Endpoints[0].DiskIdx, l.Pools[0].Endpoints[1].DiskIdx = 1, 0
	}, topology.ErrInvalidLayout)
	tamper("wrong pool idx", snmd, func(l *topology.Layout) { l.Pools[0].Endpoints[2].PoolIdx = 1 }, topology.ErrInvalidLayout)
	tamper("duplicate endpoint", snmd, func(l *topology.Layout) { l.Pools[0].Endpoints[2].Path = "/data1" }, topology.ErrDuplicateEndpoint)
	tamper("node count", dist, func(l *topology.Layout) { l.Pools[0].NodeCount = 3 }, topology.ErrInvalidLayout)
	tamper("set not spread across nodes", dist, func(l *topology.Layout) {
		// Put all drives of the set on node1: the symmetry check must notice.
		for i := range l.Pools[0].Endpoints {
			l.Pools[0].Endpoints[i].Host = "node1:9000"
			l.Pools[0].Endpoints[i].Path = "/data" + string(rune('a'+i))
			l.Pools[0].Endpoints[i].IsLocal = true
		}
		l.Pools[0].NodeCount = 1
		l.Nodes = topology.BuildNodes(l)
		l.Pools[0].NodeCount = 4
	}, topology.ErrInvalidLayout)
	tamper("uneven set", []string{"http://node{1...2}:9000/data{1...2}"}, func(l *topology.Layout) {
		// Move one drive from node2 to node1 inside the single set of 4.
		l.Pools[0].Endpoints[3].Host = "node1:9000"
		l.Pools[0].Endpoints[3].Path = "/data3"
		l.Pools[0].Endpoints[3].IsLocal = true
		l.Nodes = topology.BuildNodes(l)
	}, topology.ErrInvalidLayout)
	tamper("nodes missing", dist, func(l *topology.Layout) { l.Nodes = l.Nodes[:3] }, topology.ErrInvalidLayout)
	tamper("node without drives", dist, func(l *topology.Layout) { l.Nodes = append(l.Nodes, topology.Node{Host: "node9", Port: 9000}) }, topology.ErrInvalidLayout)
	tamper("node lists foreign drive", dist, func(l *topology.Layout) { l.Nodes[1].Endpoints[0] = l.Nodes[2].Endpoints[0] }, topology.ErrInvalidLayout)
	tamper("node locality mismatch", dist, func(l *topology.Layout) { l.Nodes[1].Local = true }, topology.ErrInvalidLayout)
	tamper("endpoint listed twice in nodes", dist, func(l *topology.Layout) {
		l.Nodes[1].Endpoints = append(l.Nodes[1].Endpoints, l.Nodes[1].Endpoints[0])
	}, topology.ErrInvalidLayout)
	tamper("two local nodes", dist, func(l *topology.Layout) {
		for i := range l.Nodes[1].Endpoints {
			l.Nodes[1].Endpoints[i].IsLocal = true
		}
		l.Nodes[1].Local = true
	}, topology.ErrInvalidLayout)
}

// TestSetQuorums: D drives to read and to write, D+1 to write when data
// and parity are as many (docs/03 §3.3).
func TestSetQuorums(t *testing.T) {
	tests := []struct {
		drives, parity, read, write int
	}{
		{drives: 4, parity: 2, read: 2, write: 3},
		{drives: 6, parity: 1, read: 5, write: 5},
		{drives: 8, parity: 4, read: 4, write: 5},
		{drives: 16, parity: 4, read: 12, write: 12},
		{drives: 1, parity: 0, read: 1, write: 1},
	}
	for _, tt := range tests {
		r, w := topology.SetQuorums(tt.drives, tt.parity)
		assert.Equal(t, tt.read, r, "%d drives EC:%d", tt.drives, tt.parity)
		assert.Equal(t, tt.write, w, "%d drives EC:%d", tt.drives, tt.parity)
	}
}
