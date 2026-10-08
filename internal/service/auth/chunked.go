package auth

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// maxChunkHeaderLength bounds a chunk header line ("<hex size>;chunk-signature=<64 hex>").
const maxChunkHeaderLength = 4096

// chunkedConfig configures the aws-chunked decoder.
type chunkedConfig struct {
	// signed requires chunk signatures (STREAMING-AWS4-HMAC-SHA256-PAYLOAD*).
	signed bool
	// trailer expects trailing headers after the final chunk.
	trailer bool
	key     []byte
	scope   credentialScope
	amzDate string
	// seed is the previous signature: the request signature at the start.
	seed string
}

// chunkedReader decodes an aws-chunked payload, verifying every chunk
// signature and the trailer signature as the data streams through.
type chunkedReader struct {
	src     *bufio.Reader
	closer  io.Closer
	cfg     chunkedConfig
	prevSig string

	remaining int64 // bytes left in the current chunk
	chunkHash hash.Hash
	chunkSig  string
	inChunk   bool
	done      bool
	err       error
	trailers  http.Header
}

// Trailers returns the trailing headers once the body was read to EOF.
func (c *chunkedReader) Trailers() http.Header { return c.trailers }

func newChunkedReader(body io.ReadCloser, cfg chunkedConfig) *chunkedReader {
	return &chunkedReader{src: bufio.NewReader(body), closer: body, cfg: cfg, prevSig: cfg.seed, chunkHash: sha256.New(), trailers: http.Header{}}
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.done {
		return 0, io.EOF
	}
	for {
		if !c.inChunk {
			if err := c.readChunkHeader(); err != nil {
				c.err = err
				return 0, err
			}
			if c.remaining == 0 {
				if err := c.finish(); err != nil {
					c.err = err
					return 0, err
				}
				c.done = true
				return 0, io.EOF
			}
		}
		if int64(len(p)) > c.remaining {
			p = p[:c.remaining]
		}
		n, err := c.src.Read(p)
		if n > 0 {
			c.chunkHash.Write(p[:n])
			c.remaining -= int64(n)
		}
		if c.remaining == 0 {
			if err := c.endChunk(); err != nil {
				c.err = err
				return n, err
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.err = incomplete("the payload ended inside a chunk")
				return n, c.err
			}
			c.err = err
			return n, err
		}
		if n > 0 {
			return n, nil
		}
	}
}

