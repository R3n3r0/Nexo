package domain

import "time"

// NullVersionID is the S3 name of the null version: the version written
// while the versioning of its bucket was off or suspended.
const NullVersionID = "null"

// DeleteMarkerError reports a read that resolved to a delete marker: the
// latest version of the key when the request names no version (Err wraps
// ErrObjectNotFound, S3 404 NoSuchKey) or the version the request names
// (Err wraps ErrMethodNotAllowed, S3 405 MethodNotAllowed). Transports
// answer with the x-amz-delete-marker, x-amz-version-id and Last-Modified
// headers of the marker.
type DeleteMarkerError struct {
	// VersionID is the version id of the marker ("null" for the null
	// version).
	VersionID string
	ModTime   time.Time
	// Err is the S3 error of the read (a *S3Error or a wrapped sentinel).
	Err error
}

// Error implements error.
func (e *DeleteMarkerError) Error() string {
	if e.Err == nil {
		return "delete marker"
	}
	return e.Err.Error()
}

// Unwrap returns the error of the read.
func (e *DeleteMarkerError) Unwrap() error { return e.Err }
