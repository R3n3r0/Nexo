package topology

// DefaultPort is the port assumed for URL endpoints that do not specify one and
// the default local server port used to recognise local endpoints.
const DefaultPort = 9000

// MaxSetDriveCount and MinSetDriveCount bound the size of an erasure set.
const (
	MinSetDriveCount = 2
	MaxSetDriveCount = 16
)

// maxExpansion bounds the number of endpoints a single argument may expand to.
const maxExpansion = 1 << 16

type options struct {
	defaultPort   int
	localPort     int
	setDriveCount int // 0 = automatic
	parity        int // -1 = automatic
}

// Option customises the service.
type Option func(*options)

// WithDefaultPort sets the port assumed for URL endpoints without an explicit port.
func WithDefaultPort(port int) Option {
	return func(o *options) { o.defaultPort = port }
}

// WithLocalPort sets the port the local server listens on; a URL endpoint is
// local only when its port matches it.
func WithLocalPort(port int) Option {
	return func(o *options) { o.localPort = port }
}

// WithSetDriveCount forces the erasure set size instead of computing it. The
// value must be in [MinSetDriveCount, MaxSetDriveCount], divide the drives of
// every pool and distribute them uniformly across nodes, otherwise Parse fails.
func WithSetDriveCount(n int) Option {
	return func(o *options) { o.setDriveCount = n }
}

// WithParity forces the parity shards per erasure set instead of using the
// default (N<=5 -> 2, 6..7 -> 3, >=8 -> 4, never above N/2). The value must be
// in [1, N/2] for erasure deployments.
func WithParity(p int) Option {
	return func(o *options) { o.parity = p }
}

func defaultOptions() options {
	return options{
		defaultPort: DefaultPort,
		localPort:   DefaultPort,
		parity:      -1,
	}
}