// readChunkHeader parses "<hex size>[;chunk-signature=<sig>]\r\n".
func (c *chunkedReader) readChunkHeader() error {
	line, err := c.readLine()
	if err != nil {
		return err
	}
	sizeHex, rest, _ := strings.Cut(line, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
	if err != nil || size < 0 {
		return malformedChunk("invalid chunk size " + strconv.Quote(sizeHex))
	}
	c.chunkSig = ""
	if c.cfg.signed {
		sig, ok := strings.CutPrefix(strings.TrimSpace(rest), "chunk-signature=")
		if !ok || len(sig) != 64 || !isHex(sig) {
			return malformedChunk("missing or malformed chunk-signature")
		}
		c.chunkSig = sig
	}
	c.remaining = size
	c.inChunk = true
	c.chunkHash.Reset()
	return nil
}

// endChunk verifies the chunk signature and consumes the trailing CRLF.
func (c *chunkedReader) endChunk() error {
	c.inChunk = false
	if c.cfg.signed {
		expected := c.chunkSignature(c.chunkHash.Sum(nil))
		if !equalSignatures(expected, c.chunkSig) {
			return fmt.Errorf("%w: chunk signature", domain.ErrSignatureMismatch)
		}
		c.prevSig = c.chunkSig
	}
	if err := c.expectCRLF(); err != nil {
		return err
	}
	return nil
}

// finish handles the final (empty) chunk: its signature was verified by
// readChunkHeader/endChunk; here the trailer, if any, is read and checked.
func (c *chunkedReader) finish() error {
	if c.cfg.signed {
		expected := c.chunkSignature(sha256.New().Sum(nil))
		if !equalSignatures(expected, c.chunkSig) {
			return fmt.Errorf("%w: final chunk signature", domain.ErrSignatureMismatch)
		}
		c.prevSig = c.chunkSig
	}
	if !c.cfg.trailer {
		// Optional terminating CRLF after the final chunk.
		_ = c.consumeOptionalCRLF()
		return nil
	}
	var canonical bytes.Buffer
	trailerSig := ""
	for {
		line, err := c.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return err
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return malformedChunk("malformed trailer line")
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if name == "x-amz-trailer-signature" {
			trailerSig = value
			continue
		}
		c.trailers.Add(name, value)
		canonical.WriteString(name)
		canonical.WriteByte(':')
		canonical.WriteString(value)
		canonical.WriteByte('\n')
	}
	if c.cfg.signed {
		if trailerSig == "" {
			return malformedChunk("missing x-amz-trailer-signature")
		}
		sts := strings.Join([]string{trailerAlgorithm, c.cfg.amzDate, c.cfg.scope.scope(), c.prevSig, sha256Hex(canonical.Bytes())}, "\n")
		if !equalSignatures(signatureHex(c.cfg.key, sts), trailerSig) {
			return fmt.Errorf("%w: trailer signature", domain.ErrSignatureMismatch)
		}
	}
	return nil
}

func (c *chunkedReader) chunkSignature(chunkSum []byte) string {
	sts := strings.Join([]string{chunkAlgorithm, c.cfg.amzDate, c.cfg.scope.scope(), c.prevSig, emptySHA256, fmt.Sprintf("%x", chunkSum)}, "\n")
	return signatureHex(c.cfg.key, sts)
}

func (c *chunkedReader) readLine() (string, error) {
	line, err := c.src.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line == "" {
			return "", io.EOF
		}
		if errors.Is(err, io.EOF) {
			return "", incomplete("the payload ended inside a chunk header")
		}
		return "", err
	}
	if len(line) > maxChunkHeaderLength {
		return "", malformedChunk("chunk header too long")
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

func (c *chunkedReader) expectCRLF() error {
	b := make([]byte, 2)
	if _, err := io.ReadFull(c.src, b); err != nil {
		return incomplete("the payload ended before the chunk terminator")
	}
	if b[0] != '\r' || b[1] != '\n' {
		return malformedChunk("missing CRLF after chunk data")
	}
	return nil
}

func (c *chunkedReader) consumeOptionalCRLF() error {
	b, err := c.src.Peek(2)
	if err == nil && b[0] == '\r' && b[1] == '\n' {
		_, _ = c.src.Discard(2)
	}
	return nil
}

func (c *chunkedReader) Close() error { return c.closer.Close() }

func malformedChunk(detail string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidChunkEncoding", "The chunk encoding of the payload is invalid: "+detail+".")
}

func incomplete(detail string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "IncompleteBody", "The request body is incomplete: "+detail+".")
}

// sha256Verifier compares the SHA-256 of the whole body with the declared
// hash when the body reaches EOF.
type sha256Verifier struct {
	src      io.ReadCloser
	hash     hash.Hash
	expected string
	checked  bool
}

func newSHA256Verifier(body io.ReadCloser, expected string) *sha256Verifier {
	return &sha256Verifier{src: body, hash: sha256.New(), expected: strings.ToLower(expected)}
}

func (s *sha256Verifier) Read(p []byte) (int, error) {
	n, err := s.src.Read(p)
	if n > 0 {
		s.hash.Write(p[:n])
	}
	if errors.Is(err, io.EOF) && !s.checked {
		s.checked = true
		if fmt.Sprintf("%x", s.hash.Sum(nil)) != s.expected {
			return n, sha256Mismatch()
		}
	}
	return n, err
}

func (s *sha256Verifier) Close() error { return s.src.Close() }
