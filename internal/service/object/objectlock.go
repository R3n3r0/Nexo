package object

import (
	"context"
	"fmt"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Object Lock metadata of a version, persisted in xl.meta with the HTTP
// metadata (the names of the S3 headers). They are never copied by
// CopyObject: a copy gets the settings of its own request (or the default
// retention of its bucket).
const (
	metaLockMode        = "x-amz-object-lock-mode"
	metaLockRetainUntil = "x-amz-object-lock-retain-until-date"
	metaLockLegalHold   = "x-amz-object-lock-legal-hold"
)

// Events of the Object Lock changes (published like MinIO does, under
// s3:ObjectCreated:*).
const (
	eventPutRetention event.Name = "s3:ObjectCreated:PutRetention"
	eventPutLegalHold event.Name = "s3:ObjectCreated:PutLegalHold"
)

// errBucketNotLocked is the S3 answer to an Object Lock setting on a
// bucket created without object locking.
func errBucketNotLocked() error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", "Bucket is missing Object Lock Configuration")
}

// errObjectLocked is the S3 answer to a change a lock forbids.
func errObjectLocked() error {
	return domain.NewS3Error(domain.ErrObjectLocked, "AccessDenied", "Access Denied because object protected by object lock.")
}

// errNoObjectLock is the S3 answer to a read of a retention or legal hold
// that was never set.
func errNoObjectLock() error {
	return domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchObjectLockConfiguration", "The specified object does not have a ObjectLock configuration")
}

// errLockMD5Required is the S3 answer to a write with Object Lock settings
// and no integrity check of its body.
func errLockMD5Required() error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
		"Content-MD5 OR x-amz-checksum- HTTP header is required for Put Object requests with Object Lock parameters")
}

// formatRetainUntil renders a retain-until date as stored and sent (ISO
// 8601 with milliseconds, UTC).
func formatRetainUntil(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

// parseRetainUntil parses a stored retain-until date.
func parseRetainUntil(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// setRetentionMeta records the retention r (none for the zero retention).
func setRetentionMeta(meta map[string]string, r domain.ObjectRetention) {
	if r.IsZero() {
		delete(meta, metaLockMode)
		delete(meta, metaLockRetainUntil)
		return
	}
	meta[metaLockMode] = string(r.Mode)
	meta[metaLockRetainUntil] = formatRetainUntil(r.RetainUntil)
}

// setLegalHoldMeta records the legal hold (nothing for the empty status).
func setLegalHoldMeta(meta map[string]string, hold domain.LegalHoldStatus) {
	if hold != "" {
		meta[metaLockLegalHold] = string(hold)
	}
}

// validateNewRetention checks a retention requested by a client: a valid
// mode and a retain-until date in the future.
func validateNewRetention(r domain.ObjectRetention, now time.Time) error {
	switch {
	case !r.Mode.IsValid():
		return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Unknown wormMode directive: "+string(r.Mode))
	case r.RetainUntil.IsZero():
		return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "x-amz-object-lock-retain-until-date and x-amz-object-lock-mode must both be supplied")
	case !r.RetainUntil.After(now):
		return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "The retain until date must be in the future!")
	}
	return nil
}

// checkLockRequest validates the Object Lock settings of a write against
// the bucket settings: they need a bucket with object locking, a valid
// legal hold and a valid retention ending in the future.
func checkLockRequest(cfg BucketConfig, r domain.ObjectRetention, hold domain.LegalHoldStatus, now time.Time) error {
	if r.IsZero() && hold == "" {
		return nil
	}
	if !cfg.ObjectLock {
		return errBucketNotLocked()
	}
	if hold != "" && !hold.IsValid() {
		return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Unknown legal hold status: "+string(hold))
	}
	if !r.IsZero() {
		return validateNewRetention(r, now)
	}
	return nil
}

// applyDefaultRetention gives a new version of a bucket with object
// locking the default retention of the bucket, counted from its
// modification time, unless the write set a retention.
func applyDefaultRetention(meta map[string]string, cfg BucketConfig, modTime time.Time) {
	if !cfg.ObjectLock || meta[metaLockMode] != "" {
		return
	}
	setRetentionMeta(meta, cfg.DefaultRetention.RetentionAt(modTime))
}

// checkDeletable decides whether a version may be removed at now (TS-U-12):
// a delete marker always; a version under legal hold never; a version
// under an active COMPLIANCE retention never, not even for root; under an
// active GOVERNANCE retention only with bypass.
func checkDeletable(info domain.ObjectInfo, now time.Time, bypass bool) error {
	if info.IsDeleteMarker {
		return nil
	}
	if info.LegalHold == domain.LegalHoldOn {
		return errObjectLocked()
	}
	if info.Retention.Active(now) && (info.Retention.Mode == domain.RetentionCompliance || !bypass) {
		return errObjectLocked()
	}
	return nil
}

