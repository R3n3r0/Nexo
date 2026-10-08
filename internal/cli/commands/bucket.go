package commands

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

func newMbCmd(e *env) *cobra.Command {
	var withVersioning, ignoreExisting bool
	var region string
	cmd := &cobra.Command{
		Use:               "mb [flags] ALIAS/BUCKET...",
		Short:             "Make buckets",
		Example:           "  nexoctl mb --with-versioning local/photos",
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
				rec := bucketRecord{Target: t.String(), Bucket: t.Bucket, Created: true}
				msg := fmt.Sprintf("Bucket created successfully `%s`.", t)
				if err := client.MakeBucket(ctx, t.Bucket, cli.MakeBucketOptions{Region: region}); err != nil {
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
				if err := e.out.Info(rec, msg); err != nil {
					return err
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&withVersioning, "with-versioning", false, "enable versioning on the new bucket")
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
incomplete upload of the bucket is removed first.`,
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
