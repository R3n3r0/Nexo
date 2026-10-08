package object

import (
	"context"
	"errors"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// walkBuffer is the number of entries a drive walk produces ahead of the
// merge.
const walkBuffer = 256

// driveStream is the walk of one drive, consumed entry by entry.
type driveStream struct {
	ch   chan storage.WalkEntry
	head *storage.WalkEntry
	done bool
	err  error
	// errc receives the result of the walk once ch is closed.
	errc chan error
}

// advance loads the next entry of the stream into head.
func (s *driveStream) advance() {
	e, ok := <-s.ch
	if !ok {
		s.head, s.done = nil, true
		s.err = <-s.errc
		return
	}
	s.head = &e
}

// entryBefore orders walk entries in S3 key order, an object before the
// prefix of the same name.
func entryBefore(aName string, aDir bool, bName string, bDir bool) bool {
	if aName != bName {
		return aName < bName
	}
	return !aDir && bDir
}

// WalkVersions merges the walks of every online drive of the set (all in
// S3 key order) and reports an entry when a read quorum of drives hold it
// (listing quorum): a prefix reported by fewer drives (the leftover of a
// failed write) is hidden, and only the versions a read quorum of drives
// agree on are reported. A drive whose walk fails is left out; the walk
// fails with domain.ErrReadQuorum when fewer than a read quorum of drives
// are left.
func (e *erasureSet) WalkVersions(ctx context.Context, opts storage.WalkDirOptions, fn func(walkEntry) error) error {
	opts.Limit = 0
	quorum := e.readQuorum()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var streams []*driveStream
	for _, d := range e.drives {
		if !isOnline(d) {
			continue
		}
		s := &driveStream{ch: make(chan storage.WalkEntry, walkBuffer), errc: make(chan error, 1)}
		streams = append(streams, s)
		go func() {
			err := d.WalkDir(ctx, opts, func(we storage.WalkEntry) error {
				select {
				case s.ch <- we:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			s.errc <- err
			close(s.ch)
		}()
	}
	// The goroutines end once ctx is canceled (deferred) and their channel
	// drained.
	defer func() {
		cancel()
		for _, s := range streams {
			for !s.done {
				s.advance()
			}
		}
	}()
	if len(streams) < quorum {
		return fmt.Errorf("%w: set %d of pool %d has %d of %d drives online, %d needed to list",
			domain.ErrReadQuorum, e.setIdx+1, e.poolIdx+1, len(streams), e.n(), quorum)
	}
	for _, s := range streams {
		s.advance()
	}
	perDrive := make([][]storage.FileInfo, 0, len(streams))
	for {
		if err := e.checkStreams(streams, quorum); err != nil {
			return err
		}
		var min *storage.WalkEntry
		for _, s := range streams {
			if s.head != nil && (min == nil || entryBefore(s.head.Name, s.head.IsDir, min.Name, min.IsDir)) {
				min = s.head
			}
		}
		if min == nil {
			return nil
		}
		name, isDir := min.Name, min.IsDir
		count := 0
		perDrive = perDrive[:0]
		for _, s := range streams {
			if s.head == nil || s.head.Name != name || s.head.IsDir != isDir {
				continue
			}
			count++
			if !isDir {
				versions, err := storage.DecodeFileInfos(opts.Bucket, name, s.head.Metadata)
				if err != nil {
					e.logger.WarnContext(ctx, "object: skipping unreadable metadata", "bucket", opts.Bucket, "key", name, "err", err)
				} else {
					perDrive = append(perDrive, versions)
				}
			}
			s.advance()
		}
		if count < quorum {
			continue
		}
		entry := walkEntry{Name: name, IsDir: isDir}
		if !isDir {
			entry.Versions = mergeVersions(perDrive, quorum)
			if len(entry.Versions) == 0 {
				continue
			}
		}
		if err := fn(entry); err != nil {
			return err
		}
	}
}

// checkStreams fails when the walks that failed leave fewer than quorum
// drives.
func (e *erasureSet) checkStreams(streams []*driveStream, quorum int) error {
	healthy := 0
	var errs []error
	for _, s := range streams {
		if s.done && s.err != nil {
			errs = append(errs, s.err)
			continue
		}
		healthy++
	}
	if healthy >= quorum {
		return nil
	}
	if err := reduceErrs(errs, 1, nil); err != nil && !errors.Is(err, domain.ErrDiskNotFound) && !errors.Is(err, domain.ErrFaultyDisk) {
		return err
	}
	return fmt.Errorf("%w: set %d of pool %d: %d drives left to list, %d needed", domain.ErrReadQuorum, e.setIdx+1, e.poolIdx+1, healthy, quorum)
}
