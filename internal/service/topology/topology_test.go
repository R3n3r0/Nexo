package topology_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

func TestNew_Defaults(t *testing.T) {
	t.Parallel()
	svc := topology.New(topology.Deps{})
	require.NotNil(t, svc)
	layout, err := svc.Parse(context.Background(), []string{"/data"})
	require.NoError(t, err)
	assert.Equal(t, domain.ModeSNSD, layout.Mode)
	hosts, ids, logger, defaultPort, localPort := topology.Defaults(svc)
	assert.NotNil(t, hosts)
	assert.NotNil(t, ids)
	assert.NotNil(t, logger)
	assert.Equal(t, topology.DefaultPort, defaultPort)
	assert.Equal(t, topology.DefaultPort, localPort)
	assert.NotEmpty(t, topology.NewUUIDGenerator().NewID())
}

// TM-03 / docs/08 §8.1: mode detection.
//
// covers: S-STO-00, S-STO-01, S-STO-02, S-STO-06
func TestParse_ModeDetection(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	ctx := context.Background()

	t.Run("snsd", func(t *testing.T) {
		layout, err := svc.Parse(ctx, []string{"/data"})
		require.NoError(t, err)
		assert.Equal(t, domain.ModeSNSD, layout.Mode)
		require.Len(t, layout.Pools, 1)
		p := layout.Pools[0]
		assert.Equal(t, 1, p.SetCount)
		assert.Equal(t, 1, p.SetDriveCount)
		assert.Equal(t, 0, p.Parity)
		assert.Equal(t, 1, p.NodeCount)
		assert.Equal(t, []string{"/data"}, p.Args)
		require.Len(t, p.Endpoints, 1)
		assert.True(t, p.Endpoints[0].IsLocal)
		assert.Equal(t, "/data", p.Endpoints[0].String())
		require.Len(t, layout.Nodes, 1)
		assert.True(t, layout.Nodes[0].Local)
		assert.Equal(t, "local", layout.Nodes[0].String())
	})

	t.Run("snmd", func(t *testing.T) {
		layout, err := svc.Parse(ctx, []string{"/data{1...4}"})
		require.NoError(t, err)
		assert.Equal(t, domain.ModeSNMD, layout.Mode)
		require.Len(t, layout.Pools, 1)
		p := layout.Pools[0]
		assert.Equal(t, 1, p.SetCount)
		assert.Equal(t, 4, p.SetDriveCount)
		assert.Equal(t, 2, p.Parity)
		assert.Equal(t, 2, p.DataShards())
		assert.Equal(t, 4, layout.DriveCount())
		for i, ep := range p.Endpoints {
			assert.True(t, ep.IsLocal)
			assert.Equal(t, 0, ep.PoolIdx)
			assert.Equal(t, 0, ep.SetIdx)
			assert.Equal(t, i, ep.DiskIdx)
			assert.Equal(t, fmt.Sprintf("/data%d", i+1), ep.Path)
		}
	})

	t.Run("snmd explicit list", func(t *testing.T) {
		layout, err := svc.Parse(ctx, []string{"/d1", "/d2", "/d3", "/d4"})
		require.NoError(t, err)
		assert.Equal(t, domain.ModeSNMD, layout.Mode)
		require.Len(t, layout.Pools, 1)
		assert.Equal(t, 4, layout.Pools[0].DriveCount())
		assert.Equal(t, []string{"/d1", "/d2", "/d3", "/d4"}, layout.Pools[0].Args)
	})

	t.Run("distributed", func(t *testing.T) {
		layout, err := svc.Parse(ctx, []string{"http://node{1...4}:9000/data{1...4}"})
		require.NoError(t, err)
		assert.Equal(t, domain.ModeDistributed, layout.Mode)
		require.Len(t, layout.Pools, 1)
		p := layout.Pools[0]
		assert.Equal(t, 16, p.DriveCount())
		assert.Equal(t, 4, p.NodeCount)
		assert.Equal(t, 1, p.SetCount)
		assert.Equal(t, 16, p.SetDriveCount)
		assert.Equal(t, 4, p.Parity)
		require.Len(t, layout.Nodes, 4)
		assert.Equal(t, "node1:9000", layout.Nodes[0].String())
		assert.True(t, layout.Nodes[0].Local)
		for _, n := range layout.Nodes[1:] {
			assert.False(t, n.Local)
			assert.Len(t, n.Endpoints, 4)
		}
		assert.Len(t, layout.LocalEndpoints(), 4)
		local, ok := layout.LocalNode()
		require.True(t, ok)
		assert.Equal(t, "node1", local.Host)
		assert.Equal(t, 9000, local.Port)
	})

	t.Run("distributed single node", func(t *testing.T) {
		layout, err := svc.Parse(ctx, []string{"http://node1:9000/data{1...4}"})
		require.NoError(t, err)
		assert.Equal(t, domain.ModeDistributed, layout.Mode)
		assert.Equal(t, 1, layout.Pools[0].NodeCount)
		assert.Len(t, layout.LocalEndpoints(), 4)
	})
}

