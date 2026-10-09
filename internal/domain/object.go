package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxObjectKeyLength is the maximum length in bytes of an object key.
const MaxObjectKeyLength = 1024

// Storage class names understood by Nexo.
const (
	StorageClassStandard          = "STANDARD"
	StorageClassReducedRedundancy = "REDUCED_REDUNDANCY"
)

// ChecksumAlgorithm identifies an additional checksum algorithm (x-amz-checksum-*).
type ChecksumAlgorithm string

// Supported checksum algorithms.
const (
	ChecksumNone   ChecksumAlgorithm = ""
	ChecksumCRC32  ChecksumAlgorithm = "CRC32"
	ChecksumCRC32C ChecksumAlgorithm = "CRC32C"
	ChecksumSHA1   ChecksumAlgorithm = "SHA1"
	ChecksumSHA256 ChecksumAlgorithm = "SHA256"
)

// Checksum is an additional checksum attached to an object or part.
type Checksum struct {
	Algorithm ChecksumAlgorithm
	// Value is the base64-encoded checksum.
	Value string
	// Type tells a whole-content checksum from the composite checksum of
	// a multipart object (empty when unknown or irrelevant).
	Type ChecksumType `json:",omitempty"`
}

// IsZero reports whether no checksum is set.
func (c Checksum) IsZero() bool { return c.Algorithm == ChecksumNone && c.Value == "" }

// ObjectInfo describes an object version.
type ObjectInfo struct {
	Bucket string
	Key    string
	// VersionID is empty for unversioned buckets and "null" for the null
	// version of a versioned or suspended bucket.
	VersionID string
	Size      int64
	// ETag is the quoted-free entity tag (MD5 hex or multipart "md5-N").
	ETag            string
	ModTime         time.Time
	ContentType     string
	ContentEncoding string
	// UserMetadata holds the x-amz-meta-* headers without the prefix.
	UserMetadata   map[string]string
	Tags           map[string]string
	IsDeleteMarker bool
	IsLatest       bool
	StorageClass   string
	Checksum       Checksum
	// Expires, CacheControl, ContentDisposition and ContentLanguage mirror the
	// corresponding standard headers stored with the object.
	Expires            time.Time
	CacheControl       string
	ContentDisposition string
	ContentLanguage    string
	// Parts is the number of parts of a multipart object, 0 otherwise.
	Parts int
	// Inlined reports whether the data is stored inline in the metadata file.
	Inlined bool
	// Retention and LegalHold are the Object Lock state of the version
	// (zero when none was set).
	Retention ObjectRetention
	LegalHold LegalHoldStatus
	// Encryption is the server-side encryption of the version (zero when
	// not encrypted); Size is always the size of the plaintext.
	Encryption EncryptionInfo
}

// ObjectPart describes one part of a multipart object, as reported by
// GetObjectAttributes.
type ObjectPart struct {
	Number int
	Size   int64
	// Checksum is the additional checksum of the part, when uploaded with one.
	Checksum Checksum
}

// Conditions are the HTTP conditional request headers.
type Conditions struct {
	IfMatch           string
	IfNoneMatch       string
	IfModifiedSince   time.Time
	IfUnmodifiedSince time.Time
}

// IsZero reports whether no condition is set.
func (c Conditions) IsZero() bool {
	return c.IfMatch == "" && c.IfNoneMatch == "" && c.IfModifiedSince.IsZero() && c.IfUnmodifiedSince.IsZero()
}

// ListLimitNone is the MaxKeys, MaxUploads or MaxParts of a listing that
// asks for no entries (S3 max-keys=0, max-uploads=0, max-parts=0): the
// bucket and the arguments are still validated, the result is empty and
// not truncated. Zero, the zero value, means the default limit (1000), so
// that internal callers never get an empty listing by omission.
const ListLimitNone = -1

