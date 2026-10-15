package s3

import (
	"net/http"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// headerAmzChecksumType is the checksum type of an object or of a
// multipart upload (FULL_OBJECT or COMPOSITE).
const headerAmzChecksumType = "x-amz-checksum-type"

// ChecksumFields are the additional checksum elements of the S3 documents
// (parts, completions, copies, object attributes).
type ChecksumFields struct {
	ChecksumCRC32     string `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string `xml:"ChecksumSHA256,omitempty"`
}

// checksumFields returns the elements of a checksum.
func checksumFields(c domain.Checksum) ChecksumFields {
	var f ChecksumFields
	switch c.Algorithm {
	case domain.ChecksumCRC32:
		f.ChecksumCRC32 = c.Value
	case domain.ChecksumCRC32C:
		f.ChecksumCRC32C = c.Value
	case domain.ChecksumCRC64NVME:
		f.ChecksumCRC64NVME = c.Value
	case domain.ChecksumSHA1:
		f.ChecksumSHA1 = c.Value
	case domain.ChecksumSHA256:
		f.ChecksumSHA256 = c.Value
	}
	return f
}

// checksum returns the checksum the elements carry (the first one set).
func (f ChecksumFields) checksum() domain.Checksum {
	for _, c := range []domain.Checksum{
		{Algorithm: domain.ChecksumCRC32, Value: f.ChecksumCRC32},
		{Algorithm: domain.ChecksumCRC32C, Value: f.ChecksumCRC32C},
		{Algorithm: domain.ChecksumCRC64NVME, Value: f.ChecksumCRC64NVME},
		{Algorithm: domain.ChecksumSHA1, Value: f.ChecksumSHA1},
		{Algorithm: domain.ChecksumSHA256, Value: f.ChecksumSHA256},
	} {
		if c.Value != "" {
			return c
		}
	}
	return domain.Checksum{}
}

// checksumHeader is the x-amz-checksum-<algorithm> header of an algorithm.
func checksumHeader(algo domain.ChecksumAlgorithm) string {
	return httpx.HeaderAmzChecksumPrefix + strings.ToLower(string(algo))
}

// checksumFromHeaders reads the additional checksum of a write (PutObject,
// UploadPart, CompleteMultipartUpload): an x-amz-checksum-<algorithm>
// header, whose value the object layer verifies, or the algorithm of a
// trailing checksum (x-amz-trailer: x-amz-checksum-<algorithm>), which the
// authentication verifies while the aws-chunked body streams and the
// object layer computes and stores. x-amz-sdk-checksum-algorithm, when
// set, must name the same algorithm; alone, it is ignored (no checksum).
// A checksum of an algorithm Nexo does not compute (x-amz-checksum-md5,
// the algorithms S3 added in 2025) is refused rather than ignored: the
// client expects the upload verified.
func checksumFromHeaders(h http.Header) (domain.Checksum, error) {
	for name := range h {
		rest, ok := strings.CutPrefix(strings.ToLower(name), httpx.HeaderAmzChecksumPrefix)
		if !ok || rest == "type" || rest == "algorithm" || rest == "mode" {
			continue
		}
		if _, err := domain.ParseChecksumAlgorithm(rest); err != nil {
			return domain.Checksum{}, unsupportedChecksum()
		}
	}
	if v := h.Get(httpx.HeaderAmzSDKChecksumAlgorithm); v != "" {
		if _, err := domain.ParseChecksumAlgorithm(v); err != nil {
			return domain.Checksum{}, unsupportedChecksum()
		}
	}
	var found domain.Checksum
	for _, algo := range domain.ChecksumAlgorithms() {
		if v := h.Get(checksumHeader(algo)); v != "" {
			if !found.IsZero() {
				return domain.Checksum{}, invalidRequest("Expecting a single x-amz-checksum- header. Multiple checksum Types are not allowed.")
			}
			found = domain.Checksum{Algorithm: algo, Value: v}
		}
	}
	if found.IsZero() {
		trailer, err := trailerChecksumAlgorithm(h)
		if err != nil {
			return domain.Checksum{}, err
		}
		found.Algorithm = trailer
	}
	if v := h.Get(httpx.HeaderAmzSDKChecksumAlgorithm); v != "" && !found.IsZero() {
		algo, err := domain.ParseChecksumAlgorithm(v)
		if err != nil || algo != found.Algorithm {
			return domain.Checksum{}, invalidRequest("Value for x-amz-sdk-checksum-algorithm header is invalid.")
		}
	}
	return found, nil
}

// trailerChecksumAlgorithm returns the algorithm of the checksum announced
// in x-amz-trailer (none without one).
func trailerChecksumAlgorithm(h http.Header) (domain.ChecksumAlgorithm, error) {
	var found domain.ChecksumAlgorithm
	for _, v := range h.Values(httpx.HeaderAmzTrailer) {
		for _, name := range strings.Split(v, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			rest, ok := strings.CutPrefix(name, httpx.HeaderAmzChecksumPrefix)
			if !ok {
				continue
			}
			algo, err := domain.ParseChecksumAlgorithm(rest)
			if err != nil || algo == domain.ChecksumNone {
				return domain.ChecksumNone, invalidRequest("The x-amz-trailer header names an unsupported checksum: " + name)
			}
			if found != domain.ChecksumNone && found != algo {
				return domain.ChecksumNone, invalidRequest("Expecting a single x-amz-checksum- trailer. Multiple checksum Types are not allowed.")
			}
			found = algo
		}
	}
	return found, nil
}

// unsupportedChecksum is the error of a checksum algorithm Nexo does not
// compute.
func unsupportedChecksum() error {
	return invalidRequest("Checksum algorithm provided is unsupported. Please try again with any of the valid types: [CRC32, CRC32C, CRC64NVME, SHA1, SHA256]")
}

// checksumAlgorithmHeader reads x-amz-checksum-algorithm (CreateMultipartUpload,
// CopyObject).
func checksumAlgorithmHeader(h http.Header) (domain.ChecksumAlgorithm, error) {
	algo, err := domain.ParseChecksumAlgorithm(h.Get(httpx.HeaderAmzChecksumAlgorithm))
	if err != nil {
		return domain.ChecksumNone, unsupportedChecksum()
	}
	return algo, nil
}

// checksumTypeHeader reads x-amz-checksum-type.
func checksumTypeHeader(h http.Header) (domain.ChecksumType, error) {
	typ, err := domain.ParseChecksumType(h.Get(headerAmzChecksumType))
	if err != nil {
		return domain.ChecksumTypeNone, invalidRequest("Value for x-amz-checksum-type header is invalid.")
	}
	return typ, nil
}

// writeChecksum sets the x-amz-checksum-<algorithm> header of a checksum
// and, when known, its x-amz-checksum-type.
func writeChecksum(w http.ResponseWriter, c domain.Checksum) {
	if c.Value == "" || c.Algorithm == domain.ChecksumNone {
		return
	}
	w.Header().Set(checksumHeader(c.Algorithm), c.Value)
	if c.Type != domain.ChecksumTypeNone {
		w.Header().Set(headerAmzChecksumType, string(c.Type))
	}
}
