// Package sysstore keeps the system documents of a deployment (identities,
// named policies, dynamic configuration, bucket metadata, share links,
// session revocations) on the system volume of the drives of the first
// erasure set (.nexo.sys/config), whatever the deployment mode: the drives
// are storage.StorageAPI values, local or remote (internode RPC), so that
// every node of a cluster reads and writes the same documents (docs/03
// §3.9: "metadati di bucket/IAM: scritti su quorum").
//
// Every document is replicated whole on every drive with a version stamp
// (documents are small: a few KiB). A write takes the distributed write
// lock of the document, reads the current stamp from a read quorum, and
// writes the next stamp to at least a write quorum of drives (W = N/2+1);
// a read collects a read quorum (R = N-W+1, so that every read overlaps
// the last successful write) and returns the newest copy, rewriting the
// stale drives on the way (read repair). Deletes write a tombstone, so
// that a drive that missed the delete cannot resurrect the document.
package sysstore

import (
	"context"
	"log/slog"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Store is a small replicated document store. Paths are slash separated,
// relative to the root of the store (.nexo.sys/config by default), made
// of non-empty segments of letters, digits and "._-~+=@," (no "." or ".."
// segment); other paths fail with domain.ErrInvalidArgument. Errors are
// domain sentinels: ErrNotFound (a domain.ErrObjectNotFound) for a missing
// document, domain.ErrReadQuorum / domain.ErrWriteQuorum when too few
// drives answer.
type Store interface {
	// Get returns the newest copy of the document at path.
	Get(ctx context.Context, path string) ([]byte, error)
	// Put creates or replaces the document at path.
	Put(ctx context.Context, path string, data []byte) error
	// Create stores data at path unless a document is there already, and
	// returns the document stored at path afterwards: data, or the
	// existing one (several nodes creating the same document agree on a
	// single value).
	Create(ctx context.Context, path string, data []byte) ([]byte, error)
	// Delete removes the document at path; deleting a missing document is
	// not an error.
	Delete(ctx context.Context, path string) error
	// List returns the documents directly under dir ("" is the root),
	// keyed by their name relative to dir. Sub-directories are not
	// listed.
	List(ctx context.Context, dir string) (map[string][]byte, error)
}

// Deps are the dependencies of the store.
type Deps struct {
	// Drives hold the documents (required): the drives of the first
	// erasure set of the first pool, local or remote. A nil drive is an
	// offline one.
	Drives []storage.StorageAPI
	// Locks serializes the writes of a document (required): the local
	// locker in a single-node deployment, the distributed one (dsync) in a
	// cluster.
	Locks lock.Locker
	// Clock stamps the versions (default: wall clock).
	Clock  clock.Clock
	Logger *slog.Logger
}

// Option configures the store.
type Option func(*options)