func TestParse_Errors(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	ctx := context.Background()
	tests := []struct {
		name string
		args []string
		want error
	}{
		{"no args", nil, topology.ErrNoEndpoints},
		{"empty args", []string{}, topology.ErrNoEndpoints},
		{"empty arg", []string{""}, topology.ErrInvalidEndpoint},
		{"relative path", []string{"data"}, topology.ErrRelativePath},
		{"dot relative path", []string{"./data"}, topology.ErrRelativePath},
		{"host without scheme", []string{"node1:9000/data"}, topology.ErrRelativePath},
		{"root path", []string{"/"}, topology.ErrRootPath},
		{"root path via dots", []string{"/data/.."}, topology.ErrRootPath},
		{"root path in expansion", []string{"/data{1...2}", "/"}, topology.ErrMixedEllipsis},
		{"bad scheme", []string{"ftp://node1:9000/data"}, topology.ErrInvalidScheme},
		{"bad scheme ellipsis", []string{"ftp://node{1...4}:9000/data"}, topology.ErrInvalidScheme},
		{"url without path", []string{"http://node1:9000"}, topology.ErrRootPath},
		{"url root path", []string{"http://node1:9000/"}, topology.ErrRootPath},
		{"url user info", []string{"http://user:pw@node1:9000/data"}, topology.ErrInvalidEndpoint},
		{"url query", []string{"http://node1:9000/data?x=1"}, topology.ErrInvalidEndpoint},
		{"url fragment", []string{"http://node1:9000/data#x"}, topology.ErrInvalidEndpoint},
		{"url port too high", []string{"http://node1:99999/data"}, topology.ErrInvalidEndpoint},
		{"url port zero", []string{"http://node1:0/data"}, topology.ErrInvalidEndpoint},
		{"url missing host", []string{"http://:9000/data"}, topology.ErrInvalidEndpoint},
		{"url garbage", []string{"http://node1:abc/data"}, topology.ErrInvalidEndpoint},
		{"mixed path and url", []string{"/data", "http://node1:9000/data"}, topology.ErrMixedEndpoints},
		{"mixed path and url ellipsis", []string{"/data{1...2}", "http://node{1...2}:9000/d"}, topology.ErrMixedEndpoints},
		{"mixed http and https", []string{"http://node1:9000/data", "https://node2:9000/data"}, topology.ErrMixedSchemes},
		{"mixed http and https ellipsis", []string{"http://node{1...2}:9000/d{1...2}", "https://node{3...4}:9000/d{1...2}"}, topology.ErrMixedSchemes},
		{"mixed scheme case", []string{"HTTP://node1:9000/data", "https://node2:9000/data"}, topology.ErrMixedSchemes},
		{"duplicate path", []string{"/data", "/data"}, topology.ErrDuplicateEndpoint},
		{"duplicate after clean", []string{"/data/", "/data"}, topology.ErrDuplicateEndpoint},
		{"duplicate across pools", []string{"/data{1...4}", "/data{3...6}"}, topology.ErrDuplicateEndpoint},
		{"duplicate url case", []string{"http://Node1:9000/d1", "http://node1:9000/d1"}, topology.ErrDuplicateEndpoint},
		{"duplicate url default port", []string{"http://node1/d1", "http://node1:9000/d1"}, topology.ErrDuplicateEndpoint},
		{"invalid ellipsis", []string{"/data{1..4}"}, topology.ErrInvalidEllipsis},
		{"invalid ellipsis url", []string{"http://node{1...4}:9000/data{a...4}"}, topology.ErrInvalidEllipsis},
		{"too many", []string{"/data{1...70000}"}, topology.ErrTooManyEndpoints},
		{"mixed ellipsis and plain", []string{"/data{1...4}", "/extra"}, topology.ErrMixedEllipsis},
		{"prime drives", []string{"/data{1...17}"}, topology.ErrInvalidSetSize},
		{"prime nodes", []string{"http://node{1...17}:9000/data"}, topology.ErrInvalidSetSize},
		{"prime nodes many drives", []string{"http://node{1...17}:9000/data{1...16}"}, topology.ErrInvalidSetSize},
		{"second pool invalid", []string{"http://node{1...4}:9000/d{1...4}", "http://node{5...23}:9000/d"}, topology.ErrInvalidSetSize},
		{"asymmetric nodes", []string{"http://node1:9000/d1", "http://node1:9000/d2", "http://node1:9000/d3", "http://node2:9000/d1"}, topology.ErrAsymmetricPool},
		{"odd split", []string{"http://node1:9000/d1", "http://node2:9000/d1", "http://node2:9000/d2"}, topology.ErrInvalidSetSize},
		{"no local endpoint", []string{"http://node{2...5}:9000/data{1...4}"}, topology.ErrNoLocalEndpoint},
		{"wrong port for local", []string{"http://node1:9001/d1", "http://node2:9001/d1"}, topology.ErrNoLocalEndpoint},
		{"ambiguous local", []string{"http://node1:9000/d1", "http://localhost:9000/d2"}, topology.ErrAmbiguousLocalNode},
		{"ambiguous local alias", []string{"http://node1:9000/d1", "http://alias:9000/d1"}, topology.ErrAmbiguousLocalNode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			layout, err := svc.Parse(ctx, tc.args)
			require.Error(t, err, "args %v", tc.args)
			assert.Nil(t, layout)
			assert.True(t, errors.Is(err, tc.want), "args %v: got %v, want %v", tc.args, err, tc.want)
		})
	}
}