// isLocked reports whether a version is protected at now, whatever the
// bypass: a legal hold or an active retention.
func isLocked(info domain.ObjectInfo, now time.Time) bool {
	return !info.IsDeleteMarker && (info.LegalHold == domain.LegalHoldOn || info.Retention.Active(now))
}

// checkRetentionChange decides whether the retention cur of a version may
// become next at now (TS-U-12): anything goes once cur has expired; an
// active retention may be extended (same or later date, the mode staying
// COMPLIANCE or becoming stricter); shortening it, removing it or turning
// COMPLIANCE into GOVERNANCE is allowed for an active GOVERNANCE retention
// with bypass only, never for COMPLIANCE.
func checkRetentionChange(cur, next domain.ObjectRetention, now time.Time, bypass bool) error {
	if !cur.Active(now) {
		return nil
	}
	extends := !next.IsZero() && !next.RetainUntil.Before(cur.RetainUntil)
	if cur.Mode == domain.RetentionCompliance {
		if extends && next.Mode == domain.RetentionCompliance {
			return nil
		}
		return errObjectLocked()
	}
	if extends || bypass {
		return nil
	}
	return errObjectLocked()
}

// requireObjectLock fails unless bucket exists and has object locking.
func (l *layer) requireObjectLock(ctx context.Context, bucket string) error {
	if err := validBucket(bucket); err != nil {
		return err
	}
	if err := l.statBucket(ctx, l.sets[0], bucket); err != nil {
		return err
	}
	cfg, err := l.bucketConfig(ctx, bucket)
	if err != nil {
		return err
	}
	if !cfg.ObjectLock {
		return errBucketNotLocked()
	}
	return nil
}

// GetObjectRetention implements Service.
func (l *layer) GetObjectRetention(ctx context.Context, bucket, key string, opts LockOptions) (domain.ObjectRetention, error) {
	if err := l.requireObjectLock(ctx, bucket); err != nil {
		return domain.ObjectRetention{}, err
	}
	info, err := l.GetObjectInfo(ctx, bucket, key, GetObjectOptions{VersionID: opts.VersionID})
	if err != nil {
		return domain.ObjectRetention{}, err
	}
	if info.Retention.IsZero() {
		return domain.ObjectRetention{}, errNoObjectLock()
	}
	return info.Retention, nil
}

// PutObjectRetention implements Service.
func (l *layer) PutObjectRetention(ctx context.Context, bucket, key string, retention domain.ObjectRetention, opts LockOptions) (domain.ObjectInfo, error) {
	if !retention.IsZero() {
		if err := validateNewRetention(retention, l.now()); err != nil {
			return domain.ObjectInfo{}, err
		}
	}
	if err := l.requireObjectLock(ctx, bucket); err != nil {
		return domain.ObjectInfo{}, err
	}
	return l.updateMeta(ctx, bucket, key, opts.VersionID, func(fi storage.FileInfo, meta map[string]string) error {
		cur := objectInfo(bucket, key, fi).Retention
		if err := checkRetentionChange(cur, retention, l.now(), opts.BypassGovernance); err != nil {
			return err
		}
		setRetentionMeta(meta, retention)
		return nil
	}, eventPutRetention)
}

// GetObjectLegalHold implements Service.
func (l *layer) GetObjectLegalHold(ctx context.Context, bucket, key string, opts LockOptions) (domain.LegalHoldStatus, error) {
	if err := l.requireObjectLock(ctx, bucket); err != nil {
		return "", err
	}
	info, err := l.GetObjectInfo(ctx, bucket, key, GetObjectOptions{VersionID: opts.VersionID})
	if err != nil {
		return "", err
	}
	if info.LegalHold == "" {
		return "", errNoObjectLock()
	}
	return info.LegalHold, nil
}

// PutObjectLegalHold implements Service.
func (l *layer) PutObjectLegalHold(ctx context.Context, bucket, key string, hold domain.LegalHoldStatus, opts LockOptions) (domain.ObjectInfo, error) {
	if !hold.IsValid() {
		return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML",
			fmt.Sprintf("The XML you provided was not well-formed or did not validate against our published schema: unknown legal hold status %q", hold))
	}
	if err := l.requireObjectLock(ctx, bucket); err != nil {
		return domain.ObjectInfo{}, err
	}
	return l.updateMeta(ctx, bucket, key, opts.VersionID, func(_ storage.FileInfo, meta map[string]string) error {
		setLegalHoldMeta(meta, hold)
		return nil
	}, eventPutLegalHold)
}
