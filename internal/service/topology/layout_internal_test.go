package topology

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/domain"
)

// The tests in this file cover unexported helpers; the service is tested
// from the external package with the generated mocks.

func TestEndpointHelpers(t *testing.T) {
	t.Parallel()
	local := domain.Endpoint{Path: "/data"}
	assert.False(t, local.IsURL())
	assert.Equal(t, "", nodeKey(local))
	assert.Equal(t, "", endpointHostname(local))
	assert.Equal(t, 0, endpointPort(local))
	assert.Equal(t, "/data", local.String())

	remote := newURLEndpoint("HTTPS", "Node1", 9000, "/data")
	assert.True(t, remote.IsURL())
	assert.Equal(t, "https", remote.Scheme)
	assert.Equal(t, "node1:9000", remote.Host)
	assert.Equal(t, "node1:9000", nodeKey(remote))
	assert.Equal(t, "node1", endpointHostname(remote))
	assert.Equal(t, 9000, endpointPort(remote))
	assert.Equal(t, "https://node1:9000/data", remote.String())

	v6 := newURLEndpoint("http", "::1", 9000, "/data")
	assert.Equal(t, "http://[::1]:9000/data", v6.String())
	assert.Equal(t, "::1", endpointHostname(v6))
	assert.Equal(t, 9000, endpointPort(v6))

	assert.Equal(t, 0, endpointPort(domain.Endpoint{Scheme: "http", Host: "node1", Path: "/d"}), "no port")
	assert.Equal(t, 0, endpointPort(domain.Endpoint{Scheme: "http", Host: "node1:abc", Path: "/d"}), "bad port")
	assert.Equal(t, "node1", endpointHostname(domain.Endpoint{Scheme: "http", Host: "NODE1:9000", Path: "/d"}))
}

func TestNode_String(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "local", Node{}.String())
	assert.Equal(t, "", Node{}.key())
	assert.Equal(t, "node1:9000", Node{Host: "node1", Port: 9000}.String())
	assert.Equal(t, "node1:9000", Node{Host: "node1", Port: 9000}.key())
	assert.Equal(t, "[fe80::1]:9000", Node{Host: "fe80::1", Port: 9000}.String())
}

func TestValidMode(t *testing.T) {
	t.Parallel()
	assert.True(t, validMode(domain.ModeSNSD))
	assert.True(t, validMode(domain.ModeSNMD))
	assert.True(t, validMode(domain.ModeDistributed))
	assert.False(t, validMode(domain.ModeUnknown))
	assert.False(t, validMode(domain.Mode(42)))
}