func TestParse_Options(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("forced set size", func(t *testing.T) {
		layout, err := newTestService(t, topology.WithSetDriveCount(4)).Parse(ctx, []string{"/data{1...16}"})
		require.NoError(t, err)
		assert.Equal(t, 4, layout.Pools[0].SetCount)
		assert.Equal(t, 4, layout.Pools[0].SetDriveCount)
		assert.Equal(t, 2, layout.Pools[0].Parity)
	})
	t.Run("forced set size invalid", func(t *testing.T) {
		_, err := newTestService(t, topology.WithSetDriveCount(5)).Parse(ctx, []string{"/data{1...16}"})
		assert.ErrorIs(t, err, topology.ErrInvalidSetSize)
		_, err = newTestService(t, topology.WithSetDriveCount(8)).Parse(ctx, []string{"http://node{1...3}:9000/d{1...8}"})
		assert.ErrorIs(t, err, topology.ErrInvalidSetSize)
	})
	t.Run("forced set size ignored for snsd", func(t *testing.T) {
		layout, err := newTestService(t, topology.WithSetDriveCount(4)).Parse(ctx, []string{"/data"})
		require.NoError(t, err)
		assert.Equal(t, 1, layout.Pools[0].SetDriveCount)
	})
	t.Run("forced parity", func(t *testing.T) {
		layout, err := newTestService(t, topology.WithParity(3)).Parse(ctx, []string{"/data{1...8}"})
		require.NoError(t, err)
		assert.Equal(t, 3, layout.Pools[0].Parity)
		assert.Equal(t, 5, layout.Pools[0].DataShards())
	})
	t.Run("forced parity invalid", func(t *testing.T) {
		_, err := newTestService(t, topology.WithParity(5)).Parse(ctx, []string{"/data{1...8}"})
		assert.ErrorIs(t, err, topology.ErrInvalidParity)
		_, err = newTestService(t, topology.WithParity(0)).Parse(ctx, []string{"/data{1...8}"})
		assert.ErrorIs(t, err, topology.ErrInvalidParity)
	})
	t.Run("forced parity ignored for snsd", func(t *testing.T) {
		layout, err := newTestService(t, topology.WithParity(2)).Parse(ctx, []string{"/data"})
		require.NoError(t, err)
		assert.Equal(t, 0, layout.Pools[0].Parity)
	})
	t.Run("default port", func(t *testing.T) {
		_, err := newTestService(t, topology.WithDefaultPort(9100)).Parse(ctx, []string{"http://node{1...2}/d{1...2}"})
		assert.ErrorIs(t, err, topology.ErrNoLocalEndpoint, "local port still 9000")

		layout, err := newTestService(t, topology.WithDefaultPort(9100), topology.WithLocalPort(9100)).Parse(ctx, []string{"http://node{1...2}/d{1...2}"})
		require.NoError(t, err)
		for _, ep := range layout.Endpoints() {
			assert.Equal(t, "9100", ep.Port())
		}
		assert.Equal(t, "http://node1:9100/d1", layout.Pools[0].Endpoints[0].String())
	})
	t.Run("explicit port wins over default", func(t *testing.T) {
		layout, err := newTestService(t, topology.WithDefaultPort(9100)).Parse(ctx, []string{"http://node{1...2}:9000/d{1...2}"})
		require.NoError(t, err)
		assert.Equal(t, "9000", layout.Pools[0].Endpoints[0].Port())
	})
}

