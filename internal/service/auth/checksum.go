package auth

import (
	"crypto/sha1" // #nosec G505 -- the SHA1 checksum algorithm of S3, an integrity check only
	"crypto/sha256"
	"encoding/base64"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"strings"
)

// checksumPrefix is the prefix of the S3 additional checksum headers and
// trailers (x-amz-checksum-crc32, -crc32c, -crc64nvme, -sha1, -sha256).
const checksumPrefix = "x-amz-checksum-"

// crc64NVMEPoly is the reflected polynomial of CRC-64/NVME, the
// x-amz-checksum-crc64nvme algorithm.
const crc64NVMEPoly = 0x9a6c9329ac4bc9b5

// newChecksum returns the running hash of an x-amz-checksum-* header or
// trailer name (lowercase) and the algorithm name used in error messages.
func newChecksum(name string) (hash.Hash, string, bool) {
	algo, ok := strings.CutPrefix(name, checksumPrefix)
	if !ok {
		return nil, "", false
	}
	switch algo {
	case "crc32":
		return crc32.NewIEEE(), "CRC32", true
	case "crc32c":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli)), "CRC32C", true
	case "crc64nvme":
		return crc64.New(crc64.MakeTable(crc64NVMEPoly)), "CRC64NVME", true
	case "sha1":
		return sha1.New(), "SHA1", true // #nosec G401 -- the S3 SHA1 checksum algorithm (x-amz-checksum-sha1), an integrity check only
	case "sha256":
		return sha256.New(), "SHA256", true
	default:
		return nil, "", false
	}
}

// encodeChecksum renders a digest the way S3 checksums are transmitted:
// standard base64 of the big-endian digest bytes.
func encodeChecksum(h hash.Hash) string {
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
