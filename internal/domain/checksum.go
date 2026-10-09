package domain

import (
	"fmt"
	"strings"
)

// ChecksumCRC64NVME is the CRC-64/NVME additional checksum algorithm.
const ChecksumCRC64NVME ChecksumAlgorithm = "CRC64NVME"

// ChecksumAlgorithms returns the supported additional checksum algorithms,
// in the order S3 documents them.
func ChecksumAlgorithms() []ChecksumAlgorithm {
	return []ChecksumAlgorithm{ChecksumCRC32, ChecksumCRC32C, ChecksumCRC64NVME, ChecksumSHA1, ChecksumSHA256}
}

// IsValid reports whether a is a supported algorithm (ChecksumNone is not).
func (a ChecksumAlgorithm) IsValid() bool {
	for _, known := range ChecksumAlgorithms() {
		if a == known {
			return true
		}
	}
	return false
}

// IsCRC reports whether a is one of the CRC algorithms, the only ones a
// multipart upload can checksum as a whole (ChecksumTypeFullObject).
func (a ChecksumAlgorithm) IsCRC() bool {
	return a == ChecksumCRC32 || a == ChecksumCRC32C || a == ChecksumCRC64NVME
}

// ParseChecksumAlgorithm parses an algorithm name, case-insensitively
// ("" is ChecksumNone). Unknown names return ErrInvalidArgument.
func ParseChecksumAlgorithm(s string) (ChecksumAlgorithm, error) {
	a := ChecksumAlgorithm(strings.ToUpper(strings.TrimSpace(s)))
	if a == ChecksumNone || a.IsValid() {
		return a, nil
	}
	return ChecksumNone, fmt.Errorf("%w: unsupported checksum algorithm %q", ErrInvalidArgument, s)
}

// ChecksumType tells how the checksum of an object was computed.
type ChecksumType string

// Checksum types.
const (
	ChecksumTypeNone ChecksumType = ""
	// ChecksumTypeFullObject is the checksum of the whole content: always
	// the case of a single-request upload, and of a multipart upload whose
	// part CRCs are combined.
	ChecksumTypeFullObject ChecksumType = "FULL_OBJECT"
	// ChecksumTypeComposite is the checksum of the concatenated checksums
	// of the parts of a multipart upload, suffixed with "-<parts>".
	ChecksumTypeComposite ChecksumType = "COMPOSITE"
)

// ParseChecksumType parses a checksum type, case-insensitively ("" is
// ChecksumTypeNone). Unknown types return ErrInvalidArgument.
func ParseChecksumType(s string) (ChecksumType, error) {
	t := ChecksumType(strings.ToUpper(strings.TrimSpace(s)))
	switch t {
	case ChecksumTypeNone, ChecksumTypeFullObject, ChecksumTypeComposite:
		return t, nil
	default:
		return ChecksumTypeNone, fmt.Errorf("%w: unsupported checksum type %q", ErrInvalidArgument, s)
	}
}
