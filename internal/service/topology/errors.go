package topology

import (
	"errors"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Sentinel errors returned by the topology service. Errors caused by the
// command line arguments wrap domain.ErrInvalidArgument so that callers can
// treat them uniformly; all of them are enriched with fmt.Errorf("...: %w", err)
// and matched with errors.Is.
var (
	// ErrNoEndpoints is returned when no argument was given.
	ErrNoEndpoints = fmt.Errorf("%w: topology: no endpoints specified", domain.ErrInvalidArgument)
	// ErrInvalidEllipsis is returned for a malformed or unsupported ellipsis pattern.
	ErrInvalidEllipsis = fmt.Errorf("%w: topology: invalid ellipsis pattern", domain.ErrInvalidArgument)
	// ErrTooManyEndpoints is returned when an expansion exceeds the supported size.
	ErrTooManyEndpoints = fmt.Errorf("%w: topology: too many endpoints", domain.ErrInvalidArgument)
	// ErrMixedEllipsis is returned when only some arguments contain ellipsis patterns.
	ErrMixedEllipsis = fmt.Errorf("%w: topology: either all or none of the arguments must contain ellipsis patterns", domain.ErrInvalidArgument)
	// ErrInvalidEndpoint is returned for an endpoint that cannot be parsed.
	ErrInvalidEndpoint = fmt.Errorf("%w: topology: invalid endpoint", domain.ErrInvalidArgument)
	// ErrInvalidScheme is returned for a URL scheme other than http or https.
	ErrInvalidScheme = fmt.Errorf("%w: topology: unsupported URL scheme (use http or https)", domain.ErrInvalidArgument)
	// ErrRelativePath is returned for a local drive path that is not absolute.
	ErrRelativePath = fmt.Errorf("%w: topology: drive path must be absolute", domain.ErrInvalidArgument)
	// ErrRootPath is returned when a drive path is the file system root.
	ErrRootPath = fmt.Errorf("%w: topology: drive path cannot be the root directory", domain.ErrInvalidArgument)
	// ErrMixedEndpoints is returned when local paths and URLs are mixed.
	ErrMixedEndpoints = fmt.Errorf("%w: topology: cannot mix local paths and URL endpoints", domain.ErrInvalidArgument)
	// ErrMixedSchemes is returned when http and https endpoints are mixed.
	ErrMixedSchemes = fmt.Errorf("%w: topology: cannot mix http and https endpoints", domain.ErrInvalidArgument)
	// ErrDuplicateEndpoint is returned when the same drive appears twice.
	ErrDuplicateEndpoint = fmt.Errorf("%w: topology: duplicate endpoint", domain.ErrInvalidArgument)
	// ErrInvalidSetSize is returned when no erasure set size in [2..16] divides the
	// drives of a pool uniformly across its nodes.
	ErrInvalidSetSize = fmt.Errorf("%w: topology: cannot split the drives into erasure sets of 2 to 16 drives distributed uniformly across nodes", domain.ErrInvalidArgument)
	// ErrAsymmetricPool is returned when the nodes of a pool expose a different number of drives.
	ErrAsymmetricPool = fmt.Errorf("%w: topology: all nodes of a pool must expose the same number of drives", domain.ErrInvalidArgument)
	// ErrInvalidParity is returned when the configured parity is out of range for the set size.
	ErrInvalidParity = fmt.Errorf("%w: topology: invalid parity for erasure set size", domain.ErrInvalidArgument)
	// ErrNoLocalEndpoint is returned when none of the URL endpoints belongs to this node.
	ErrNoLocalEndpoint = fmt.Errorf("%w: topology: no endpoint belongs to the local host", domain.ErrInvalidArgument)
	// ErrAmbiguousLocalNode is returned when more than one node resolves to this host.
	ErrAmbiguousLocalNode = fmt.Errorf("%w: topology: more than one node resolves to the local host", domain.ErrInvalidArgument)

	// ErrInvalidLayout is returned by Layout.Validate for an inconsistent layout.
	ErrInvalidLayout = errors.New("topology: invalid layout")
	// ErrInvalidFormat is returned for a malformed format.json document.
	ErrInvalidFormat = errors.New("topology: invalid format.json")
	// ErrFormatVersion is returned for an unsupported format.json version.
	ErrFormatVersion = errors.New("topology: unsupported format.json version")
	// ErrFormatMismatch is returned when the formats of different drives disagree.
	ErrFormatMismatch = errors.New("topology: format.json mismatch between drives")
	// ErrFormatModeMismatch is returned when a drive formatted in one mode is used in another.
	ErrFormatModeMismatch = errors.New("topology: format.json mode is incompatible with the deployment mode")
	// ErrFormatLayoutMismatch is returned when format.json sets do not match the pool layout.
	ErrFormatLayoutMismatch = errors.New("topology: format.json sets do not match the pool layout")
)
