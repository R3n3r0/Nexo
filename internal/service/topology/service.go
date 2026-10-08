package topology

import (
	"context"
	"log/slog"
	"net"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Service turns the server command line into a validated deployment Layout and
// manages the format.json documents that record the layout on the drives.
//
// Typical usage during bootstrap:
//
//	layout, err := svc.Parse(ctx, os.Args[2:])
//	...
//	formats := svc.NewFormats(ctx, layout, poolIdx, deploymentID) // only when the pool is fresh
//	ref, err := svc.CheckFormats(ctx, layout, poolIdx, formatsReadFromDrives)
type Service interface {
	// Parse expands the ellipsis patterns of args, validates the endpoints,
	// detects the deployment mode, splits every pool into erasure sets and
	// marks the endpoints that belong to the local node.
	//
	// When every argument contains an ellipsis pattern each argument is a pool;
	// when none does, all the arguments form a single pool (mixing both styles
	// is an error). Endpoints are canonical: local paths are absolute and
	// cleaned, URL endpoints have a lower-cased host and an explicit port.
	// Errors caused by the arguments wrap domain.ErrInvalidArgument.
	Parse(ctx context.Context, args []string) (*Layout, error)

	// IsLocal reports whether ep belongs to the node running this process: local
	// paths are always local, URL endpoints are local when the port equals the
	// local server port and the host is the local hostname, a loopback address or
	// an address assigned to a local network interface.
	IsLocal(ctx context.Context, ep domain.Endpoint) (bool, error)

	// NewFormats builds a fresh FormatV1 for every drive of the given pool of
	// layout, all sharing the deployment id and the same set map. The result is
	// ordered like Pool.Endpoints. A new drive id is generated for every drive.
	NewFormats(ctx context.Context, layout *Layout, poolIdx int, deploymentID string) ([]*FormatV1, error)

	// CheckFormats verifies that the formats read from the drives of a pool are
	// consistent with each other (same deployment id, mode, sets) and compatible
	// with the pool layout (same mode family, set count and set drive count).
	// Nil entries represent drives that could not be read and are ignored. It
	// returns the reference format on success.
	CheckFormats(ctx context.Context, layout *Layout, poolIdx int, formats []*FormatV1) (*FormatV1, error)
}

// HostResolver abstracts the host identity lookups used to decide whether an
// endpoint is local. The OS implementation is used by default; tests inject
// the generated mock.
type HostResolver interface {
	// Hostname returns the host name of the local machine.
	Hostname(ctx context.Context) (string, error)
	// LookupHost resolves host to its IP addresses.
	LookupHost(ctx context.Context, host string) ([]string, error)
	// InterfaceAddrs returns the addresses assigned to the local network interfaces.
	InterfaceAddrs(ctx context.Context) ([]net.Addr, error)
}

// IDGenerator produces unique identifiers for deployments and drives.
type IDGenerator interface {
	// NewID returns a new unique identifier. It is a pure accessor (no I/O
	// beyond the random source, never blocks): the documented exception to
	// the context rule.
	NewID() string
}

// Deps lists the dependencies of the implementation. Every field is optional:
// nil values are replaced by OS-backed defaults.
type Deps struct {
	// Hosts resolves the local host identity (default: OS resolver).
	Hosts HostResolver
	// IDs generates deployment and drive identifiers (default: UUID v4).
	IDs IDGenerator
	// Logger receives structured diagnostics (default: slog.Default()).
	Logger *slog.Logger
}