// covers: S-STO-08
func TestParse_MultiPool(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	layout, err := svc.Parse(context.Background(), []string{
		"http://node{1...4}:9000/data{1...4}",
		"http://node{5...8}:9000/data{1...8}",
	})
	require.NoError(t, err)
	assert.Equal(t, domain.ModeDistributed, layout.Mode)
	require.Len(t, layout.Pools, 2)
	require.Len(t, layout.Nodes, 8)
	assert.Equal(t, 48, layout.DriveCount())

	p0, p1 := layout.Pools[0], layout.Pools[1]
	assert.Equal(t, 0, p0.Index)
	assert.Equal(t, 1, p1.Index)
	assert.Equal(t, []string{"http://node{1...4}:9000/data{1...4}"}, p0.Args)
	assert.Equal(t, 1, p0.SetCount)
	assert.Equal(t, 16, p0.SetDriveCount)
	assert.Equal(t, 2, p1.SetCount)
	assert.Equal(t, 16, p1.SetDriveCount)
	assert.Equal(t, 4, p1.Parity)
	for _, ep := range p1.Endpoints {
		assert.Equal(t, 1, ep.PoolIdx)
		assert.False(t, ep.IsLocal)
	}
	for s := 0; s < p1.SetCount; s++ {
		perNode := map[string]int{}
		for _, ep := range p1.Set(s) {
			perNode[ep.Host]++
		}
		assert.Len(t, perNode, 4)
		for _, c := range perNode {
			assert.Equal(t, 4, c)
		}
	}
	assert.Len(t, layout.LocalEndpoints(), 4)
	assert.Equal(t, "node1:9000", layout.Nodes[0].String())
	assert.Equal(t, "node8:9000", layout.Nodes[7].String())
}

