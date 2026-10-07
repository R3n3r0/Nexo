package domain

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// SystemBucket is the reserved bucket that holds Nexo metadata on disk.
const SystemBucket = ".nexo.sys"

// Versioning is the versioning state of a bucket.
type Versioning int

const (
	// VersioningOff means versioning has never been enabled.
	VersioningOff Versioning = iota
	// VersioningEnabled means every write creates a new version.
	VersioningEnabled
	// VersioningSuspended means versioning was enabled and then suspended.
	VersioningSuspended
)

// String returns the S3 status string ("", "Enabled", "Suspended").
func (v Versioning) String() string {
	switch v {
	case VersioningEnabled:
		return "Enabled"
	case VersioningSuspended:
		return "Suspended"
	default:
		return ""
	}
}

// Versioned reports whether versions are retained (enabled or suspended).
func (v Versioning) Versioned() bool { return v != VersioningOff }

// ParseVersioning parses an S3 versioning status string.
func ParseVersioning(s string) (Versioning, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return VersioningOff, nil
	case "enabled":
		return VersioningEnabled, nil
	case "suspended":
		return VersioningSuspended, nil
	default:
		return VersioningOff, fmt.Errorf("%w: unknown versioning status %q", ErrInvalidArgument, s)
	}
}

// Bucket is the minimal identity of a bucket.
type Bucket struct {
	Name    string
	Created time.Time
	Region  string
}

// BucketInfo extends Bucket with its settings and usage statistics.
type BucketInfo struct {
	Bucket
	Versioning        Versioning
	ObjectLockEnabled bool
	// Objects is the number of live objects (latest versions, no delete markers).
	Objects uint64
	// Versions is the number of non-current versions.
	Versions uint64
	// DeleteMarkers is the number of delete markers.
	DeleteMarkers uint64
	// Size is the logical size in bytes of all versions.
	Size uint64
	// QuotaBytes is the hard quota in bytes, 0 when unset.
	QuotaBytes uint64
	Tags       map[string]string
}

// ValidateBucketName validates a bucket name against the S3 naming rules:
// 3-63 characters, lowercase letters, digits, dots and hyphens, starting and
// ending with a letter or digit, no adjacent dots, no dot next to a hyphen,
// not formatted as an IPv4 address, no "xn--" prefix, no "-s3alias" or
// "--ol-s3" suffix. Names failing the check return ErrInvalidBucketName.
func ValidateBucketName(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return fmt.Errorf("%w: %q must be between 3 and 63 characters", ErrInvalidBucketName, name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-':
		default:
			return fmt.Errorf("%w: %q contains invalid character %q", ErrInvalidBucketName, name, c)
		}
	}
	if !isAlnum(name[0]) || !isAlnum(name[len(name)-1]) {
		return fmt.Errorf("%w: %q must start and end with a letter or digit", ErrInvalidBucketName, name)
	}
	if strings.Contains(name, "..") || strings.Contains(name, ".-") || strings.Contains(name, "-.") {
		return fmt.Errorf("%w: %q has adjacent dots or dot next to hyphen", ErrInvalidBucketName, name)
	}
	if net.ParseIP(name) != nil {
		return fmt.Errorf("%w: %q must not be an IP address", ErrInvalidBucketName, name)
	}
	if strings.HasPrefix(name, "xn--") || strings.HasPrefix(name, "sthree-") {
		return fmt.Errorf("%w: %q uses a reserved prefix", ErrInvalidBucketName, name)
	}
	if strings.HasSuffix(name, "-s3alias") || strings.HasSuffix(name, "--ol-s3") {
		return fmt.Errorf("%w: %q uses a reserved suffix", ErrInvalidBucketName, name)
	}
	return nil
}

// IsSystemBucket reports whether name is the reserved metadata bucket.
func IsSystemBucket(name string) bool { return name == SystemBucket }

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}
