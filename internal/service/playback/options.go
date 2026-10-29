package playback

import "time"

// Defaults of the options.
const (
	// DefaultRefresh is how long a node trusts its copy of the
	// revocations and of the streams of a viewer.
	DefaultRefresh = 10 * time.Second
	// DefaultBurst is the number of key periods a stream gets before the
	// pace of playback applies (a player buffering ahead, a seek).
	DefaultBurst = 3
	// defaultMaxEntries bounds the streams and viewers a node remembers
	// before it forgets the expired ones.
	defaultMaxEntries = 100_000
)

type options struct {
	key        []byte
	refresh    time.Duration
	burst      int
	maxEntries int
}

// Option configures the service.
type Option func(*options)

func newOptions(opts []Option) options {
	o := options{refresh: DefaultRefresh, burst: DefaultBurst, maxEntries: defaultMaxEntries}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WithKey sets the media protection key of the deployment
// (media.DeriveProtectionKey): the tokens are signed with a key derived
// from it.
func WithKey(key []byte) Option {
	return func(o *options) { o.key = append([]byte(nil), key...) }
}

// WithRefresh sets how long a node trusts its copy of the revocations and
// of the streams of a viewer (default DefaultRefresh).
func WithRefresh(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.refresh = d
		}
	}
}

// WithBurst sets the key periods a stream gets before the pace applies
// (default DefaultBurst).
func WithBurst(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.burst = n
		}
	}
}
