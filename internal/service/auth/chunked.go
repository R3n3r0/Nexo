package auth

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	// chunkReaderBuffer is the read buffer of the aws-chunked decoder; it
	// also bounds a chunk header or trailer line.
	chunkReaderBuffer = 64 << 10
	// maxChunkSizeDigits bounds the hex chunk size (16 digits = 64 bits).
	maxChunkSizeDigits = 16
	// maxTrailers bounds the number of trailing headers.
	maxTrailers = 16
	// trailerSignatureName is the trailer carrying the trailer signature.
	trailerSignatureName = "x-amz-trailer-signature"
	chunkSignaturePrefix = "chunk-signature="
)

// chunkedConfig configures the aws-chunked decoder.
type chunkedConfig struct {
	// signed requires chunk signatures (STREAMING-AWS4-HMAC-SHA256-PAYLOAD*).
	signed bool
	// trailer expects trailing headers after the final chunk.
	trailer bool
	key     []byte
	scope   string
	amzDate string
	// seed is the previous signature: the request signature at the start.
	seed string
	// decodedLength is x-amz-decoded-content-length.
	decodedLength int64
	// checksums are the running hashes of the declared x-amz-checksum-*
	// trailers (lowercase names).
	checksums map[string]checksumState
	// trailers receives the decoded trailers (r.Trailer); may be nil.
	trailers http.Header
}

// checksumState is the running hash of a declared checksum trailer.
type checksumState struct {
	hash      hash.Hash
	algorithm string
}

// chunkedReader decodes an aws-chunked payload while it streams: it checks
// the signature of every chunk (signed variants), the decoded length, the
// trailer signature and the trailing checksums. The error of a faulty
// chunk is returned by the Read that consumes its last byte; when the
// declared length has been read the final chunk and the trailer are
// processed eagerly, so that callers reading exactly the decoded length
// (io.CopyN, io.ReadFull) still get every check.
type chunkedReader struct {
	src     *bufio.Reader
	closer  io.Closer
	cfg     chunkedConfig
	prevSig string

	remaining int64 // bytes left in the current chunk
	decoded   int64 // bytes of payload returned so far
	chunkHash hash.Hash
	chunkSig  string
	inChunk   bool
	done      bool
	err       error
	trailers  http.Header
}

func newChunkedReader(body io.ReadCloser, cfg chunkedConfig) *chunkedReader {
	return &chunkedReader{
		src:       bufio.NewReaderSize(body, chunkReaderBuffer),
		closer:    body,
		cfg:       cfg,
		prevSig:   cfg.seed,
		chunkHash: sha256.New(),
		trailers:  http.Header{},
	}
}

// Trailers returns the trailing headers once the body was read to EOF.
func (c *chunkedReader) Trailers() http.Header { return c.trailers }

// Close closes the underlying body.
func (c *chunkedReader) Close() error { return c.closer.Close() }

// Read implements io.Reader.
func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.done {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if !c.inChunk {
			if err := c.nextChunk(); err != nil {
				return 0, c.fail(err)
			}
			if c.done {
				return 0, io.EOF
			}
		}
		if int64(len(p)) > c.remaining {
			p = p[:c.remaining]
		}
		n, err := c.src.Read(p)
		if n > 0 {
			c.consume(p[:n])
		}
		// A failed check withholds the bytes of this read: the caller never
		// gets the end of a chunk that does not verify, nor the complete
		// payload (io.ReadFull drops the error of the read that fills its
		// buffer).
		if c.remaining == 0 {
			if cerr := c.endChunk(); cerr != nil {
				return 0, c.fail(cerr)
			}
			if c.decoded == c.cfg.decodedLength {
				// Process the final chunk and the trailer now.
				if cerr := c.nextChunk(); cerr != nil {
					return 0, c.fail(cerr)
				}
				if !c.done {
					return 0, c.fail(errIncomplete("the payload is longer than x-amz-decoded-content-length"))
				}
			}
		}
		if err != nil && c.remaining > 0 {
			if errors.Is(err, io.EOF) {
				err = errIncomplete("the payload ended inside a chunk")
			}
			return n, c.fail(err)
		}
		if n > 0 {
			return n, nil
		}
	}
}

func (c *chunkedReader) fail(err error) error {
	c.err = err
	return err
}

