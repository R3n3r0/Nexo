package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

func newAnonymousCmd(e *env) *cobra.Command {
	cmd := groupCmd("anonymous", "Manage the anonymous access of buckets and prefixes")
	cmd.Long = `Grant anonymous (unauthenticated) access to a bucket or to a prefix with a
preset, implemented as statements of the bucket policy:

  private   no anonymous access
  download  list and read
  upload    write (and abort multipart uploads)
  public    download + upload + delete

The statements of other prefixes and the custom statements are kept.`
	cmd.AddCommand(newAnonymousSetCmd(e), newAnonymousGetCmd(e), newAnonymousListCmd(e),
		newAnonymousGetJSONCmd(e), newAnonymousSetJSONCmd(e))
	return cmd
}

func newAnonymousSetCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "set private|download|upload|public ALIAS/BUCKET[/PREFIX]",
		Short: "Set the anonymous access of a bucket or prefix",
		Example: `  nexoctl anonymous set download local/website
  nexoctl anonymous set download local/data/public/`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAnonymousSet(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			preset, err := cli.ParseAnonymousPreset(args[0])
			if err != nil {
				return err
			}
			t, client, current, err := e.bucketPolicy(ctx, args[1])
			if err != nil {
				return err
			}
			next, err := cli.ApplyAnonymousPreset(current, t.Bucket, t.Key, preset)
			if err != nil {
				return err
			}
			if err := e.savePolicy(ctx, client, t.Bucket, next); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			rec := accessRecord{Target: t.String(), Bucket: t.Bucket, Prefix: t.Key, Access: string(preset)}
			return e.out.Info(rec, fmt.Sprintf("Access permission for `%s` is set to `%s`.", t, preset))
		},
	}
}

func newAnonymousGetCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "get ALIAS/BUCKET[/PREFIX]",
		Short:             "Show the anonymous access of a bucket or prefix",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, _, current, err := e.bucketPolicy(ctx, args[0])
			if err != nil {
				return err
			}
			preset := cli.DetectAnonymousPreset(current, t.Bucket, t.Key)
			rec := accessRecord{Target: t.String(), Bucket: t.Bucket, Prefix: t.Key, Access: string(preset)}
			return e.out.Result(rec, fmt.Sprintf("Access permission for `%s` is `%s`.", t, preset))
		},
	}
}

func newAnonymousListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS/BUCKET",
		Aliases:           []string{"ls"},
		Short:             "List the prefixes of a bucket with anonymous access",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, _, current, err := e.bucketPolicy(ctx, args[0])
			if err != nil {
				return err
			}
			if t.Key != "" {
				return cli.UsageError("%s: list takes ALIAS/BUCKET", args[0])
			}
			for _, r := range cli.ListAnonymousAccess(current, t.Bucket) {
				target := t.Child(r.Prefix)
				rec := accessRecord{Target: target.String(), Bucket: t.Bucket, Prefix: r.Prefix, Access: string(r.Preset)}
				if err := e.out.Result(rec, fmt.Sprintf("%-8s %s", r.Preset, target)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAnonymousGetJSONCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "get-json ALIAS/BUCKET",
		Short:             "Print the bucket policy",
		Long:              "Print the bucket policy document (an empty policy when the bucket has none).",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, _, current, err := e.bucketPolicy(ctx, args[0])
			if err != nil {
				return err
			}
			if t.Key != "" {
				return cli.UsageError("%s: get-json takes ALIAS/BUCKET", args[0])
			}
			if current == nil {
				current = &domain.Policy{Version: domain.PolicyVersion, Statement: []domain.Statement{}}
			}
			doc, err := json.MarshalIndent(current, "", "  ")
			if err != nil {
				return fmt.Errorf("encode policy: %w", err)
			}
			return e.out.Result(policyRecord{Target: t.String(), Bucket: t.Bucket, Policy: current}, string(doc))
		},
	}
}

func newAnonymousSetJSONCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "set-json FILE ALIAS/BUCKET",
		Short: "Replace the bucket policy with a JSON document",
		Long: `Replace the bucket policy with the document in FILE ("-" reads the standard
input). A document without statements removes the bucket policy.`,
		Example:           "  nexoctl anonymous set-json policy.json local/website",
		Args:              exactArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var data []byte
			var err error
			if args[0] == "-" {
				data, err = io.ReadAll(e.deps.Stdin)
			} else {
				data, err = os.ReadFile(args[0])
			}
			if err != nil {
				return err
			}
			p, err := cli.ParseBucketPolicy(string(data))
			if err != nil {
				return err
			}
			t, err := e.remoteTarget(ctx, args[1], true)
			if err != nil {
				return err
			}
			if t.Key != "" {
				return cli.UsageError("%s: set-json takes ALIAS/BUCKET", args[1])
			}
			client, err := e.client(ctx, t.Alias)
			if err != nil {
				return err
			}
			if len(p.Statement) == 0 {
				p = nil
			}
			if err := e.savePolicy(ctx, client, t.Bucket, p); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			msg := fmt.Sprintf("Bucket policy of `%s` updated.", t)
			if p == nil {
				msg = fmt.Sprintf("Bucket policy of `%s` removed.", t)
			}
			return e.out.Info(policyRecord{Target: t.String(), Bucket: t.Bucket, Policy: p}, msg)
		},
	}
}

// bucketPolicy parses a remote target and loads the bucket policy (nil
// when the bucket has none).
func (e *env) bucketPolicy(ctx context.Context, arg string) (cli.Target, cli.S3Client, *domain.Policy, error) {
	t, err := e.remoteTarget(ctx, arg, true)
	if err != nil {
		return t, nil, nil, err
	}
	client, err := e.client(ctx, t.Alias)
	if err != nil {
		return t, nil, nil, err
	}
	p, err := loadPolicy(ctx, client, t.Bucket)
	if err != nil {
		return t, nil, nil, fmt.Errorf("%s: %w", t, err)
	}
	return t, client, p, nil
}

func loadPolicy(ctx context.Context, client cli.S3Client, bucket string) (*domain.Policy, error) {
	doc, err := client.GetBucketPolicy(ctx, bucket)
	if errors.Is(err, domain.ErrPolicyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cli.ParseBucketPolicy(doc)
}

// savePolicy stores a bucket policy, or deletes it when p is nil.
func (e *env) savePolicy(ctx context.Context, client cli.S3Client, bucket string, p *domain.Policy) error {
	if p == nil {
		err := client.DeleteBucketPolicy(ctx, bucket)
		if errors.Is(err, domain.ErrPolicyNotFound) {
			return nil
		}
		return err
	}
	doc, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode policy: %w", err)
	}
	return client.PutBucketPolicy(ctx, bucket, string(doc))
}

// anonymousAccess returns the anonymous preset of a bucket prefix.
func (e *env) anonymousAccess(ctx context.Context, client cli.S3Client, bucket, prefix string) (cli.AnonymousPreset, error) {
	p, err := loadPolicy(ctx, client, bucket)
	if err != nil {
		if errors.Is(err, domain.ErrNotImplemented) || errors.Is(err, domain.ErrAccessDenied) {
			return "", nil
		}
		return "", err
	}
	return cli.DetectAnonymousPreset(p, bucket, prefix), nil
}

// completeAnonymousSet completes the preset, then the target.
func completeAnonymousSet(e *env) cobra.CompletionFunc {
	targets := completeTargets(e)
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) == 0 {
			var out []string
			for _, p := range []string{"private", "download", "upload", "public"} {
				if strings.HasPrefix(p, toComplete) {
					out = append(out, p)
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		}
		return targets(cmd, args, toComplete)
	}
}
