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
	"hash/crc64"
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
	case domain.ChecksumCRC64NVME:
		return crc64.New(crc64.MakeTable(crc64NVMEPoly)), nil
	case domain.ChecksumSHA1:
		return sha1.New(), nil
	case domain.ChecksumSHA256:
		return sha256.New(), nil
	default:
		return nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", "Unsupported checksum algorithm "+string(algo))
	}
}

// checksumTypeOf returns the checksum type of a stored checksum: the
// recorded one, or (versions written before the type was recorded) the
// type its form tells: "<value>-N" is the composite checksum of N parts.
func checksumTypeOf(value, stored string) domain.ChecksumType {
	switch t := domain.ChecksumType(stored); t {
	case domain.ChecksumTypeFullObject, domain.ChecksumTypeComposite:
		return t
	}
	if i := strings.LastIndexByte(value, '-'); i >= 0 {
		if _, err := strconv.Atoi(value[i+1:]); err == nil {
			return domain.ChecksumTypeComposite
		}
	}
	return domain.ChecksumTypeFullObject
}

// uploadChecksumType resolves the checksum type of a new multipart upload
// (CreateMultipartUpload): COMPOSITE by default, FULL_OBJECT for
// CRC64NVME, which supports nothing else; FULL_OBJECT needs a CRC
// algorithm and a type needs an algorithm.
func uploadChecksumType(algo domain.ChecksumAlgorithm, typ domain.ChecksumType) (domain.ChecksumType, error) {
	switch {
	case algo == domain.ChecksumNone && typ != domain.ChecksumTypeNone:
		return "", domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			"The x-amz-checksum-type header can only be used with the x-amz-checksum-algorithm header.")
	case algo == domain.ChecksumNone:
		return domain.ChecksumTypeNone, nil
	case typ == domain.ChecksumTypeNone && algo == domain.ChecksumCRC64NVME:
		return domain.ChecksumTypeFullObject, nil
	case typ == domain.ChecksumTypeNone:
		return domain.ChecksumTypeComposite, nil
	case typ == domain.ChecksumTypeFullObject && !algo.IsCRC():
		return "", domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			fmt.Sprintf("The FULL_OBJECT checksum type cannot be used with the %s checksum algorithm.", strings.ToLower(string(algo))))
	case typ == domain.ChecksumTypeComposite && algo == domain.ChecksumCRC64NVME:
		return "", domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			"The COMPOSITE checksum type cannot be used with the crc64nvme checksum algorithm.")
	default:
		return typ, nil
	}
}

// partSum is the checksum of one part of a completed upload with
// the length of the content it covers.
type partSum struct {
	sum  domain.Checksum
	size int64
}

// compositeChecksum returns the checksum of a multipart object as S3
// computes it (checksum type COMPOSITE): the algorithm applied to the
// concatenated binary checksums of the parts, base64-encoded, followed by
// "-" and the number of parts. ok is false when a part has no checksum of
// that algorithm.
func compositeChecksum(algo domain.ChecksumAlgorithm, parts []partSum) (sum domain.Checksum, ok bool, err error) {
	h, err := newChecksumHash(algo)
	if err != nil {
		return domain.Checksum{}, false, err
	}
	for _, p := range parts {
		raw, valid := decodePartChecksum(algo, p.sum)
		if !valid {
			return domain.Checksum{}, false, nil
		}
		_, _ = h.Write(raw)
	}
	value := base64.StdEncoding.EncodeToString(h.Sum(nil)) + "-" + strconv.Itoa(len(parts))
	return domain.Checksum{Algorithm: algo, Value: value, Type: domain.ChecksumTypeComposite}, true, nil
}

// fullObjectChecksum returns the CRC of the whole content of a multipart
// object (checksum type FULL_OBJECT), combined from the CRCs of its parts
// and their lengths. ok is false when a part has no checksum of that
// algorithm.
func fullObjectChecksum(algo domain.ChecksumAlgorithm, parts []partSum) (sum domain.Checksum, ok bool, err error) {
	c, err := newCRCCombiner(algo)
	if err != nil {
		return domain.Checksum{}, false, err
	}
	var crc uint64
	for i, p := range parts {
		raw, valid := decodePartChecksum(algo, p.sum)
		if !valid || len(raw) != c.size {
			return domain.Checksum{}, false, nil
		}
		part := c.decode(raw)
		if i == 0 {
			crc = part
			continue
		}
		crc = c.combine(crc, part, p.size)
	}
	if len(parts) == 0 {
		crc = 0
	}
	return domain.Checksum{Algorithm: algo, Value: base64.StdEncoding.EncodeToString(c.encode(crc)), Type: domain.ChecksumTypeFullObject}, true, nil
}

// decodePartChecksum decodes the checksum of a part, when it is one of
// algo.
func decodePartChecksum(algo domain.ChecksumAlgorithm, c domain.Checksum) ([]byte, bool) {
	if c.Algorithm != algo {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(c.Value)
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	return raw, true
}

// multipartChecksum returns the additional checksum of a completed upload:
// the composite or full-object checksum (typ, the type of the upload) of
// its parts, in the algorithm of the upload (or, without one, of the
// declared checksum or of the parts, composite). A checksum declared on
// the completion is verified against it (BadDigest when it differs; a
// composite one with or without the "-N" suffix) and refused when it
// cannot be verified: a value chosen by the client is never stored as is.
// The zero checksum means the object has none.
func multipartChecksum(uploadAlgo domain.ChecksumAlgorithm, typ domain.ChecksumType, parts []partSum, declared domain.Checksum) (domain.Checksum, error) {
	algo := uploadAlgo
	if algo == domain.ChecksumNone {
		algo = declared.Algorithm
		typ = domain.ChecksumTypeComposite
	}
	if algo == domain.ChecksumNone && len(parts) > 0 {
		algo = parts[0].sum.Algorithm
		typ = domain.ChecksumTypeComposite
	}
	if !declared.IsZero() && declared.Algorithm != algo {
		return domain.Checksum{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			fmt.Sprintf("Checksum Type mismatch occurred, expected checksum Type: %s, actual checksum Type: %s", algo, declared.Algorithm))
	}
	if algo == domain.ChecksumNone {
		return domain.Checksum{}, nil
	}
	var (
		sum domain.Checksum
		ok  bool
		err error
	)
	if typ == domain.ChecksumTypeFullObject {
		sum, ok, err = fullObjectChecksum(algo, parts)
	} else {
		sum, ok, err = compositeChecksum(algo, parts)
	}
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