// consume accounts for payload bytes of the current chunk.
func (c *chunkedReader) consume(b []byte) {
	c.chunkHash.Write(b)
	for _, cs := range c.cfg.checksums {
		cs.hash.Write(b)
	}
	c.remaining -= int64(len(b))
	c.decoded += int64(len(b))
}

// nextChunk parses "<hex size>[;chunk-signature=<sig>]\r\n". A zero size
// is the final chunk: its signature, the trailer and the decoded length
// are checked and the reader is done.
func (c *chunkedReader) nextChunk() error {
	line, err := c.readLine()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return errIncomplete("the payload ended before the final chunk")
		}
		return err
	}
	sizeHex, ext, _ := strings.Cut(line, ";")
	sizeHex = strings.TrimSpace(sizeHex)
	if sizeHex == "" || len(sizeHex) > maxChunkSizeDigits {
		return errMalformedChunk("invalid chunk size " + strconv.Quote(sizeHex))
	}
	size, err := strconv.ParseUint(sizeHex, 16, 64)
	if err != nil {
		return errMalformedChunk("invalid chunk size " + strconv.Quote(sizeHex))
	}
	if size > uint64(c.cfg.decodedLength-c.decoded) {
		return errIncomplete("the payload is longer than x-amz-decoded-content-length")
	}
	c.chunkSig = ""
	if c.cfg.signed {
		sig, ok := strings.CutPrefix(strings.TrimSpace(ext), chunkSignaturePrefix)
		if !ok || !isSignature(sig) {
			return errMalformedChunk("missing or malformed chunk-signature")
		}
		c.chunkSig = sig
	}
	c.remaining = int64(size)
	c.chunkHash.Reset()
	if size > 0 {
		c.inChunk = true
		return nil
	}
	return c.finish()
}

// endChunk verifies the signature of a data chunk and consumes its CRLF.
func (c *chunkedReader) endChunk() error {
	c.inChunk = false
	if err := c.verifyChunkSignature(); err != nil {
		return err
	}
	return c.expectCRLF()
}

// verifyChunkSignature checks the current chunk signature, chained to the
// previous one.
func (c *chunkedReader) verifyChunkSignature() error {
	if !c.cfg.signed {
		return nil
	}
	sts := chunkAlgorithm + "\n" + c.cfg.amzDate + "\n" + c.cfg.scope + "\n" + c.prevSig + "\n" + emptySHA256 + "\n" + hexSum(c.chunkHash)
	if !equalSignatures(signatureHex(c.cfg.key, sts), c.chunkSig) {
		return errSignature("chunk signature")
	}
	c.prevSig = strings.ToLower(c.chunkSig)
	return nil
}

// finish handles the final (empty) chunk: its signature, the trailer and
// the decoded length.
func (c *chunkedReader) finish() error {
	if err := c.verifyChunkSignature(); err != nil {
		return err
	}
	if c.cfg.trailer {
		if err := c.readTrailer(); err != nil {
			return err
		}
	} else {
		// The final chunk is followed by an empty line; tolerate its
		// absence at EOF.
		if b, err := c.src.Peek(2); err == nil && b[0] == '\r' && b[1] == '\n' {
			_, _ = c.src.Discard(2)
		} else if b, err := c.src.Peek(1); err == nil && b[0] == '\n' {
			_, _ = c.src.Discard(1)
		}
	}
	if c.decoded != c.cfg.decodedLength {
		return errIncomplete("the decoded payload is shorter than x-amz-decoded-content-length")
	}
	c.done = true
	return nil
}

