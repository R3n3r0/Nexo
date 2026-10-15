package s3

import (
	"bytes"
	"crypto/md5" // #nosec G501 -- the S3 Content-MD5 integrity check
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// errBodyTooLarge reports a request document beyond its limit; callers
// map it to the S3 code of the document.
var errBodyTooLarge = errors.New("request body too large")

// readBody reads a bounded request document and verifies its Content-MD5.
// A body larger than limit fails with errBodyTooLarge (wrapped).
func readBody(r *http.Request, limit int64) ([]byte, error) {
	want, err := decodeContentMD5(r.Header)
	if err != nil {
		return nil, err
	}
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		body, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			return nil, bodyReadError(err)
		}
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: more than %d bytes", errBodyTooLarge, limit)
	}
	if want != nil {
		// Content-MD5 is the integrity check of the S3 protocol, not a
		// security primitive.
		got := md5.Sum(body) // #nosec G401 -- the Content-MD5 check of S3, an integrity check only
		if !bytes.Equal(got[:], want) {
			return nil, domain.NewS3Error(domain.ErrBadDigest, "BadDigest", "The Content-MD5 you specified did not match what we received.")
		}
	}
	return body, nil
}

// bodyReadError maps a failure reading the request body: a body shorter
// than its Content-Length is IncompleteBody, a body beyond the server
// limit EntityTooLarge; the S3Error of a verifying reader (signature,
// SHA-256 mismatch) is kept.
func bodyReadError(err error) error {
	var s3err *domain.S3Error
	var maxErr *http.MaxBytesError
	switch {
	case errors.As(err, &s3err):
		return err
	case errors.As(err, &maxErr):
		return domain.NewS3Error(domain.ErrEntityTooLarge, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size")
	case errors.Is(err, io.ErrUnexpectedEOF):
		return domain.NewS3Error(domain.ErrInvalidArgument, "IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header.")
	default:
		return fmt.Errorf("%w: read request body: %v", domain.ErrInvalidArgument, err)
	}
}

// decodeXMLBody reads an XML request document (at most limit bytes,
// Content-MD5 verified) into v. Oversized and malformed documents are
// MalformedXML.
func decodeXMLBody(r *http.Request, dec httpx.Decoder, limit int64, v any) error {
	body, err := readBody(r, limit)
	if errors.Is(err, errBodyTooLarge) {
		return domain.NewS3Error(domain.ErrMalformedXML, "MaxMessageLengthExceeded", "Your request was too big.")
	}
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
	}
	return dec.DecodeXML(bytes.NewReader(body), limit, v)
}
