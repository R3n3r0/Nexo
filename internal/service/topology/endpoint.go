package topology

import (
	"net"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// newURLEndpoint builds a canonical URL endpoint: lower-case scheme and host
// name, explicit port. Canonical endpoints make duplicate detection and the
// topology hash independent of how the operator spelled the argument.
func newURLEndpoint(scheme, host string, port int, path string) domain.Endpoint {
	return domain.Endpoint{
		Scheme: strings.ToLower(scheme),
		Host:   net.JoinHostPort(strings.ToLower(host), strconv.Itoa(port)),
		Path:   path,
	}
}

// nodeKey identifies the node that owns the endpoint: "host:port" for URL
// endpoints and the empty string for local paths, which all belong to the
// implicit local node.
func nodeKey(ep domain.Endpoint) string {
	if !ep.IsURL() {
		return ""
	}
	return ep.Host
}

// endpointHostname returns the lower-cased host name or IP literal of a URL
// endpoint, without the port. It is empty for local paths.
func endpointHostname(ep domain.Endpoint) string { return strings.ToLower(ep.Hostname()) }

// endpointPort returns the port of a URL endpoint, or 0 when the endpoint is a
// local path or carries no valid port.
func endpointPort(ep domain.Endpoint) int {
	p, err := strconv.Atoi(ep.Port())
	if err != nil || p < 0 {
		return 0
	}
	return p
}

// validMode reports whether m is one of the modes the service can detect.
func validMode(m domain.Mode) bool {
	switch m {
	case domain.ModeSNSD, domain.ModeSNMD, domain.ModeDistributed:
		return true
	}
	return false
}
