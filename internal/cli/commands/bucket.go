package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

func newMbCmd(e *env) *cobra.Command {
	var withVersioning, withLock, withHLS, withImages, ignoreExisting bool
	var region string
	cmd := &cobra.Command{
		Use:   "mb [flags] ALIAS/BUCKET...",
		Short: "Make buckets",
		Long: `Make buckets. --with-lock creates them with Object Lock (WORM retention and
legal holds, see "nexoctl retention" and "nexoctl legalhold"), which implies
versioning; an existing bucket can get it later only once its versioning is
enabled. --with-hls makes a video bucket: it adds the media rule "hls", with
which the media worker derives every rendition (240p to 2160p, up to the
height of each video), a poster and timeline thumbnails from every video
uploaded (see "nexoctl media"; it needs the Admin API and a media worker).
--with-images makes an image bucket: it adds the media rule "images", with which
the media worker derives every format (AVIF, WebP, JPEG, PNG) at every width
from 160 to 2560 pixels (up to the width of each image) from every image
uploaded. Both make a bucket for videos and images.`,
		Example: `  nexoctl mb --with-versioning local/photos
  nexoctl mb --with-lock local/vault
  nexoctl mb --with-hls local/videos
  nexoctl mb --with-images local/pictures`,
		Args:              minArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			for _, arg := range args {
				t, err := e.remoteTarget(ctx, arg, true)
				if err != nil {
					return err
				}
				if t.Key != "" {
					return cli.UsageError("%s: mb takes ALIAS/BUCKET without an object name", arg)
				}
				client, err := e.client(ctx, t.Alias)
				if err != nil {
					return err
				}
				rec := bucketRecord{Target: t.String(), Bucket: t.Bucket, Created: true, ObjectLock: withLock}
				msg := fmt.Sprintf("Bucket created successfully `%s`.", t)
				if withLock {
					msg = fmt.Sprintf("Bucket created successfully `%s` (with Object Lock).", t)
					rec.Versioning = string(cli.VersioningEnabled)
				}
				if err := client.MakeBucket(ctx, t.Bucket, cli.MakeBucketOptions{Region: region, ObjectLock: withLock}); err != nil {
					if !ignoreExisting || !errors.Is(err, domain.ErrBucketAlreadyExists) {
						return fmt.Errorf("%s: %w", t, err)
					}
					rec.Created = false
					msg = fmt.Sprintf("Bucket `%s` already exists.", t)
				}
				if withVersioning {
					if err := client.SetVersioning(ctx, t.Bucket, cli.VersioningEnabled); err != nil {
						return fmt.Errorf("%s: enable versioning: %w", t, err)
					}
					rec.Versioning = string(cli.VersioningEnabled)
				}
				if withHLS {
					admin, err := e.admin(ctx, t.Alias)
					if err == nil {
						_, _, err = upsertMediaRule(ctx, admin, t.Bucket, hlsMediaRule())
					}
					if err != nil {
						return fmt.Errorf("%s: media rule %q: %w", t, domain.HLSMediaRuleID, err)
					}
					rec.HLS = true
					if rec.Created {
						msg = strings.TrimSuffix(msg, ".") + ", with HLS video (media rule \"hls\": every rendition, a poster and thumbnails)."
					} else {
						msg += " HLS video set (media rule \"hls\")."
					}
				}
				if withImages {
					admin, err := e.admin(ctx, t.Alias)
					if err == nil {
						_, _, err = upsertMediaRule(ctx, admin, t.Bucket, imagesMediaRule())
					}
					if err != nil {
						return fmt.Errorf("%s: media rule %q: %w", t, domain.ImagesMediaRuleID, err)
					}
					rec.Images = true
					if rec.Created {
						msg = strings.TrimSuffix(msg, ".") + ", with image variants (media rule \"images\": every format at every width)."
					} else {
						msg += " Image variants set (media rule \"images\")."
					}
				}
				if err := e.out.Info(rec, msg); err != nil {
					return err
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&withVersioning, "with-versioning", false, "enable versioning on the new bucket")
	f.BoolVarP(&withLock, "with-lock", "l", false, "create the bucket with Object Lock (implies versioning)")
	f.BoolVar(&withHLS, "with-hls", false, `make a video bucket: the media rule "hls" (every rendition, a poster and thumbnails)`)
	f.BoolVar(&withImages, "with-images", false, `make an image bucket: the media rule "images" (every format at every width)`)
	f.BoolVarP(&ignoreExisting, "ignore-existing", "p", false, "do not fail when the bucket already exists")
	f.StringVar(&region, "region", "", "location constraint (default: the region of the alias)")
	return cmd
}

func newRbCmd(e *env) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rb [flags] ALIAS/BUCKET...",
		Short: "Remove buckets",
		Long: `Remove empty buckets. With --force every object, version, delete marker and
incomplete upload of the bucket is removed first.

--force respects Object Lock: a bucket that holds versions under retention or
legal hold is left untouched (exit code 4), nothing is removed.`,
		Example:           "  nexoctl rb --force local/scratch",
		Args:              minArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			for _, arg := range args {
				t, err := e.remoteTarget(ctx, arg, true)
				if err != nil {
					return err
				}
				if t.Key != "" {
					return cli.UsageError("%s: rb takes ALIAS/BUCKET without an object name", arg)
				}
				client, err := e.client(ctx, t.Alias)
				if err != nil {
					return err
				}
				rec := bucketRecord{Target: t.String(), Bucket: t.Bucket, Removed: true}
				if force {
					if err := e.checkUnlocked(ctx, client, t); err != nil {
						return err
					}
					var first error
					sum, err := cli.EmptyBucket(ctx, client, t.Bucket, func(r cli.RemoveResult) error {
						if r.Err != nil && first == nil {
							first = r.Err
						}
						return nil
					})
					if err != nil {
						return fmt.Errorf("%s: %w", t, err)
					}
					if err := batchError("removals", sum.Failed, sum.Removed+sum.Failed, first); err != nil {
						return fmt.Errorf("%s: %w", t, err)
					}
					rec.Objects = sum.Removed
				}
				if err := client.RemoveBucket(ctx, t.Bucket); err != nil {
					if errors.Is(err, domain.ErrBucketNotEmpty) && !force {
						return fmt.Errorf("%s: %w (use --force to remove its content)", t, err)
					}
					return fmt.Errorf("%s: %w", t, err)
				}
				if err := e.out.Info(rec, fmt.Sprintf("Removed `%s` successfully.", t)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove the content of the bucket first")
	return cmd
}

// maxLockedReport bounds the protected versions listed by rb --force.
const maxLockedReport = 10

// checkUnlocked refuses to empty a bucket that holds versions protected by
// Object Lock (rb --force): it lists the first ones and removes nothing.
// An endpoint without Object Lock, or a caller not allowed to read it, is
// not checked (the server still protects the versions).
func (e *env) checkUnlocked(ctx context.Context, client cli.S3Client, t cli.Target) error {
	now := e.deps.Clock.Now()
	locked, err := cli.LockedVersions(ctx, client, t.Bucket, now, maxLockedReport+1)
	switch {
	case errors.Is(err, domain.ErrNotImplemented), errors.Is(err, domain.ErrAccessDenied) && len(locked) == 0:
		return nil
	case err != nil:
		return fmt.Errorf("%s: %w", t, err)
	case len(locked) == 0:
		return nil
	}
	for i, v := range locked {
		if i == maxLockedReport {
			e.out.Warn("...")
			break
		}
		version := v.VersionID
		if version == "" {
			version = "null"
		}
		e.out.Warn(fmt.Sprintf("`%s` (version %s) is protected: %s", t.Child(v.Key), version, v.Reason(now)))
	}
	count := fmt.Sprintf("%d %s", len(locked), plural(len(locked), "version", "versions"))
	if len(locked) > maxLockedReport {
		count = fmt.Sprintf("more than %d versions", maxLockedReport)
	}
	return fmt.Errorf("%s: %s protected by Object Lock, nothing removed (release the legal holds, wait for the retentions or remove the GOVERNANCE ones with \"rm --versions --bypass\"): %w",
		t, count, domain.ErrObjectLocked)
}
