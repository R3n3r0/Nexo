package topology

import "errors"

// Sentinel errors returned by the topology service. They are wrapped with
// additional context through fmt.Errorf("...: %w", err) and can be matched with
// errors.Is. The integrator may move them to internal/domain.
var (
	// ErrNoEndpoints is returned when no argument was given.
	ErrNoEndpoints = errors.New("topology: no endpoints specified")
	// ErrInvalidEllipsis is returned for a malformed or unsupported ellipsis pattern.
	ErrInvalidEllipsis = errors.New("topology: invalid ellipsis pattern")
	// ErrTooManyEndpoints is returned when an expansion exceeds the supported size.
	ErrTooManyEndpoints = errors.New("topology: too many endpoints")
	// ErrMixedEllipsis is returned when only some arguments contain ellipsis patterns.
	ErrMixedEllipsis = errors.New("topology: either all or none of the arguments must contain ellipsis patterns")
	// ErrInvalidEndpoint is returned for an endpoint that cannot be parsed.
	ErrInvalidEndpoint = errors.New("topology: invalid endpoint")
	// ErrInvalidScheme is returned for a URL scheme other than http or https.
	ErrInvalidScheme = errors.New("topology: unsupported URL scheme (use http or https)")
	// ErrRelativePath is returned for a local drive path that is not absolute.
	ErrRelativePath = errors.New("topology: drive path must be absolute")
	// ErrRootPath is returned when a drive path is the file system root.
	ErrRootPath = errors.New("topology: drive path cannot be the root directory")
	// ErrMixedEndpoints is returned when local paths and URLs are mixed.
	ErrMixedEndpoints = errors.New("topology: cannot mix local paths and URL endpoints")
	// ErrDuplicateEndpoint is returned when the same drive appears twice.
	ErrDuplicateEndpoint = errors.New("topology: duplicate endpoint")
	// ErrInvalidSetSize is returned when no erasure set size in [2..16] divides the
	// drives of a pool uniformly across its nodes.
	ErrInvalidSetSize = errors.New("topology: cannot split the drives into erasure sets of 2 to 16 drives distributed uniformly across nodes")
	// ErrAsymmetricPool is returned when the nodes of a pool expose a different number of drives.
	ErrAsymmetricPool = errors.New("topology: all nodes of a pool must expose the same number of drives")
	// ErrInvalidParity is returned when the configured parity is out of range for the set size.
	ErrInvalidParity = errors.New("topology: invalid parity for erasure set size")
	// ErrNoLocalEndpoint is returned when none of the URL endpoints belongs to this node.
	ErrNoLocalEndpoint = errors.New("topology: no endpoint belongs to the local host")
	// ErrAmbiguousLocalNode is returned when more than one node resolves to this host.
	ErrAmbiguousLocalNode = errors.New("topology: more than one node resolves to the local host")
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
