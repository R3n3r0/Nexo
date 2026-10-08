package topology

import (
	"log/slog"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Test-only accessors for the external test package (topology_test), which
// uses the generated mocks and therefore cannot live in this package.

// BuildNodes exposes buildNodes.
func BuildNodes(l *Layout) []Node { return buildNodes(l) }

// IsSymmetric exposes isSymmetric.
func IsSymmetric(setSize, nodeCount int) bool { return isSymmetric(setSize, nodeCount) }

// Canonical exposes the canonical form hashed by Layout.Hash.
func Canonical(l *Layout) string { return l.canonical() }

// ParseEndpoint exposes the endpoint parser of the service.
func ParseEndpoint(svc Service, raw string) (domain.Endpoint, error) {
	return svc.(*service).parseEndpoint(raw)
}

// Defaults reports the dependencies and ports the service resolved.
func Defaults(svc Service) (hosts HostResolver, ids IDGenerator, logger *slog.Logger, defaultPort, localPort int) {
	s := svc.(*service)
	return s.deps.Hosts, s.deps.IDs, s.deps.Logger, s.opts.defaultPort, s.opts.localPort
}
