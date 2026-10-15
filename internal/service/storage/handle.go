package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
)

// fileHandle implements FileHandle on a file of a local drive: every
// stream reads the open file at its own offset (pread), so the handle
// serves any offset, in any order, for as long as it is open.
type fileHandle struct {
	f    *os.File
	size int64
	// name is volume/path, for the errors.
	name string

	mu      sync.Mutex
	streams int
	closed  bool
}

var _ FileHandle = (*fileHandle)(nil)

// Size implements FileHandle.
func (h *fileHandle) Size() int64 { return h.size }

// ReadStream implements FileHandle.
func (h *fileHandle) ReadStream(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch {
	case offset < 0 || offset > h.size:
		return nil, fmt.Errorf("%w: offset %d outside %s (%d bytes)", domain.ErrInvalidRange, offset, h.name, h.size)
	case length < 0:
		length = h.size - offset
	case length > h.size-offset:
		return nil, fmt.Errorf("%w: %s has %d bytes, want [%d, %d)", ErrFileCorrupt, h.name, h.size, offset, offset+length)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, fmt.Errorf("storage: handle of %s: %w", h.name, os.ErrClosed)
	}
	h.streams++
	return &handleStream{SectionReader: io.NewSectionReader(h.f, offset, length), h: h}, nil
}

// Close implements FileHandle.
func (h *fileHandle) Close(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	if h.streams == 0 {
		return h.f.Close()
	}
	return nil
}

// release ends a stream: the file is closed with the last stream of a
// closed handle.
func (h *fileHandle) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.streams--
	if h.closed && h.streams == 0 {
		_ = h.f.Close()
	}
}

// handleStream is a stream of a fileHandle.
type handleStream struct {
	*io.SectionReader
	h    *fileHandle
	once sync.Once
}

// Close implements io.Closer.
func (s *handleStream) Close() error {
	s.once.Do(s.h.release)
	return nil
}
