package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// RemoveOptions select the objects removed by rm and rb --force (docs/02
// C-OBJ-04).
type RemoveOptions struct {
	// Recursive removes everything under the key used as a prefix: the
	// object named key and the objects under key + "/".
	Recursive bool
	// Versions removes every version and delete marker instead of the
	// latest version.
	Versions bool
	// VersionID removes one version of a single object.
	VersionID string
	// OlderThan and NewerThan, when not zero, keep the objects modified
	// more recently / longer ago than Now minus the duration.
	OlderThan time.Duration
	NewerThan time.Duration
	// DryRun reports the objects without removing them.
	DryRun bool
	// Now is the reference time of OlderThan and NewerThan.
	Now time.Time
	// BypassGovernance removes the versions a GOVERNANCE retention
	// protects (rm --bypass), where the caller is allowed to.
	BypassGovernance bool
}

// RemoveSummary counts the outcome of RemoveObjects.
type RemoveSummary struct {
	Removed int
	Failed  int
}

// RemoveObjects removes the objects (or the versions) of bucket/key
// selected by opts; report is called for every removed object (or, with
// DryRun, every selected one) including the failed ones (RemoveResult.Err).
// The error is the failure of a listing or of a whole request.
func RemoveObjects(ctx context.Context, client S3Client, bucket, key string, opts RemoveOptions, report func(RemoveResult) error) (RemoveSummary, error) {
	var sum RemoveSummary
	if opts.VersionID != "" && (opts.Recursive || opts.Versions) {
		return sum, UsageError("--version-id removes one version: it excludes --recursive and --versions")
	}
	if !opts.Recursive && (key == "" || strings.HasSuffix(key, "/")) {
		return sum, UsageError("%s/%s is a bucket or a prefix: use --recursive --force", bucket, key)
	}
	if opts.VersionID != "" {
		// One version: the server reports a missing version.
		return sum, removeBatch(ctx, client, bucket, []ObjectVersion{{Key: key, VersionID: opts.VersionID}}, opts, &sum, report)
	}
	if !opts.Recursive && !opts.Versions && opts.OlderThan == 0 && opts.NewerThan == 0 {
		// A single object: removing a missing object is not an error in
		// S3, but it is for rm.
		if _, err := client.Stat(ctx, bucket, key, StatOptions{}); err != nil {
			return sum, err
		}
		return sum, removeBatch(ctx, client, bucket, []ObjectVersion{{Key: key}}, opts, &sum, report)
	}
	listPrefix := key
	match := func(k string) bool { return true }
	switch {
	case !opts.Recursive:
		match = func(k string) bool { return k == key }
	case key != "" && !strings.HasSuffix(key, "/"):
		match = func(k string) bool { return k == key || strings.HasPrefix(k, key+"/") }
	}
	var batch []ObjectVersion
	found := false
	for o, err := range client.List(ctx, bucket, ListOptions{Prefix: listPrefix, Recursive: true, Versions: opts.Versions}) {
		if err != nil {
			return sum, err
		}
		if o.Prefix || !match(o.Key) || !opts.keep(o) {
			continue
		}
		if !opts.Versions && o.DeleteMarker {
			continue
		}
		found = true
		v := ObjectVersion{Key: o.Key}
		if opts.Versions {
			v.VersionID = o.VersionID
		}
		batch = append(batch, v)
		if len(batch) == deleteBatch {
			if err := removeBatch(ctx, client, bucket, batch, opts, &sum, report); err != nil {
				return sum, err
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := removeBatch(ctx, client, bucket, batch, opts, &sum, report); err != nil {
			return sum, err
		}
	}
	if !found && !opts.Recursive && opts.OlderThan == 0 && opts.NewerThan == 0 {
		return sum, fmt.Errorf("%w: %s", domain.ErrObjectNotFound, key)
	}
	return sum, nil
}

// keep applies the age filters.
func (o RemoveOptions) keep(info ObjectInfo) bool {
	if o.OlderThan > 0 && !info.LastModified.Before(o.Now.Add(-o.OlderThan)) {
		return false
	}
	if o.NewerThan > 0 && !info.LastModified.After(o.Now.Add(-o.NewerThan)) {
		return false
	}
	return true
}

func removeBatch(ctx context.Context, client S3Client, bucket string, batch []ObjectVersion, opts RemoveOptions, sum *RemoveSummary, report func(RemoveResult) error) error {
	var results []RemoveResult
	if opts.DryRun {
		for _, v := range batch {
			results = append(results, RemoveResult{Key: v.Key, VersionID: v.VersionID})
		}
	} else {
		var err error
		if results, err = client.Remove(ctx, bucket, batch, DeleteOptions{BypassGovernance: opts.BypassGovernance}); err != nil {
			return err
		}
	}
	for _, r := range results {
		if r.Err != nil {
			sum.Failed++
		} else {
			sum.Removed++
		}
		if err := report(r); err != nil {
			return err
		}
	}
	return nil
}

// RemoveUploads aborts the incomplete multipart uploads of key (or under
// it with recursive); report is called for each one.
func RemoveUploads(ctx context.Context, client S3Client, bucket, key string, recursive, dryRun bool, report func(UploadInfo, error) error) (RemoveSummary, error) {
	var sum RemoveSummary
	var uploads []UploadInfo
	for u, err := range client.ListUploads(ctx, bucket, key) {
		if err != nil {
			return sum, err
		}
		if !recursive && u.Key != key {
			continue
		}
		uploads = append(uploads, u)
	}
	for _, u := range uploads {
		var err error
		if !dryRun {
			err = client.AbortUpload(ctx, bucket, u.Key, u.UploadID)
		}
		if err != nil {
			sum.Failed++
		} else {
			sum.Removed++
		}
		if rerr := report(u, err); rerr != nil {
			return sum, rerr
		}
	}
	return sum, nil
}

// EmptyBucket removes every object version, delete marker and incomplete
// upload of a bucket (rb --force). Versions protected by Object Lock are
// reported as failures, never bypassed.
func EmptyBucket(ctx context.Context, client S3Client, bucket string, report func(RemoveResult) error) (RemoveSummary, error) {
	sum, err := RemoveObjects(ctx, client, bucket, "", RemoveOptions{Recursive: true, Versions: true}, report)
	if err != nil {
		return sum, err
	}
	uploads, err := RemoveUploads(ctx, client, bucket, "", true, false, func(UploadInfo, error) error { return nil })
	sum.Failed += uploads.Failed
	return sum, err
}
