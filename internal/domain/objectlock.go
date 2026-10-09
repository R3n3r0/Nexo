package domain

import (
	"fmt"
	"strings"
	"time"
)

// RetentionMode is the S3 Object Lock retention mode of an object version,
// or of the default retention of a bucket.
type RetentionMode string

// Retention modes.
const (
	// RetentionGovernance protects a version until its retain-until date
	// from deletion and from a shorter retention, except for the requests
	// allowed to bypass it (s3:BypassGovernanceRetention with the
	// x-amz-bypass-governance-retention header).
	RetentionGovernance RetentionMode = "GOVERNANCE"
	// RetentionCompliance protects a version until its retain-until date
	// for everyone, root included: the date can only be extended.
	RetentionCompliance RetentionMode = "COMPLIANCE"
)

// IsValid reports whether m is GOVERNANCE or COMPLIANCE.
func (m RetentionMode) IsValid() bool { return m == RetentionGovernance || m == RetentionCompliance }

// ParseRetentionMode parses a retention mode, case-insensitively. Invalid
// modes return ErrInvalidArgument.
func ParseRetentionMode(s string) (RetentionMode, error) {
	m := RetentionMode(strings.ToUpper(strings.TrimSpace(s)))
	if !m.IsValid() {
		return "", fmt.Errorf("%w: unknown object lock retention mode %q", ErrInvalidArgument, s)
	}
	return m, nil
}

// LegalHoldStatus is the S3 Object Lock legal hold of an object version:
// ON protects the version from deletion whatever its retention.
type LegalHoldStatus string

// Legal hold statuses. The empty status means that no legal hold was ever
// set on the version.
const (
	LegalHoldOn  LegalHoldStatus = "ON"
	LegalHoldOff LegalHoldStatus = "OFF"
)

// IsValid reports whether s is ON or OFF.
func (s LegalHoldStatus) IsValid() bool { return s == LegalHoldOn || s == LegalHoldOff }

// ParseLegalHold parses a legal hold status, case-insensitively. Invalid
// values return ErrInvalidArgument.
func ParseLegalHold(s string) (LegalHoldStatus, error) {
	h := LegalHoldStatus(strings.ToUpper(strings.TrimSpace(s)))
	if !h.IsValid() {
		return "", fmt.Errorf("%w: unknown object lock legal hold status %q", ErrInvalidArgument, s)
	}
	return h, nil
}

// ObjectRetention is the retention of an object version: its mode and the
// date until which the version is protected.
type ObjectRetention struct {
	Mode        RetentionMode
	RetainUntil time.Time
}

// IsZero reports whether no retention is set.
func (r ObjectRetention) IsZero() bool { return r.Mode == "" && r.RetainUntil.IsZero() }

// Active reports whether the retention protects the version at now.
func (r ObjectRetention) Active(now time.Time) bool {
	return r.Mode.IsValid() && r.RetainUntil.After(now)
}

// DefaultRetention is the default retention of the versions written to a
// bucket with Object Lock (the Rule of its ObjectLockConfiguration):
// either Days or Years, counted from the creation of each version.
type DefaultRetention struct {
	Mode  RetentionMode
	Days  int
	Years int
}

// IsZero reports whether the bucket has no default retention.
func (d DefaultRetention) IsZero() bool { return d.Mode == "" && d.Days == 0 && d.Years == 0 }

// Validate checks a default retention: none, or a valid mode with a
// positive number of either days or years. Invalid rules return
// ErrMalformedXML (the S3 MalformedXML of PutObjectLockConfiguration).
func (d DefaultRetention) Validate() error {
	switch {
	case d.IsZero():
		return nil
	case !d.Mode.IsValid():
		return fmt.Errorf("%w: invalid default retention mode %q", ErrMalformedXML, d.Mode)
	case d.Days < 0 || d.Years < 0:
		return fmt.Errorf("%w: the default retention period must be positive", ErrMalformedXML)
	case (d.Days > 0) == (d.Years > 0):
		return fmt.Errorf("%w: the default retention needs either Days or Years", ErrMalformedXML)
	}
	return nil
}

// RetentionAt returns the retention of a version created at t: the zero
// retention when there is no default.
func (d DefaultRetention) RetentionAt(t time.Time) ObjectRetention {
	if !d.Mode.IsValid() || (d.Days <= 0 && d.Years <= 0) {
		return ObjectRetention{}
	}
	return ObjectRetention{Mode: d.Mode, RetainUntil: t.UTC().AddDate(d.Years, 0, d.Days)}
}