func TestParse_SetsSpreadAcrossNodes(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	ctx := context.Background()

	t.Run("32 nodes x 1 drive -> 2 sets on distinct nodes", func(t *testing.T) {
		layout, err := svc.Parse(ctx, []string{"http://node{1...32}:9000/data"})
		require.NoError(t, err)
		p := layout.Pools[0]
		assert.Equal(t, 2, p.SetCount)
		assert.Equal(t, 16, p.SetDriveCount)
		for s := 0; s < 2; s++ {
			hosts := map[string]struct{}{}
			for _, ep := range p.Set(s) {
				hosts[ep.Hostname()] = struct{}{}
			}
			assert.Len(t, hosts, 16)
		}
		assert.Equal(t, "node1", p.Set(0)[0].Hostname())
		assert.Equal(t, "node16", p.Set(0)[15].Hostname())
		assert.Equal(t, "node17", p.Set(1)[0].Hostname())
		assert.Equal(t, "node32", p.Set(1)[15].Hostname())
	})

	t.Run("16 nodes x 2 drives -> every set takes one drive per node", func(t *testing.T) {
		layout, err := svc.Parse(ctx, []string{"http://node{1...16}:9000/data{1...2}"})
		require.NoError(t, err)
		p := layout.Pools[0]
		assert.Equal(t, 2, p.SetCount)
		for s := 0; s < 2; s++ {
			hosts := map[string]int{}
			for _, ep := range p.Set(s) {
				hosts[ep.Hostname()]++
				assert.Equal(t, fmt.Sprintf("/data%d", s+1), ep.Path)
			}
			assert.Len(t, hosts, 16)
		}
	})

	t.Run("4 nodes x 2 drives -> one set of 8, two per node", func(t *testing.T) {
		layout, err := svc.Parse(ctx, []string{"http://node{1...4}:9000/data{1...2}"})
		require.NoError(t, err)
		p := layout.Pools[0]
		assert.Equal(t, 1, p.SetCount)
		assert.Equal(t, 8, p.SetDriveCount)
		assert.Equal(t, 4, p.Parity)
		want := []string{
			"http://node1:9000/data1", "http://node2:9000/data1", "http://node3:9000/data1", "http://node4:9000/data1",
			"http://node1:9000/data2", "http://node2:9000/data2", "http://node3:9000/data2", "http://node4:9000/data2",
		}
		got := make([]string, 0, 8)
		for _, ep := range p.Endpoints {
			got = append(got, ep.String())
		}
		assert.Equal(t, want, got)
	})

	t.Run("explicit list is interleaved by node", func(t *testing.T) {
		layout, err := svc.Parse(ctx, []string{
			"http://node1:9000/d1", "http://node1:9000/d2", "http://node2:9000/d1", "http://node2:9000/d2",
		})
		require.NoError(t, err)
		p := layout.Pools[0]
		assert.Equal(t, "node1:9000", p.Endpoints[0].Host)
		assert.Equal(t, "node2:9000", p.Endpoints[1].Host)
		assert.Equal(t, "node1:9000", p.Endpoints[2].Host)
		assert.Equal(t, "node2:9000", p.Endpoints[3].Host)
	})
}

// TS-U-02: every topology from 1x1 to 32x32 nodes x drives.
//
// covers: S-STO-00
func TestParse_Matrix(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	other := topology.New(topology.Deps{Hosts: newResolver(t, nodeHost(2)), Logger: quietLogger()})
	ctx := context.Background()

	for nodes := 1; nodes <= 32; nodes++ {
		for drives := 1; drives <= 32; drives++ {
			nodes, drives := nodes, drives
			t.Run(fmt.Sprintf("%dx%d", nodes, drives), func(t *testing.T) {
				t.Parallel()
				var args []string
				switch {
				case nodes == 1 && drives == 1:
					args = []string{"/data"}
				case nodes == 1:
					args = []string{fmt.Sprintf("/data{1...%d}", drives)}
				default:
					args = []string{fmt.Sprintf("http://node{1...%d}:9000/data{1...%d}", nodes, drives)}
				}
				total := nodes * drives

				possible := false
				for s := topology.MinSetDriveCount; s <= topology.MaxSetDriveCount; s++ {
					if total%s == 0 && topology.IsSymmetric(s, nodes) {
						possible = true
					}
				}

				layout, err := svc.Parse(ctx, args)
				if total == 1 {
					require.NoError(t, err)
					assert.Equal(t, domain.ModeSNSD, layout.Mode)
					assert.Equal(t, 1, layout.Pools[0].SetDriveCount)
					assert.Equal(t, 0, layout.Pools[0].Parity)
					return
				}
				if !possible {
					require.Error(t, err)
					assert.ErrorIs(t, err, topology.ErrInvalidSetSize)
					return
				}
				require.NoError(t, err)
				require.NoError(t, layout.Validate())
				if nodes == 1 {
					assert.Equal(t, domain.ModeSNMD, layout.Mode)
				} else {
					assert.Equal(t, domain.ModeDistributed, layout.Mode)
				}
				require.Len(t, layout.Pools, 1)
				require.Len(t, layout.Nodes, nodes)
				p := layout.Pools[0]
				assert.Equal(t, nodes, p.NodeCount)
				assert.GreaterOrEqual(t, p.SetDriveCount, topology.MinSetDriveCount)
				assert.LessOrEqual(t, p.SetDriveCount, topology.MaxSetDriveCount)
				assert.Equal(t, total, p.SetCount*p.SetDriveCount)
				assert.Equal(t, 0, total%p.SetDriveCount)
				assert.True(t, topology.IsSymmetric(p.SetDriveCount, nodes))
				assert.Equal(t, topology.DefaultParity(p.SetDriveCount), p.Parity)

				// Symmetry: every set touches min(S, N) nodes with the same number of drives each.
				for s := 0; s < p.SetCount; s++ {
					perNode := map[string]int{}
					for d, ep := range p.Set(s) {
						perNode[ep.Host]++
						assert.Equal(t, s, ep.SetIdx)
						assert.Equal(t, d, ep.DiskIdx)
					}
					assert.Len(t, perNode, min(p.SetDriveCount, nodes), "set %d", s)
					for _, c := range perNode {
						assert.Equal(t, p.SetDriveCount/min(p.SetDriveCount, nodes), c, "set %d", s)
					}
				}
				for _, n := range layout.Nodes {
					assert.Len(t, n.Endpoints, drives)
				}
				assert.Len(t, layout.LocalEndpoints(), drives)

				// Hash is stable across parses and independent of the local node.
				again, err := svc.Parse(ctx, args)
				require.NoError(t, err)
				assert.Equal(t, layout.Hash(), again.Hash())
				assert.Len(t, layout.Hash(), 64)
				if nodes > 1 {
					fromNode2, err := other.Parse(ctx, args)
					require.NoError(t, err)
					assert.Equal(t, layout.Hash(), fromNode2.Hash())
					assert.Equal(t, "node2", mustLocalNode(t, fromNode2).Host)
				}
			})
		}
	}
}

