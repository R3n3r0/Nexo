package topology

import (
	"fmt"
	"net"
	"strconv"
)

// NOTE: Mode and Endpoint are shared vocabulary that belongs to internal/domain.
// They are declared here until the domain package exists; the integrator can
// turn them into aliases (type Mode = domain.Mode, type Endpoint = domain.Endpoint)
// without touching the rest of the package.

// Mode is the deployment mode detected from the command line arguments.
type Mode string

const (
	// ModeSNSD is Single-Node Single-Drive: one local directory, no erasure coding.
	ModeSNSD Mode = "snsd"
	// ModeSNMD is Single-Node Multi-Drive: several local drives with erasure coding.
	ModeSNMD Mode = "snmd"
	// ModeDistributed is Multi-Node Multi-Drive: URL endpoints spread over nodes.
	ModeDistributed Mode = "distributed"
)

// String implements fmt.Stringer.
func (m Mode) String() string { return string(m) }

// Valid reports whether m is one of the known modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeSNSD, ModeSNMD, ModeDistributed:
		return true
	}
	return false
}

// Erasure reports whether the mode uses erasure coding (SNMD and distributed).
func (m Mode) Erasure() bool { return m == ModeSNMD || m == ModeDistributed }

// Endpoint is a single drive of the deployment, either a local directory or a
// drive exposed by a node through its URL.
type Endpoint struct {
	// Scheme is "http" or "https" for network endpoints and empty for local paths.
	Scheme string
	// Host is the lower-cased host name or IP literal; empty for local paths.
	Host string
	// Port is the TCP port of the node; zero for local paths.
	Port int
	// Path is the cleaned absolute drive path on the node.
	Path string
	// IsLocal is true when the endpoint belongs to the node running this process.
	IsLocal bool
	// PoolIdx is the zero-based index of the pool the endpoint belongs to.
	PoolIdx int
	// SetIdx is the zero-based index of the erasure set inside the pool.
	SetIdx int
	// DiskIdx is the zero-based position of the drive inside its erasure set.
	DiskIdx int
}

// IsURL reports whether the endpoint is a network endpoint (as opposed to a local path).
func (e Endpoint) IsURL() bool { return e.Scheme != "" }

// HostPort returns "host:port" for network endpoints and the empty string for local paths.
func (e Endpoint) HostPort() string {
	if !e.IsURL() {
		return ""
	}
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// NodeKey identifies the node that owns the endpoint: "host:port" for network
// endpoints and the empty string for local paths (all local paths share a node).
func (e Endpoint) NodeKey() string { return e.HostPort() }

// String returns the canonical textual form of the endpoint: the absolute path
// for local drives and scheme://host:port/path for network drives.
func (e Endpoint) String() string {
	if !e.IsURL() {
		return e.Path
	}
	return fmt.Sprintf("%s://%s%s", e.Scheme, e.HostPort(), e.Path)
}

// Equal reports whether two endpoints designate the same drive, ignoring the
// locality flag and the pool/set/disk placement.
func (e Endpoint) Equal(o Endpoint) bool {
	return e.Scheme == o.Scheme && e.Host == o.Host && e.Port == o.Port && e.Path == o.Path
}