// readTrailer reads the trailing headers up to the empty line (or EOF),
// verifies the trailer signature (signed variants) and the checksums.
func (c *chunkedReader) readTrailer() error {
	type trailer struct{ name, value string }
	var received []trailer
	trailerSig := ""
	for {
		line, err := c.readLine()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return errMalformedTrailer("a trailer line is not a name:value pair")
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if name == trailerSignatureName {
			if trailerSig != "" {
				return errMalformedTrailer("duplicate trailer signature")
			}
			trailerSig = value
			continue
		}
		if len(received) >= maxTrailers {
			return errMalformedTrailer("too many trailers")
		}
		received = append(received, trailer{name, value})
	}
	sort.SliceStable(received, func(i, j int) bool { return received[i].name < received[j].name })

	if c.cfg.signed {
		if !isSignature(trailerSig) {
			return errMalformedTrailer("missing or malformed x-amz-trailer-signature")
		}
		var canonical bytes.Buffer
		for _, t := range received {
			canonical.WriteString(t.name)
			canonical.WriteByte(':')
			canonical.WriteString(t.value)
			canonical.WriteByte('\n')
		}
		sts := trailerAlgorithm + "\n" + c.cfg.amzDate + "\n" + c.cfg.scope + "\n" + c.prevSig + "\n" + sha256Hex(canonical.Bytes())
		if !equalSignatures(signatureHex(c.cfg.key, sts), trailerSig) {
			return errSignature("trailer signature")
		}
	}

	seen := make(map[string]bool, len(received))
	for _, t := range received {
		cs, declared := c.cfg.checksums[t.name]
		if !declared {
			return errMalformedTrailer("the trailer " + t.name + " was not declared in x-amz-trailer")
		}
		if seen[t.name] {
			return errMalformedTrailer("duplicate trailer " + t.name)
		}
		seen[t.name] = true
		if encodeChecksum(cs.hash) != t.value {
			return errBadDigest(cs.algorithm)
		}
		c.trailers.Set(t.name, t.value)
		if c.cfg.trailers != nil {
			c.cfg.trailers.Set(t.name, t.value)
		}
	}
	for name := range c.cfg.checksums {
		if !seen[name] {
			return errMalformedTrailer("the declared trailer " + name + " is missing")
		}
	}
	return nil
}

// readLine reads a CRLF (or LF) terminated line, bounded by the buffer.
func (c *chunkedReader) readLine() (string, error) {
	line, err := c.src.ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		return "", errMalformedChunk("chunk header or trailer line too long")
	case errors.Is(err, io.EOF) && len(line) == 0:
		return "", io.EOF
	case errors.Is(err, io.EOF):
		return "", errIncomplete("the payload ended inside a chunk header")
	case err != nil:
		return "", err
	}
	line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
	return string(line), nil
}

// expectCRLF consumes the CRLF that terminates the data of a chunk.
func (c *chunkedReader) expectCRLF() error {
	b, err := c.src.ReadByte()
	if err != nil {
		return errIncomplete("the payload ended before the chunk terminator")
	}
	if b == '\r' {
		if b, err = c.src.ReadByte(); err != nil {
			return errIncomplete("the payload ended before the chunk terminator")
		}
	}
	if b != '\n' {
		return errMalformedChunk("missing CRLF after the chunk data")
	}
	return nil
}

func hexSum(h hash.Hash) string {
	return hex.EncodeToString(h.Sum(nil))
}

// sha256Verifier compares the SHA-256 of the body with the declared hash.
// The comparison happens as soon as the declared Content-Length has been
// read (so that callers reading exactly that many bytes are covered) or
// at EOF when the length is unknown.
type sha256Verifier struct {
	src      io.ReadCloser
	hash     hash.Hash
	expected string
	size     int64 // expected length, -1 when unknown
	read     int64
	checked  bool
	err      error
}

func newSHA256Verifier(body io.ReadCloser, expected string, size int64) *sha256Verifier {
	return &sha256Verifier{src: body, hash: sha256.New(), expected: strings.ToLower(expected), size: size}
}

// Read implements io.Reader.
func (s *sha256Verifier) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.src.Read(p)
	if n > 0 {
		s.hash.Write(p[:n])
		s.read += int64(n)
	}
	// On failure the bytes of this read are withheld, so that a caller
	// reading exactly the declared length (io.ReadFull drops the error of
	// the read that fills its buffer) never gets the complete body.
	switch {
	case s.size >= 0 && s.read > s.size:
		s.err = errSHA256Mismatch()
		return 0, s.err
	case s.checked:
	case (s.size >= 0 && s.read == s.size) || errors.Is(err, io.EOF):
		s.checked = true
		if hexSum(s.hash) != s.expected {
			s.err = errSHA256Mismatch()
			return 0, s.err
		}
	}
	return n, err
}

// Close closes the underlying body.
func (s *sha256Verifier) Close() error { return s.src.Close() }

// bufferedBody replays a body read in memory and closes the original.
type bufferedBody struct {
	*bytes.Reader
	closer io.Closer
}

// Close closes the original body.
func (b *bufferedBody) Close() error { return b.closer.Close() }
