// Package erasure implements the Reed-Solomon erasure coding of the object
// layer and the bitrot protection of the shards it produces.
//
// An object part is cut into blocks of BlockSize bytes (1 MiB by default);
// every block is split into DataShards data shards and ParityShards parity
// shards of ceil(blockLen/DataShards) bytes, and shard i of every block is
// appended to the shard file of drive i. On disk each shard block is
// preceded by its HighwayHash-256 (keyed by a key derived from the
// deployment id): a shard file is the sequence hash(b0) b0 hash(b1) b1 ...
// so that corruption is detected per block while streaming.
//
// Any DataShards healthy shards of a block are enough to decode it: up to
// ParityShards shards may be missing, unreadable or corrupt.
//
// The implementation (New, NewFactory) wraps github.com/klauspost/reedsolomon
// (SIMD accelerated) and github.com/minio/highwayhash; neither library
// leaves this package.
package erasure

import (
	"context"
	"io"
	"log/slog"
)

// Deps are the dependencies of the codec implementations. Every field is
// optional: nil values are replaced by defaults.
type Deps struct {
	// Logger receives the diagnostics of the decoder (shards set aside).
	Logger *slog.Logger
}

// Layout is the erasure layout of a codec.
type Layout struct {
	// DataShards (D) and ParityShards (P) split every block into D+P shards;
	// D >= 1, P >= 0 and D+P <= 256.
	DataShards   int
	ParityShards int
	// BlockSize is the size of the blocks the content is cut into; zero
	// selects the default of the factory (1 MiB).
	BlockSize int64
}

// Factory builds the codecs of the layouts an object layer meets: the
// layouts of its storage classes and those recorded with existing objects.
// Codecs are cached: a layout always yields the same codec.
type Factory interface {
	// Codec returns the codec of layout, or an error wrapping
	// ErrInvalidLayout when the layout is not valid.
	Codec(ctx context.Context, layout Layout) (Codec, error)
}

// StreamFunc opens a sequential stream over a shard file, from byte offset
// to its end (typically StorageAPI.ReadFileStream with length -1).
type StreamFunc func(ctx context.Context, offset int64) (io.ReadCloser, error)

// ShardReader reads the verified shard blocks of one shard file. It is
// bound to the context it was created with (Codec.BitrotReader), like an
// http.Request: its methods are the standard io ones.
type ShardReader interface {
	// ReadAt reads the shard block that starts at shard offset off (a
	// multiple of Codec.ShardSize, hashes excluded) into p, whose length
	// is the length of that shard block, and verifies it against its
	// bitrot hash. A corrupt block fails with a *BitrotError (ErrBitrot),
	// a short file with ErrShardTruncated. Calls are expected in
	// increasing offset order (the stream is reopened otherwise) and must
	// not overlap.
	ReadAt(p []byte, off int64) (int, error)
	// Close releases the underlying stream. It may be called while a
	// ReadAt is in progress, which then fails.
	Close() error
}

// DecodeResult reports the outcome of Decode and Heal.
type DecodeResult struct {
	// Written is the number of bytes written to the destination.
	Written int64
	// ShardErrs has one entry per shard: the error that set the shard
	// aside (unreadable, truncated or corrupt), nil for the shards that
	// were read successfully, not needed, or given as nil readers.
	ShardErrs []error
}

// Codec erasure-codes content with one layout. The layout accessors are
// pure (no I/O, never blocking), the documented exception to the context
// rule; the coding methods take the context first.
type Codec interface {
	// DataShards returns D.
	DataShards() int
	// ParityShards returns P.
	ParityShards() int
	// BlockSize returns the block size.
	BlockSize() int64
	// ShardSize returns the shard length of a full block:
	// ceil(BlockSize/DataShards).
	ShardSize() int64
	// ShardFileSize returns the number of shard bytes (hashes excluded)
	// every drive stores for a part of totalLength bytes.
	ShardFileSize(totalLength int64) int64
	// BitrotFileSize returns the size on disk of a shard file of a part of
	// totalLength bytes: ShardFileSize plus one hash per block.
	BitrotFileSize(totalLength int64) int64
	// BitrotSum returns the HighwayHash-256 of p under the deployment key
	// (the hash of inline content). It is a pure computation.
	BitrotSum(p []byte) []byte

	// EncodeBlock splits one block (at most BlockSize bytes, not empty)
	// into D+P freshly allocated shards.
	EncodeBlock(ctx context.Context, block []byte) ([][]byte, error)
	// Reconstruct rebuilds the missing shards of a block in place (nil or
	// empty entries are missing); with dataOnly only the data shards are
	// rebuilt. It fails with ErrTooFewShards when fewer than D shards are
	// present.
	Reconstruct(ctx context.Context, shards [][]byte, dataOnly bool) error
	// Encode reads src to its end, block by block, and writes shard i of
	// every block to writers[i] (wrap them with BitrotWriter to store the
	// hashes); the writes of a block run in parallel. Nil writers are
	// skipped; a writer that fails is set to nil in the slice and left
	// aside, and the encoding goes on while at least quorum writers
	// remain, failing with ErrWriteQuorum otherwise. A read error of src
	// is returned as is. It returns the number of bytes read from src.
	Encode(ctx context.Context, src io.Reader, writers []io.Writer, quorum int) (int64, error)
	// Decode writes [offset, offset+length) of a part of totalLength bytes
	// to dst, reading the blocks from readers (one per shard, nil when
	// unavailable). It starts with D shards, preferring the data shards,
	// and adds another shard whenever one fails or does not answer within
	// the hedge delay; a shard set aside is not used again. It fails with
	// ErrTooFewShards when fewer than D shards of a block can be read.
	Decode(ctx context.Context, dst io.Writer, readers []ShardReader, offset, length, totalLength int64) (DecodeResult, error)
	// Heal rebuilds the shard files of a part of totalLength bytes: every
	// block is read from the healthy readers, reconstructed, and shard i
	// is written to writers[i] for every non-nil writer (the shards to
	// rebuild; their readers must be nil). It fails when a block cannot
	// be read or no writer is left.
	Heal(ctx context.Context, writers []io.Writer, readers []ShardReader, totalLength int64) (DecodeResult, error)

	// BitrotWriter wraps the destination of one shard file: every Write
	// (one shard block) is stored as hash || block. Writes fail once ctx
	// is done.
	BitrotWriter(ctx context.Context, w io.Writer) io.Writer
	// BitrotReader returns the reader of one shard file, whose stream is
	// opened with open on the first read (no I/O happens before).
	BitrotReader(ctx context.Context, open StreamFunc) ShardReader
}
