package erasure

import (
	"encoding/hex"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Sentinel errors of the codec. Every sentinel wraps a domain sentinel
// (docs/12 §12.2 rule 5) so that the object layer and the transports keep
// matching the domain meaning with errors.Is.
var (
	// ErrInvalidLayout reports an unusable layout or argument.
	ErrInvalidLayout = fmt.Errorf("%w: erasure: invalid layout", domain.ErrInvalidArgument)
	// ErrTooFewShards reports a block that cannot be decoded: fewer than D
	// shards are readable.
	ErrTooFewShards = fmt.Errorf("%w: erasure: too few readable shards", domain.ErrReadQuorum)
	// ErrWriteQuorum reports an encoding that lost too many shard writers.
	ErrWriteQuorum = fmt.Errorf("%w: erasure: too few shard writers", domain.ErrWriteQuorum)
	// ErrBitrot reports a shard block whose content does not match its
	// hash: the drive copy is corrupt and must be healed.
	ErrBitrot = fmt.Errorf("%w: erasure: bitrot detected", domain.ErrFaultyDisk)
	// ErrShardTruncated reports a shard file shorter than its part.
	ErrShardTruncated = fmt.Errorf("%w: erasure: shard truncated", domain.ErrFaultyDisk)
)

// BitrotError is the typed error of a corrupt shard block. It wraps
// ErrBitrot (and so domain.ErrFaultyDisk).
type BitrotError struct {
	// Offset is the shard offset of the block (hashes excluded).
	Offset int64
	// Want is the stored hash, Got the hash of the content read.
	Want []byte
	Got  []byte
}

// Error implements error.
func (e *BitrotError) Error() string {
	return fmt.Sprintf("erasure: bitrot detected in the shard block at offset %d: hash %s, want %s",
		e.Offset, hex.EncodeToString(e.Got), hex.EncodeToString(e.Want))
}

// Unwrap returns ErrBitrot.
func (e *BitrotError) Unwrap() error { return ErrBitrot }
