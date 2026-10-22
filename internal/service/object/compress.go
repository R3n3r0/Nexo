package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/s2"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Transparent compression (S-OBJ-14). The versions a CompressionPolicy
// selects are stored as an S2 stream per part: every block carries the
// CRC-32C of its content, checked when it is decompressed. The index of
// the blocks of a part (s2.Index) is kept in its PartInfo.Index, so that
// a range read decompresses from the block holding its start, not from the
// beginning of the part. The sizes the clients see (ObjectInfo.Size, the
// part sizes), the ETag and the checksums are those of the uncompressed
// content: metaActualSize records it, PartInfo.Size the stored bytes.
//
// Encrypted versions are never compressed: the length of a compressed
// ciphertext would tell about its plaintext.

// CompressionPolicy decides whether a new version is stored compressed,
// from its key and content type; it is asked at every write, so that a
// change of the configuration applies to the next writes.
type CompressionPolicy func(ctx context.Context, bucket, key, contentType string) bool

const (
	// metaCompression is the compression of the parts of a version
	// (compressionS2). It is internal: never exposed, never copied.
	metaCompression = "x-nexo-internal-compression"
	compressionS2   = "s2"

	// partIndexCompressed is the version of the part index holding the
	// s2.Index of a compressed part (the encryption indexes are versions
	// 1 and 2; encrypted versions are never compressed).
	partIndexCompressed = 0x10

	// minCompressSize is the size under which a body is not compressed:
	// the gain is small and the version could not be inlined.
	minCompressSize = 4 << 10
)

// isCompressed reports whether the parts of a version are compressed.
func isCompressed(meta map[string]string) bool { return meta[metaCompression] == compressionS2 }

// compressFor reports whether a body of size bytes (-1: unknown) written
// to bucket/key is to be compressed. A body small enough to be inlined is
// not: inlining saves more than compression.
func (l *layer) compressFor(ctx context.Context, bucket, key, contentType string, size int64, encrypted bool) bool {
	if l.opts.compression == nil || encrypted {
		return false
	}
	if size >= 0 && (size < minCompressSize || size <= l.opts.inlineLimit) {
		return false
	}
	return l.opts.compression(ctx, bucket, key, contentType)
}

// compressor compresses a body as it is read: Read returns the S2 stream
// of the content of src. The compression runs in its own goroutine, which
// reads src: finish (after the stream was read to the end) or abort (when
// it was not) waits for it, so that src is never read after them.
type compressor struct {
	pr   *io.PipeReader
	done chan struct{}
	idx  []byte
	err  error
}

func newCompressor(src io.Reader) *compressor {
	pr, pw := io.Pipe()
	c := &compressor{pr: pr, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		w := s2.NewWriter(pw, s2.WriterConcurrency(2))
		_, err := io.Copy(w, src)
		idx, cerr := w.CloseIndex()
		if err == nil {
			err = cerr
		}
		c.idx, c.err = idx, err
		_ = pw.CloseWithError(err)
	}()
	return c
}

func (c *compressor) Read(p []byte) (int, error) { return c.pr.Read(p) }

// finish waits for the compression and returns the index of the stream.
// The stream must have been read to its end: if it was not, the
// compression fails instead of waiting for a reader.
func (c *compressor) finish() ([]byte, error) {
	_ = c.pr.CloseWithError(errCompressionAborted)
	<-c.done
	if c.err != nil {
		return nil, c.err
	}
	return encodeCompressionIndex(c.idx), nil
}

// abort stops the compression of a stream not read to the end.
func (c *compressor) abort() {
	_ = c.pr.CloseWithError(errCompressionAborted)
	<-c.done
}

var errCompressionAborted = errors.New("object: compression aborted")

// encodeCompressionIndex returns the part index of a compressed part.
func encodeCompressionIndex(idx []byte) []byte {
	return append([]byte{partIndexCompressed}, idx...)
}

// compressionIndex loads the block index of a compressed part.
func compressionIndex(p storage.PartInfo) (*s2.Index, error) {
	if len(p.Index) < 1 || p.Index[0] != partIndexCompressed {
		return nil, fmt.Errorf("%w: compressed part %d has no block index", domain.ErrFaultyDisk, p.Number)
	}
	var idx s2.Index
	if _, err := idx.Load(p.Index[1:]); err != nil {
		return nil, fmt.Errorf("%w: block index of compressed part %d: %w", domain.ErrFaultyDisk, p.Number, err)
	}
	return &idx, nil
}

// compressedReader reads a range of the content of a compressed version:
// it decompresses every part the range crosses, one part at a time, from
// the block holding the start of the range in the part.
type compressedReader struct {
	ctx       context.Context
	st        store
	bucket    string
	key       string
	fi        storage.FileInfo
	offset    int64
	remaining int64
	// cur decompresses the current part; partLeft is what remains to read
	// of it for the range.
	cur      *s2.Reader
	partLeft int64
	closer   io.Closer
	pinned   []pinnedStream
}

func (l *layer) openCompressed(ctx context.Context, st store, bucket, key string, fi storage.FileInfo, offset, length int64) *compressedReader {
	return &compressedReader{ctx: ctx, st: st, bucket: bucket, key: key, fi: fi, offset: offset, remaining: length}
}

// compressedRead is the read of the part holding an offset of the content:
// the part, the offset and length of the range in its content, and the
// stored offset of the block holding the offset with the content to skip
// in that block.
type compressedRead struct {
	part      storage.PartInfo
	partLen   int64
	storedOff int64
	skip      int64
}

