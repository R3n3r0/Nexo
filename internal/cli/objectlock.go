package cli

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Validity is the period of an Object Lock retention (retention set
// --validity): a number of days or of years.
type Validity struct {
	Days  int
	Years int
}

// ParseValidity parses a retention period: "30d" (days) or "1y" (years),
// a positive number of one unit.
func ParseValidity(s string) (Validity, error) {
	in := strings.ToLower(strings.TrimSpace(s))
	if len(in) < 2 {
		return Validity{}, UsageError("invalid validity %q (e.g. 30d or 1y)", s)
	}
	n, err := strconv.Atoi(in[:len(in)-1])
	if err != nil || n <= 0 {
		return Validity{}, UsageError("invalid validity %q: a positive number of days (30d) or years (1y)", s)
	}
	switch in[len(in)-1] {
	case 'd':
		return Validity{Days: n}, nil
	case 'y':
		return Validity{Years: n}, nil
	}
	return Validity{}, UsageError("invalid validity %q: the unit is d (days) or y (years)", s)
}

// String renders the period ("30d", "1y").
func (v Validity) String() string {
	if v.Years > 0 {
		return strconv.Itoa(v.Years) + "y"
	}
	return strconv.Itoa(v.Days) + "d"
}

// Until returns the end of the period started at now, truncated to the
// second.
func (v Validity) Until(now time.Time) time.Time {
	return now.UTC().AddDate(v.Years, 0, v.Days).Truncate(time.Second)
}

// LockTargetOptions select the versions of the retention and legalhold
// commands.
type LockTargetOptions struct {
	// Recursive applies to every object under the key used as a prefix.
	Recursive bool
	// Versions applies to every version instead of the latest one.
	Versions bool
	// VersionID applies to one version of a single object.
	VersionID string
}

// LockTargets iterates over the object versions selected by opts under
// bucket/key: one object (its latest version, or opts.VersionID), or with
// Recursive the objects under the prefix (their latest version, or every
// version with Versions). Delete markers carry no Object Lock state and are
// skipped. An object version with an empty VersionID is the latest one.
func LockTargets(ctx context.Context, client S3Client, bucket, key string, opts LockTargetOptions) iter.Seq2[ObjectVersion, error] {
	return func(yield func(ObjectVersion, error) bool) {
		if opts.VersionID != "" && (opts.Recursive || opts.Versions) {
			yield(ObjectVersion{}, UsageError("--version-id selects one version: it excludes --recursive and --versions"))
			return
		}
		if !opts.Recursive {
			if key == "" || strings.HasSuffix(key, "/") {
				yield(ObjectVersion{}, UsageError("%s/%s is a bucket or a prefix: use --recursive", bucket, key))
				return
			}
			if !opts.Versions {
				yield(ObjectVersion{Key: key, VersionID: opts.VersionID}, nil)
				return
			}
		}
		match := func(k string) bool { return k == key }
		if opts.Recursive {
			match = func(k string) bool { return true }
			if key != "" && !strings.HasSuffix(key, "/") {
				match = func(k string) bool { return k == key || strings.HasPrefix(k, key+"/") }
			}
		}
		found := false
		for o, err := range client.List(ctx, bucket, ListOptions{Prefix: key, Recursive: true, Versions: opts.Versions}) {
			if err != nil {
				yield(ObjectVersion{}, err)
				return
			}
			if o.Prefix || o.DeleteMarker || !match(o.Key) {
				continue
			}
			found = true
			v := ObjectVersion{Key: o.Key}
			if opts.Versions {
				v.VersionID = o.VersionID
			}
			if !yield(v, nil) {
				return
			}
		}
		if !found && !opts.Recursive {
			yield(ObjectVersion{}, fmt.Errorf("%w: %s", domain.ErrObjectNotFound, key))
		}
	}
}

// LockedVersion is a version that Object Lock protects from deletion.
type LockedVersion struct {
	Key       string
	VersionID string
	Retention domain.ObjectRetention
	LegalHold domain.LegalHoldStatus
}

// Reason renders why the version is protected.
func (v LockedVersion) Reason(now time.Time) string {
	var parts []string
	if v.Retention.Active(now) {
		parts = append(parts, fmt.Sprintf("%s retention until %s", v.Retention.Mode, FormatTime(v.Retention.RetainUntil)))
	}
	if v.LegalHold == domain.LegalHoldOn {
		parts = append(parts, "legal hold")
	}
	return strings.Join(parts, ", ")
}

// LockedVersions returns the versions of a bucket that Object Lock
// protects at now (an active retention or a legal hold ON), at most limit
// of them (0: all). A bucket without Object Lock has none; the state of
// each version is read with Stat, so the caller needs
// s3:GetObjectRetention and s3:GetObjectLegalHold to see it.
func LockedVersions(ctx context.Context, client S3Client, bucket string, now time.Time, limit int) ([]LockedVersion, error) {
	cfg, err := client.GetObjectLock(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}
	var out []LockedVersion
	for o, err := range client.List(ctx, bucket, ListOptions{Recursive: true, Versions: true}) {
		if err != nil {
			return out, err
		}
		if o.Prefix || o.DeleteMarker {
			continue
		}
		info, err := client.Stat(ctx, bucket, o.Key, StatOptions{VersionID: o.VersionID})
		if err != nil {
			if errors.Is(err, domain.ErrObjectNotFound) || errors.Is(err, domain.ErrNoSuchVersion) {
				continue
			}
			return out, fmt.Errorf("%s (version %s): %w", o.Key, o.VersionID, err)
		}
		if info.Retention.Active(now) || info.LegalHold == domain.LegalHoldOn {
			out = append(out, LockedVersion{Key: o.Key, VersionID: o.VersionID, Retention: info.Retention, LegalHold: info.LegalHold})
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}
