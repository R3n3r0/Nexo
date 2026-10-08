package topology_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/topology"
	topologymock "github.com/r3n3r0/nexo/internal/service/topology/mock"
)

// host describes the identity a mocked HostResolver reports.
type host struct {
	hostname    string
	hostnameErr error
	addrs       []string
	addrsErr    error
}

// nodeHost is the machine node<n> at 10.0.0.<n> (plus loopback).
func nodeHost(n int) host {
	return host{hostname: fmt.Sprintf("node%d", n), addrs: []string{"127.0.0.1", fmt.Sprintf("10.0.0.%d", n)}}
}

// newResolver returns the generated HostResolver mock answering for h, in a
// DNS where node1..node32 map to 10.0.0.1..10.0.0.32, "alias" to 10.0.0.1
// and "multi" to 10.0.0.9 and 10.0.0.1.
func newResolver(t *testing.T, h host) *topologymock.HostResolver {
	t.Helper()
	dns := map[string][]string{
		"alias": {"10.0.0.1"},
		"multi": {"10.0.0.9", "10.0.0.1"},
	}
	for i := 1; i <= 32; i++ {
		dns[fmt.Sprintf("node%d", i)] = []string{fmt.Sprintf("10.0.0.%d", i)}
	}
	var addrs []net.Addr
	for _, a := range h.addrs {
		addrs = append(addrs, &net.IPNet{IP: net.ParseIP(a), Mask: net.CIDRMask(24, 32)})
	}
	r := topologymock.NewHostResolver(t)
	r.EXPECT().Hostname(mock.Anything).Return(h.hostname, h.hostnameErr).Maybe()
	r.EXPECT().InterfaceAddrs(mock.Anything).Return(addrs, h.addrsErr).Maybe()
	r.EXPECT().LookupHost(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, name string) ([]string, error) {
		if ips, ok := dns[name]; ok {
			return ips, nil
		}
		return nil, fmt.Errorf("lookup %s: no such host", name)
	}).Maybe()
	return r
}

// counterIDs returns the generated IDGenerator mock producing id-001,
// id-002, ...
func counterIDs(t *testing.T) *topologymock.IDGenerator {
	t.Helper()
	var n atomic.Int64
	ids := topologymock.NewIDGenerator(t)
	ids.EXPECT().NewID().RunAndReturn(func() string { return fmt.Sprintf("id-%03d", n.Add(1)) }).Maybe()
	return ids
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestService returns the service running on node1.
func newTestService(t *testing.T, opts ...topology.Option) topology.Service {
	t.Helper()
	return topology.New(topology.Deps{Hosts: newResolver(t, nodeHost(1)), IDs: counterIDs(t), Logger: quietLogger()}, opts...)
}

func parseLayout(t *testing.T, args ...string) *topology.Layout {
	t.Helper()
	layout, err := newTestService(t).Parse(context.Background(), args)
	require.NoError(t, err)
	return layout
}

func mustParse(t *testing.T, raw string) domain.Endpoint {
	t.Helper()
	ep, err := domain.ParseEndpoint(raw)
	require.NoError(t, err)
	return ep
}

func mustLocalNode(t *testing.T, l *topology.Layout) topology.Node {
	t.Helper()
	n, ok := l.LocalNode()
	require.True(t, ok)
	return n
}