func (r *compressedReader) locate(off, remaining int64) (compressedRead, error) {
	var start int64
	for _, part := range r.fi.Parts {
		size := part.ActualSize
		if off >= start+size {
			start += size
			continue
		}
		partOff := off - start
		idx, err := compressionIndex(part)
		if err != nil {
			return compressedRead{}, fmt.Errorf("object: %s/%s: %w", r.bucket, r.key, err)
		}
		if idx.TotalUncompressed != size {
			return compressedRead{}, fmt.Errorf("%w: object: %s/%s: part %d holds %d bytes, its index %d", domain.ErrFaultyDisk, r.bucket, r.key, part.Number, size, idx.TotalUncompressed)
		}
		cOff, uOff, err := idx.Find(partOff)
		if err != nil || cOff < 0 || cOff > part.Size || uOff > partOff {
			return compressedRead{}, fmt.Errorf("%w: object: %s/%s: block index of part %d: offset %d", domain.ErrFaultyDisk, r.bucket, r.key, part.Number, partOff)
		}
		return compressedRead{part: part, partLen: min(size-partOff, remaining), storedOff: cOff, skip: partOff - uOff}, nil
	}
	return compressedRead{}, fmt.Errorf("%w: offset %d beyond the parts of %s/%s", domain.ErrFaultyDisk, off, r.bucket, r.key)
}

func (r *compressedReader) Read(p []byte) (int, error) {
	for {
		if r.remaining <= 0 {
			return 0, io.EOF
		}
		if r.cur == nil {
			if err := r.openNext(); err != nil {
				return 0, err
			}
		}
		if int64(len(p)) > r.partLeft {
			p = p[:r.partLeft]
		}
		n, err := r.cur.Read(p)
		r.offset += int64(n)
		r.remaining -= int64(n)
		r.partLeft -= int64(n)
		if r.partLeft == 0 {
			r.closeCur()
			if n == 0 {
				continue
			}
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			// The part ended before its recorded size.
			r.closeCur()
			return n, fmt.Errorf("%w: object: read %s/%s: compressed part shorter than its size", domain.ErrFaultyDisk, r.bucket, r.key)
		}
		if err != nil {
			return n, fmt.Errorf("object: read %s/%s: %w", r.bucket, r.key, lockErr(r.ctx, err))
		}
		return n, nil
	}
}

// openNext opens the part holding the current offset, from the block
// holding it.
func (r *compressedReader) openNext() error {
	rd, err := r.locate(r.offset, r.remaining)
	if err != nil {
		return err
	}
	stored := rd.part.Size - rd.storedOff
	var raw io.Reader
	if inlineData(r.fi) {
		if rd.part.Size > int64(len(r.fi.Data)) {
			return fmt.Errorf("%w: inline data of %d bytes, want %d", domain.ErrFaultyDisk, len(r.fi.Data), rd.part.Size)
		}
		raw = bytes.NewReader(r.fi.Data[rd.storedOff:rd.part.Size])
	} else if rc := takePinned(&r.pinned, r.offset); rc != nil {
		raw, r.closer = rc, rc
	} else {
		rc, err := r.st.ReadData(r.ctx, r.bucket, r.key, r.fi, rd.part, rd.storedOff, stored)
		if err != nil {
			return fmt.Errorf("object: open part %d of %s/%s: %w", rd.part.Number, r.bucket, r.key, lockErr(r.ctx, err))
		}
		raw, r.closer = rc, rc
	}
	dec := s2.NewReader(raw, s2.ReaderIgnoreStreamIdentifier())
	if rd.skip > 0 {
		if err := dec.Skip(rd.skip); err != nil {
			r.closeCur()
			return fmt.Errorf("%w: object: read %s/%s: part %d: %w", domain.ErrFaultyDisk, r.bucket, r.key, rd.part.Number, err)
		}
	}
	r.cur, r.partLeft = dec, rd.partLen
	return nil
}

// pin implements pinnable: the stored streams of the parts of the range
// are opened up front (inline data needs none).
func (r *compressedReader) pin(max int) bool {
	if inlineData(r.fi) {
		return true
	}
	parts := 0
	for off, rem := r.offset, r.remaining; rem > 0; parts++ {
		rd, err := r.locate(off, rem)
		if err != nil || parts == max {
			return false
		}
		off, rem = off+rd.partLen, rem-rd.partLen
	}
	var opened []pinnedStream
	for off, rem := r.offset, r.remaining; rem > 0; {
		rd, err := r.locate(off, rem)
		if err != nil {
			closePinned(opened)
			return false
		}
		rc, pinned, err := r.st.OpenData(r.ctx, r.bucket, r.key, r.fi, rd.part, rd.storedOff, rd.part.Size-rd.storedOff)
		if err != nil {
			closePinned(opened)
			return false
		}
		opened = append(opened, pinnedStream{off: off, rc: rc})
		if !pinned {
			closePinned(opened)
			return false
		}
		off, rem = off+rd.partLen, rem-rd.partLen
	}
	r.pinned = opened
	return true
}

func (r *compressedReader) closeCur() {
	if r.closer != nil {
		_ = r.closer.Close()
	}
	r.cur, r.closer, r.partLeft = nil, nil, 0
}

func (r *compressedReader) Close() error {
	closePinned(r.pinned)
	r.pinned = nil
	r.closeCur()
	return nil
}
