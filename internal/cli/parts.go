package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/r3n3r0/nexo/internal/domain"
)

// part is one part of a multipart upload or copy.
type part struct {
	number int32
	// offset and size locate the part in the object.
	offset int64
	size   int64
	// body is the content of an uploaded part (nil for a copied range).
	body io.ReadSeeker
	// buf is the buffer backing body, returned to the pool once the part
	// is uploaded.
	buf []byte
}

// partSource produces the parts of an upload in order; ok is false after
// the last part.
type partSource interface {
	next(pool *bufferPool) (p part, ok bool, err error)
}

// sectionParts cuts a random-access body of known size into parts.
type sectionParts struct {
	ra       io.ReaderAt
	size     int64
	partSize int64
	parts    int
	n        int
}

func (s *sectionParts) next(*bufferPool) (part, bool, error) {
	if s.n >= s.parts {
		return part{}, false, nil
	}
	offset := int64(s.n) * s.partSize
	size := min(s.partSize, s.size-offset)
	s.n++
	return part{number: int32(s.n), offset: offset, size: size, body: io.NewSectionReader(s.ra, offset, size)}, true, nil // #nosec G115 -- a part number, at most the 10000 parts of a planned upload
}

// rangeParts cuts an object of known size into the ranges of an
// UploadPartCopy.
type rangeParts struct {
	size     int64
	partSize int64
	parts    int
	n        int
}

func (s *rangeParts) next(*bufferPool) (part, bool, error) {
	if s.n >= s.parts {
		return part{}, false, nil
	}
	offset := int64(s.n) * s.partSize
	size := min(s.partSize, s.size-offset)
	s.n++
	return part{number: int32(s.n), offset: offset, size: size}, true, nil // #nosec G115 -- a part number, at most the 10000 parts of a planned upload
}

// streamParts reads the parts of a sequential body into buffers. With a
// known size (size >= 0) every part has partSize bytes but the last and the
// body must hold exactly size bytes; with an unknown size (size < 0) part n
// has StreamPartSize(n, partSize) bytes and first, when set, is the content
// of part 1 already read.
type streamParts struct {
	r        io.Reader
	size     int64
	partSize int64
	first    []byte
	n        int
	read     int64
	done     bool
}

func (s *streamParts) next(pool *bufferPool) (part, bool, error) {
	if s.done {
		return part{}, false, nil
	}
	number := s.n + 1
	if number > MaxParts {
		return part{}, false, fmt.Errorf("%w: the stream exceeds %d parts", domain.ErrEntityTooLarge, MaxParts)
	}
	var buf []byte
	if number == 1 && s.first != nil {
		buf = s.first
		s.first = nil
	} else {
		want := s.partSize
		if s.size < 0 {
			want = StreamPartSize(number, s.partSize)
		} else {
			want = min(want, s.size-s.read)
		}
		if want <= 0 {
			s.done = true
			return part{}, false, nil
		}
		buf = pool.get(want)
		n, err := io.ReadFull(s.r, buf)
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			if s.size >= 0 {
				pool.put(buf)
				return part{}, false, fmt.Errorf("read part %d: %w (%d of %d bytes)", number, io.ErrUnexpectedEOF, s.read+int64(n), s.size)
			}
			buf = buf[:n]
			s.done = true
			if n == 0 {
				pool.put(buf)
				return part{}, false, nil
			}
		case err != nil:
			pool.put(buf)
			return part{}, false, fmt.Errorf("read part %d: %w", number, err)
		}
	}
	s.n = number
	offset := s.read
	s.read += int64(len(buf))
	if s.size >= 0 && s.read >= s.size {
		s.done = true
	}
	return part{number: int32(number), offset: offset, size: int64(len(buf)), body: bytes.NewReader(buf), buf: buf}, true, nil // #nosec G115 -- a part number, at most the 10000 parts of an upload
}

// bufferPool recycles the part buffers of one upload: at most cap(free)
// buffers are kept.
type bufferPool struct {
	free chan []byte
}

func newBufferPool(n int) *bufferPool { return &bufferPool{free: make(chan []byte, n)} }

func (p *bufferPool) get(size int64) []byte {
	select {
	case b := <-p.free:
		if int64(cap(b)) >= size {
			return b[:size]
		}
	default:
	}
	return make([]byte, size)
}

func (p *bufferPool) put(b []byte) {
	if b == nil {
		return
	}
	select {
	case p.free <- b[:cap(b)]:
	default:
	}
}

// runParts uploads the parts of src with parallel workers (DefaultParallel
// when <= 0) and returns them sorted by number with the total size. The
// first failure cancels the other workers; progress, when not nil, is
// called with the size of every uploaded part.
func runParts(ctx context.Context, src partSource, parallel int, progress func(int64),
	upload func(context.Context, part) (types.CompletedPart, error),
) ([]types.CompletedPart, int64, error) {
	if parallel <= 0 {
		parallel = DefaultParallel
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pool := newBufferPool(parallel + 1)
	jobs := make(chan part)
	var (
		mu        sync.Mutex
		firstErr  error
		completed []types.CompletedPart
		total     int64
		wg        sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	for range parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				if ctx.Err() != nil {
					pool.put(p.buf)
					continue
				}
				done, err := upload(ctx, p)
				pool.put(p.buf)
				if err != nil {
					fail(fmt.Errorf("part %d: %w", p.number, err))
					continue
				}
				mu.Lock()
				completed = append(completed, done)
				total += p.size
				mu.Unlock()
				if progress != nil {
					progress(p.size)
				}
			}
		}()
	}
produce:
	for {
		p, ok, err := src.next(pool)
		if err != nil {
			fail(err)
			break
		}
		if !ok {
			break
		}
		select {
		case jobs <- p:
		case <-ctx.Done():
			pool.put(p.buf)
			break produce
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}
	if firstErr != nil {
		return nil, 0, firstErr
	}
	sort.Slice(completed, func(i, j int) bool {
		return aws.ToInt32(completed[i].PartNumber) < aws.ToInt32(completed[j].PartNumber)
	})
	return completed, total, nil
}
