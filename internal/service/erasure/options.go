package erasure

import (
	"crypto/sha256"
	"time"
)

// Defaults of the codec options.
const (
	// DefaultBlockSize is the default erasure block size (1 MiB).
	DefaultBlockSize int64 = 1 << 20
	// DefaultHedgeDelay is how long Decode waits for a shard block before
	// asking one more shard.
	DefaultHedgeDelay = 250 * time.Millisecond
	// MaxShards bounds D+P (Reed-Solomon over GF(2^8)).
	MaxShards = 256
)

// Option configures a codec (New) or a factory (NewFactory).
type Option func(*options)

type options struct {
	data, parity int
	blockSize    int64
	key          []byte
	hedgeDelay   time.Duration
}

func defaultOptions() options {
	return options{blockSize: DefaultBlockSize, key: DeriveBitrotKey(""), hedgeDelay: DefaultHedgeDelay}
}

// WithShards sets the layout of the codec built by New (ignored by
// NewFactory, whose callers pass the layout to Factory.Codec).
func WithShards(data, parity int) Option {
	return func(o *options) { o.data, o.parity = data, parity }
}

// WithBlockSize sets the block size (default 1 MiB); for a factory it is
// the size used when a layout leaves it zero.
func WithBlockSize(n int64) Option {
	return func(o *options) {
		if n > 0 {
			o.blockSize = n
		}
	}
}

// WithDeploymentID keys the bitrot hashes with the key derived from the
// deployment id (DeriveBitrotKey). Every node of a deployment must use the
// same id: the hashes are part of the on-disk format.
func WithDeploymentID(id string) Option {
	return func(o *options) { o.key = DeriveBitrotKey(id) }
}

// WithBitrotKey sets the 32-byte HighwayHash key directly; other lengths
// are ignored.
func WithBitrotKey(key []byte) Option {
	return func(o *options) {
		if len(key) == 32 {
			o.key = append([]byte(nil), key...)
		}
	}
}

// WithHedgeDelay sets how long Decode waits for the shards of a block
// before reading one more shard; zero or a negative value disables the
// time-based hedging (shards are then added on errors only).
func WithHedgeDelay(d time.Duration) Option {
	return func(o *options) { o.hedgeDelay = d }
}

// DeriveBitrotKey returns the 32-byte HighwayHash key of a deployment:
// SHA-256 of a domain separator and the deployment id.
func DeriveBitrotKey(deploymentID string) []byte {
	sum := sha256.Sum256([]byte("nexo/erasure/bitrot/v1\x00" + deploymentID))
	return sum[:]
}