// ListObjectsInput are the parameters of ListObjects (v1 and v2) and
// ListObjectVersions.
type ListObjectsInput struct {
	Bucket    string
	Prefix    string
	Delimiter string
	// Marker is the v1 start key; ContinuationToken the opaque v2 token.
	Marker            string
	ContinuationToken string
	// StartAfter is the v2 start key (used when no continuation token is set).
	StartAfter string
	// VersionIDMarker is the version id to start from (ListObjectVersions).
	VersionIDMarker string
	// MaxKeys limits the number of keys returned: 1-1000, 0 for the
	// default (1000), ListLimitNone for none.
	MaxKeys    int
	FetchOwner bool
	// IncludeVersions requests all versions and delete markers.
	IncludeVersions bool
}

// ListObjectsResult is the result of a listing.
type ListObjectsResult struct {
	Objects        []ObjectInfo
	CommonPrefixes []string
	IsTruncated    bool
	// NextMarker (v1), NextContinuationToken (v2) and NextVersionIDMarker
	// (versions) resume a truncated listing.
	NextMarker            string
	NextContinuationToken string
	NextVersionIDMarker   string
}

// MultipartUpload is an in-progress multipart upload.
type MultipartUpload struct {
	Bucket       string
	Key          string
	UploadID     string
	Initiated    time.Time
	StorageClass string
	ContentType  string
	UserMetadata map[string]string
	Tags         map[string]string
	Checksum     ChecksumAlgorithm
	// ChecksumType is how the checksum of the completed object is
	// computed (empty without a checksum algorithm).
	ChecksumType ChecksumType
	// Encryption is the server-side encryption of the upload (zero when
	// not encrypted).
	Encryption EncryptionInfo
}

// Part is an uploaded part of a multipart upload.
type Part struct {
	Number  int
	ETag    string
	Size    int64
	ModTime time.Time
	// ActualSize is the size before any transparent compression/encryption.
	ActualSize int64
	Checksum   Checksum
	// Encryption is the server-side encryption of the part (the one of
	// its upload).
	Encryption EncryptionInfo
}

// CompletePart is a part reference supplied to CompleteMultipartUpload.
type CompletePart struct {
	Number   int
	ETag     string
	Checksum Checksum
}

// ListMultipartUploadsInput are the parameters of ListMultipartUploads.
type ListMultipartUploadsInput struct {
	Bucket         string
	Prefix         string
	Delimiter      string
	KeyMarker      string
	UploadIDMarker string
	// MaxUploads limits the uploads returned, as MaxKeys does.
	MaxUploads int
}

// ListMultipartUploadsResult is the result of ListMultipartUploads.
type ListMultipartUploadsResult struct {
	Uploads            []MultipartUpload
	CommonPrefixes     []string
	IsTruncated        bool
	NextKeyMarker      string
	NextUploadIDMarker string
}

// ListPartsInput are the parameters of ListParts.
type ListPartsInput struct {
	Bucket           string
	Key              string
	UploadID         string
	PartNumberMarker int
	// MaxParts limits the parts returned, as MaxKeys does.
	MaxParts int
}

// ListPartsResult is the result of ListParts.
type ListPartsResult struct {
	Upload               MultipartUpload
	Parts                []Part
	IsTruncated          bool
	NextPartNumberMarker int
}

// ValidateObjectKey validates an object key: non-empty, at most
// MaxObjectKeyLength bytes, valid UTF-8, no NUL bytes, no "//" and no "." or
// ".." path segments (keys map to directories on disk). Invalid keys return
// ErrInvalidObjectName.
func ValidateObjectKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalidObjectName)
	}
	if len(key) > MaxObjectKeyLength {
		return fmt.Errorf("%w: key longer than %d bytes", ErrInvalidObjectName, MaxObjectKeyLength)
	}
	if !utf8.ValidString(key) {
		return fmt.Errorf("%w: key is not valid UTF-8", ErrInvalidObjectName)
	}
	if strings.ContainsRune(key, 0) {
		return fmt.Errorf("%w: key contains NUL byte", ErrInvalidObjectName)
	}
	if strings.Contains(key, "//") {
		return fmt.Errorf("%w: key contains empty path segment", ErrInvalidObjectName)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("%w: key contains %q path segment", ErrInvalidObjectName, seg)
		}
	}
	return nil
}

// ValidateObjectPrefix validates a listing prefix: same rules as an object
// key except that it may be empty and may end with a slash.
func ValidateObjectPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return ValidateObjectKey(strings.TrimSuffix(prefix, "/"))
}