// mustParse builds an endpoint with domain.ParseEndpoint (not canonicalised).
func TestIsLocal(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	ctx := context.Background()
	url := func(raw string) domain.Endpoint {
		ep, err := topology.ParseEndpoint(svc, raw)
		require.NoError(t, err)
		return ep
	}
	tests := []struct {
		name string
		ep   domain.Endpoint
		want bool
	}{
		{"local path", domain.Endpoint{Path: "/data"}, true},
		{"hostname", url("http://node1:9000/d"), true},
		{"hostname upper case", domain.Endpoint{Scheme: "http", Host: "NODE1:9000", Path: "/d"}, true},
		{"missing port", domain.Endpoint{Scheme: "http", Host: "node1", Path: "/d"}, false},
		{"domain parsed endpoint", mustParse(t, "http://node1:9000/d"), true},
		{"localhost", url("http://localhost:9000/d"), true},
		{"loopback v4", url("http://127.0.0.1:9000/d"), true},
		{"loopback v6", url("http://[::1]:9000/d"), true},
		{"unspecified", url("http://0.0.0.0:9000/d"), true},
		{"interface ip", url("http://10.0.0.1:9000/d"), true},
		{"other ip", url("http://10.0.0.2:9000/d"), false},
		{"dns alias of local ip", url("http://alias:9000/d"), true},
		{"dns multiple ips one local", url("http://multi:9000/d"), true},
		{"other host", url("http://node2:9000/d"), false},
		{"unresolvable host", url("http://nowhere:9000/d"), false},
		{"wrong port", url("http://node1:9001/d"), false},
		{"wrong port localhost", url("http://localhost:9001/d"), false},
		{"https same host", url("https://node1:9000/d"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.IsLocal(ctx, tc.ep)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestIsLocal_ResolverFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("hostname error falls back to addresses", func(t *testing.T) {
		h := nodeHost(1)
		h.hostnameErr = errors.New("boom")
		r := newResolver(t, h)
		svc := topology.New(topology.Deps{Hosts: r, Logger: quietLogger()})
		local, err := svc.IsLocal(ctx, domain.Endpoint{Scheme: "http", Host: "10.0.0.1:9000", Path: "/d"})
		require.NoError(t, err)
		assert.True(t, local)
		local, err = svc.IsLocal(ctx, domain.Endpoint{Scheme: "http", Host: "node1:9000", Path: "/d"})
		require.NoError(t, err)
		assert.True(t, local, "node1 still resolves to a local address")
	})

	t.Run("interface error falls back to hostname", func(t *testing.T) {
		h := nodeHost(1)
		h.addrsErr = errors.New("boom")
		r := newResolver(t, h)
		svc := topology.New(topology.Deps{Hosts: r, Logger: quietLogger()})
		local, err := svc.IsLocal(ctx, domain.Endpoint{Scheme: "http", Host: "node1:9000", Path: "/d"})
		require.NoError(t, err)
		assert.True(t, local)
		local, err = svc.IsLocal(ctx, domain.Endpoint{Scheme: "http", Host: "alias:9000", Path: "/d"})
		require.NoError(t, err)
		assert.True(t, local, "hostname resolution still provides 10.0.0.1")
	})

	t.Run("nothing known", func(t *testing.T) {
		r := newResolver(t, host{hostnameErr: errors.New("boom"), addrsErr: errors.New("boom")})
		svc := topology.New(topology.Deps{Hosts: r, Logger: quietLogger()})
		local, err := svc.IsLocal(ctx, domain.Endpoint{Scheme: "http", Host: "node1:9000", Path: "/d"})
		require.NoError(t, err)
		assert.False(t, local)
		local, err = svc.IsLocal(ctx, domain.Endpoint{Scheme: "http", Host: "localhost:9000", Path: "/d"})
		require.NoError(t, err)
		assert.True(t, local)
		_, err = svc.Parse(ctx, []string{"http://node{1...2}:9000/d"})
		assert.ErrorIs(t, err, topology.ErrNoLocalEndpoint)
	})

	t.Run("cancelled context", func(t *testing.T) {
		svc := newTestService(t)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := svc.IsLocal(cctx, domain.Endpoint{Scheme: "http", Host: "nowhere:9000", Path: "/d"})
		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestService_Formats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	layout, err := svc.Parse(ctx, []string{"/data{1...4}"})
	require.NoError(t, err)

	formats, err := svc.NewFormats(ctx, layout, 0, "dep-1")
	require.NoError(t, err)
	require.Len(t, formats, 4)
	ids := map[string]struct{}{}
	for i, f := range formats {
		require.NoError(t, f.Validate())
		assert.Equal(t, "dep-1", f.ID)
		assert.Equal(t, topology.FormatModeErasure, f.Mode)
		assert.Equal(t, 1, f.SetCount())
		assert.Equal(t, 4, f.SetDriveCount())
		set, disk, err := f.Position()
		require.NoError(t, err)
		assert.Equal(t, 0, set)
		assert.Equal(t, i, disk)
		ids[f.Erasure.This] = struct{}{}
		assert.True(t, formats[0].SameTopology(f))
	}
	assert.Len(t, ids, 4)

	ref, err := svc.CheckFormats(ctx, layout, 0, formats)
	require.NoError(t, err)
	assert.Same(t, formats[0], ref)

	t.Run("missing drive tolerated", func(t *testing.T) {
		partial := append([]*topology.FormatV1(nil), formats...)
		partial[0] = nil
		ref, err := svc.CheckFormats(ctx, layout, 0, partial)
		require.NoError(t, err)
		assert.Same(t, formats[1], ref)
	})
	t.Run("all missing", func(t *testing.T) {
		_, err := svc.CheckFormats(ctx, layout, 0, make([]*topology.FormatV1, 4))
		assert.ErrorIs(t, err, topology.ErrInvalidFormat)
	})
	t.Run("wrong count", func(t *testing.T) {
		_, err := svc.CheckFormats(ctx, layout, 0, formats[:3])
		assert.ErrorIs(t, err, topology.ErrFormatLayoutMismatch)
	})
	t.Run("different deployment", func(t *testing.T) {
		foreign := formats[2].Clone()
		foreign.ID = "dep-2"
		mixed := append([]*topology.FormatV1(nil), formats...)
		mixed[2] = foreign
		_, err := svc.CheckFormats(ctx, layout, 0, mixed)
		assert.ErrorIs(t, err, topology.ErrFormatMismatch)
	})
	t.Run("layout mismatch", func(t *testing.T) {
		bigger, err := svc.Parse(ctx, []string{"/data{1...8}"})
		require.NoError(t, err)
		other, err := svc.NewFormats(ctx, bigger, 0, "dep-1")
		require.NoError(t, err)
		_, err = svc.CheckFormats(ctx, bigger, 0, append(append([]*topology.FormatV1(nil), formats...), nil, nil, nil, nil))
		assert.ErrorIs(t, err, topology.ErrFormatLayoutMismatch, "drives formatted 1x4 used in a 1x8 pool")
		_, err = svc.CheckFormats(ctx, layout, 0, other[:4])
		assert.ErrorIs(t, err, topology.ErrFormatLayoutMismatch)
	})
	t.Run("mode mismatch", func(t *testing.T) {
		single, err := svc.Parse(ctx, []string{"/data"})
		require.NoError(t, err)
		_, err = svc.CheckFormats(ctx, single, 0, formats[:1])
		assert.ErrorIs(t, err, topology.ErrFormatModeMismatch)

		snsd, err := svc.NewFormats(ctx, single, 0, "dep-3")
		require.NoError(t, err)
		require.Len(t, snsd, 1)
		assert.Equal(t, topology.FormatModeSNSD, snsd[0].Mode)
		ref, err := svc.CheckFormats(ctx, single, 0, snsd)
		require.NoError(t, err)
		assert.Equal(t, "dep-3", ref.ID)
	})
	t.Run("pool index", func(t *testing.T) {
		_, err := svc.NewFormats(ctx, layout, 1, "dep-1")
		assert.ErrorIs(t, err, topology.ErrInvalidLayout)
		_, err = svc.CheckFormats(ctx, layout, -1, formats)
		assert.ErrorIs(t, err, topology.ErrInvalidLayout)
		_, err = svc.NewFormats(ctx, nil, 0, "dep-1")
		assert.ErrorIs(t, err, topology.ErrInvalidLayout)
	})
	t.Run("empty deployment id", func(t *testing.T) {
		_, err := svc.NewFormats(ctx, layout, 0, "")
		assert.ErrorIs(t, err, topology.ErrInvalidFormat)
	})
	t.Run("multi pool", func(t *testing.T) {
		cluster, err := svc.Parse(ctx, []string{"http://node{1...4}:9000/d{1...4}", "http://node{5...8}:9000/d{1...8}"})
		require.NoError(t, err)
		f1, err := svc.NewFormats(ctx, cluster, 1, "dep-9")
		require.NoError(t, err)
		require.Len(t, f1, 32)
		assert.Equal(t, 2, f1[0].SetCount())
		assert.Equal(t, 16, f1[0].SetDriveCount())
		set, disk, err := f1[17].Position()
		require.NoError(t, err)
		assert.Equal(t, 1, set)
		assert.Equal(t, 1, disk)
		_, err = svc.CheckFormats(ctx, cluster, 1, f1)
		require.NoError(t, err)
		_, err = svc.CheckFormats(ctx, cluster, 0, f1[:16])
		assert.ErrorIs(t, err, topology.ErrFormatLayoutMismatch)
	})
}

// Argument errors wrap domain.ErrInvalidArgument so that callers (CLI exit
// codes, dry-run reports) can treat them uniformly.
func TestParse_ErrorsWrapInvalidArgument(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	for _, args := range [][]string{
		nil, {"data"}, {"/"}, {"/data{1..4}"}, {"/data", "/data"}, {"/data{1...17}"},
		{"ftp://node1:9000/d"}, {"/data", "http://node1:9000/d"}, {"http://node{2...5}:9000/d"},
		{"http://node1:9000/d", "https://node2:9000/d"}, {"http://node1:9000/d1", "http://localhost:9000/d2"},
	} {
		_, err := svc.Parse(context.Background(), args)
		require.Error(t, err, "%v", args)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, "%v: %v", args, err)
	}
	// Layout and format consistency errors are not argument errors.
	assert.NotErrorIs(t, topology.ErrInvalidLayout, domain.ErrInvalidArgument)
	assert.NotErrorIs(t, topology.ErrFormatMismatch, domain.ErrInvalidArgument)
}
