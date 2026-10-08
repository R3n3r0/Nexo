package object

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// hashReader computes the ETag (MD5), the optional SHA-256 and the optional
// additional checksum of a stream while it is being stored.
type hashReader struct {
	r     io.Reader
	md5   hash.Hash
	sha   hash.Hash
	extra hash.Hash
	algo  domain.ChecksumAlgorithm
	n     int64
}

func newHashReader(r io.Reader, wantSHA bool, algo domain.ChecksumAlgorithm) (*hashReader, error) {
	h := &hashReader{r: r, md5: md5.New(), algo: algo}
	if wantSHA {
		h.sha = sha256.New()
	}
	if algo != domain.ChecksumNone {
		extra, err := newChecksumHash(algo)
		if err != nil {
			return nil, err
		}
		h.extra = extra
	}
	return h, nil
}

// newChecksumHash returns the hash of an additional checksum algorithm.
func newChecksumHash(algo domain.ChecksumAlgorithm) (hash.Hash, error) {
	switch algo {
	case domain.ChecksumCRC32:
		return crc32.New(crc32.IEEETable), nil
	case domain.ChecksumCRC32C:
		return crc32.New(crc32.MakeTable(crc32.Castagnoli)), nil
	case domain.ChecksumSHA1:
		return sha1.New(), nil
	case domain.ChecksumSHA256:
		return sha256.New(), nil
	default:
		return nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", "Unsupported checksum algorithm "+string(algo))
	}
}

// compositeChecksum returns the checksum of a multipart object as S3
// computes it (checksum type COMPOSITE): the algorithm applied to the
// concatenated binary checksums of the parts, base64-encoded, followed by
// "-" and the number of parts. ok is false when a part has no checksum of
// that algorithm.
func compositeChecksum(algo domain.ChecksumAlgorithm, parts []domain.Checksum) (sum domain.Checksum, ok bool, err error) {
	h, err := newChecksumHash(algo)
	if err != nil {
		return domain.Checksum{}, false, err
	}
	for _, p := range parts {
		if p.Algorithm != algo {
			return domain.Checksum{}, false, nil
		}
		raw, err := base64.StdEncoding.DecodeString(p.Value)
		if err != nil || len(raw) == 0 {
			return domain.Checksum{}, false, nil
		}
		_, _ = h.Write(raw)
	}
	value := base64.StdEncoding.EncodeToString(h.Sum(nil)) + "-" + strconv.Itoa(len(parts))
	return domain.Checksum{Algorithm: algo, Value: value}, true, nil
}

// multipartChecksum returns the additional checksum of a completed upload:
// the composite checksum of its parts, in the algorithm of the upload (or,
// without one, of the declared checksum or of the parts). A checksum
// declared on the completion is verified against it (BadDigest when it
// differs, with or without the "-N" suffix) and refused when it cannot be
// verified: a value chosen by the client is never stored as is. The zero
// checksum means the object has none.
func multipartChecksum(uploadAlgo domain.ChecksumAlgorithm, parts []domain.Checksum, declared domain.Checksum) (domain.Checksum, error) {
	algo := uploadAlgo
	if algo == domain.ChecksumNone {
		algo = declared.Algorithm
	}
	if algo == domain.ChecksumNone && len(parts) > 0 {
		algo = parts[0].Algorithm
	}
	if !declared.IsZero() && declared.Algorithm != algo {
		return domain.Checksum{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			fmt.Sprintf("Checksum Type mismatch occurred, expected checksum Type: %s, actual checksum Type: %s", algo, declared.Algorithm))
	}
	if algo == domain.ChecksumNone {
		return domain.Checksum{}, nil
	}
	sum, ok, err := compositeChecksum(algo, parts)
	if err != nil {
		return domain.Checksum{}, err
	}
	if !ok {
		if !declared.IsZero() {
			return domain.Checksum{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
				fmt.Sprintf("The %s checksum of the object cannot be verified: not every part was uploaded with a %s checksum.", algo, algo))
		}
		return domain.Checksum{}, nil
	}
	if !declared.IsZero() {
		base, _, _ := strings.Cut(sum.Value, "-")
		if declared.Value != sum.Value && declared.Value != base {
			return domain.Checksum{}, domain.NewS3Error(domain.ErrBadDigest, "BadDigest",
				fmt.Sprintf("The %s you specified did not match the calculated checksum.", algo))
		}
	}
	return sum, nil
}

func (h *hashReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.n += int64(n)
		_, _ = h.md5.Write(p[:n])
		if h.sha != nil {
			_, _ = h.sha.Write(p[:n])
		}
		if h.extra != nil {
			_, _ = h.extra.Write(p[:n])
		}
	}
	return n, err
}

// etag returns the hex MD5 of the stream (unquoted; transports quote it).
func (h *hashReader) etag() string { return hex.EncodeToString(h.md5.Sum(nil)) }

// verify compares the computed digests with the client-declared ones and
// returns the additional checksum to persist. A Content-MD5 or additional
// checksum mismatch is domain.ErrBadDigest (S3 BadDigest); a payload hash
// mismatch is XAmzContentSHA256Mismatch.
func (h *hashReader) verify(md5Hex, sha256Hex string, declared domain.Checksum) (domain.Checksum, error) {
	if md5Hex != "" && !strings.EqualFold(md5Hex, h.etag()) {
		return domain.Checksum{}, domain.NewS3Error(domain.ErrBadDigest, "BadDigest", "The Content-MD5 you specified did not match what we received.")
	}
	if h.sha != nil && sha256Hex != "" && !strings.EqualFold(sha256Hex, hex.EncodeToString(h.sha.Sum(nil))) {
		return domain.Checksum{}, domain.NewS3Error(domain.ErrChecksumMismatch, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.")
	}
	if h.extra == nil {
		return domain.Checksum{}, nil
	}
	got := domain.Checksum{Algorithm: h.algo, Value: base64.StdEncoding.EncodeToString(h.extra.Sum(nil))}
	if declared.Value != "" && declared.Value != got.Value {
		return domain.Checksum{}, domain.NewS3Error(domain.ErrBadDigest, "BadDigest",
			fmt.Sprintf("The %s you specified did not match the calculated checksum.", h.algo))
	}
	return got, nil
}
