package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// quotaRecord is the quota of a bucket (C-OBJ-09).
type quotaRecord struct {
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	// Type is "hard", or "" without a quota.
	Type string `json:"type,omitempty"`
	// Size is the limit in bytes, 0 without a quota.
	Size uint64 `json:"size"`
	// Used and Objects are the usage of the bucket (quota info, when the
	// caller may read it).
	Used    *uint64 `json:"used,omitempty"`
	Objects *uint64 `json:"objects,omitempty"`
}

// encryptionRecord is the default encryption of a bucket (C-OBJ-09).
type encryptionRecord struct {
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	// Algorithm is AES256 (sse-s3), aws:kms (sse-kms) or "" without a
	// default encryption.
	Algorithm string `json:"algorithm,omitempty"`
	KMSKeyID  string `json:"kmsKeyId,omitempty"`
}

// bucketAdmin resolves ALIAS/BUCKET and the Admin API client of the alias:
// the bucket settings without an S3 operation (quota) go through the
// Admin API of a Nexo server.
func (e *env) bucketAdmin(ctx context.Context, arg, what string) (cli.Target, cli.AdminClient, error) {
	t, err := e.remoteTarget(ctx, arg, true)
	if err != nil {
		return cli.Target{}, nil, err
	}
	if t.Key != "" {
		return cli.Target{}, nil, cli.UsageError("%s: %s applies to ALIAS/BUCKET", arg, what)
	}
	client, err := e.admin(ctx, t.Alias)
	if err != nil {
		return cli.Target{}, nil, err
	}
	return t, client, nil
}

func newQuotaCmd(e *env) *cobra.Command {
	cmd := groupCmd("quota", "Manage the hard quota of buckets (Admin API)")
	cmd.Long = `Manage the hard quota of buckets: writes that would exceed it are refused.
The quota is a setting of the Nexo Admin API (admin:SetBucketQuota).`
	cmd.AddCommand(newQuotaSetCmd(e), newQuotaInfoCmd(e), newQuotaClearCmd(e))
	return cmd
}

func newQuotaSetCmd(e *env) *cobra.Command {
	var size string
	cmd := &cobra.Command{
		Use:               "set ALIAS/BUCKET --size SIZE",
		Short:             "Set the hard quota of a bucket",
		Example:           "  nexoctl quota set local/backups --size 500GiB",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if size == "" {
				return cli.UsageError("--size is required (use \"quota clear\" to remove the quota)")
			}
			n, err := cli.ParseSize(size)
			if err != nil {
				return err
			}
			if n <= 0 {
				return cli.UsageError("--size must be positive (use \"quota clear\" to remove the quota)")
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "quota")
			if err != nil {
				return err
			}
			q, err := client.SetBucketQuota(ctx, t.Bucket, nexoadmin.BucketQuota{Type: "hard", Size: uint64(n)})
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			// #nosec G115 -- display of a quota size, far below 2^63
			return e.out.Info(quotaRecord{Target: t.String(), Bucket: t.Bucket, Type: q.Type, Size: q.Size},
				fmt.Sprintf("Set a hard quota of %s on `%s`.", cli.HumanSize(int64(q.Size)), t))
		},
	}
	cmd.Flags().StringVar(&size, "size", "", "limit, e.g. 10GiB, 500MB or bytes")
	return cmd
}

func newQuotaInfoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "info ALIAS/BUCKET",
		Short:             "Show the quota of a bucket",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "quota")
			if err != nil {
				return err
			}
			q, err := client.GetBucketQuota(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			rec := quotaRecord{Target: t.String(), Bucket: t.Bucket, Type: q.Type, Size: q.Size}
			usage := ""
			// The usage is informative: a caller that may read the quota
			// but not the bucket summary still gets the quota.
			if b, err := client.GetBucket(ctx, t.Bucket); err == nil && !b.Partial {
				rec.Used, rec.Objects = &b.Size, &b.Objects
				usage = fmt.Sprintf("used %s in %d %s", cli.HumanSize(int64(b.Size)), b.Objects, plural(int(b.Objects), "object", "objects")) // #nosec G115 -- display of a bucket size and object count, far below 2^63
				if q.Size > 0 {
					usage += fmt.Sprintf(", %.1f%%", 100*float64(b.Size)/float64(q.Size))
				}
			}
			text := fmt.Sprintf("`%s` has no quota", t)
			if q.Size > 0 {
				text = fmt.Sprintf("`%s` has a %s quota of %s", t, q.Type, cli.HumanSize(int64(q.Size))) // #nosec G115 -- display of a quota size, far below 2^63
			}
			if usage != "" {
				text += " (" + usage + ")"
			}
			return e.out.Result(rec, text+".")
		},
	}
}

func newQuotaClearCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "clear ALIAS/BUCKET",
		Short:             "Remove the quota of a bucket",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "quota")
			if err != nil {
				return err
			}
			if _, err := client.SetBucketQuota(ctx, t.Bucket, nexoadmin.BucketQuota{}); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(quotaRecord{Target: t.String(), Bucket: t.Bucket}, fmt.Sprintf("Removed the quota of `%s`.", t))
		},
	}
}

func newEncryptCmd(e *env) *cobra.Command {
	cmd := groupCmd("encrypt", "Manage the default encryption of buckets")
	cmd.Long = `Manage the default server-side encryption of buckets: sse-s3 (AES256) or sse-kms
(aws:kms, with an optional key ID). It applies to the objects written from then
on; existing objects are not rewritten.`
	cmd.AddCommand(newEncryptSetCmd(e), newEncryptInfoCmd(e), newEncryptClearCmd(e))
	return cmd
}

// encryptionAlgorithm maps the names of the command line to the S3
// algorithms.
func encryptionAlgorithm(name string) (string, error) {
	switch strings.ToLower(name) {
	case "sse-s3", "aes256":
		return "AES256", nil
	case "sse-kms", "aws:kms":
		return "aws:kms", nil
	}
	return "", cli.UsageError("unknown encryption %q (sse-s3 or sse-kms)", name)
}

// encryptionText describes a default encryption.
func encryptionText(enc *nexoadmin.BucketEncryption) string {
	switch {
	case enc == nil:
		return "no default encryption"
	case enc.Algorithm == "aws:kms" && enc.KMSKeyID != "":
		return "SSE-KMS with key " + enc.KMSKeyID
	case enc.Algorithm == "aws:kms":
		return "SSE-KMS"
	default:
		return "SSE-S3 (AES256)"
	}
}

func newEncryptSetCmd(e *env) *cobra.Command {
	var kmsKey string
	cmd := &cobra.Command{
		Use:   "set sse-s3|sse-kms [KEY] ALIAS/BUCKET",
		Short: "Set the default encryption of a bucket",
		Long: `Set the default encryption of a bucket: sse-s3, or sse-kms with an optional KMS
key (the default key of the KMS without; "nexoctl admin kms key ls" lists them).`,
		Example: `  nexoctl encrypt set sse-s3 local/docs
  nexoctl encrypt set sse-kms app-key local/docs
  nexoctl encrypt set sse-kms local/docs --kms-key-id app-key`,
		Args: func(cmd *cobra.Command, args []string) error { return argsBetween(cmd, args, 2, 3) },
		RunE: func(cmd *cobra.Command, args []string) error {
			algorithm, err := encryptionAlgorithm(args[0])
			if err != nil {
				return err
			}
			target := args[len(args)-1]
			if len(args) == 3 {
				if algorithm != "aws:kms" {
					return cli.UsageError("a KEY needs sse-kms: encrypt set sse-kms KEY ALIAS/BUCKET")
				}
				if kmsKey != "" && kmsKey != args[1] {
					return cli.UsageError("give the KMS key once: as KEY or with --kms-key-id")
				}
				kmsKey = args[1]
			}
			if kmsKey != "" && algorithm != "aws:kms" {
				return cli.UsageError("--kms-key-id needs sse-kms")
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, target, "encryption")
			if err != nil {
				return err
			}
			enc, err := client.SetBucketEncryption(ctx, t.Bucket, nexoadmin.BucketEncryption{Algorithm: algorithm, KMSKeyID: kmsKey})
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(encryptionRecord{Target: t.String(), Bucket: t.Bucket, Algorithm: enc.Algorithm, KMSKeyID: enc.KMSKeyID},
				fmt.Sprintf("Default encryption of `%s`: %s.", t, encryptionText(enc)))
		},
	}
	cmd.Flags().StringVar(&kmsKey, "kms-key-id", "", "key of SSE-KMS (default: the key of the server)")
	return cmd
}

func newEncryptInfoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "info ALIAS/BUCKET",
		Short:             "Show the default encryption of a bucket",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "encryption")
			if err != nil {
				return err
			}
			enc, err := client.GetBucketEncryption(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			rec := encryptionRecord{Target: t.String(), Bucket: t.Bucket}
			if enc != nil {
				rec.Algorithm, rec.KMSKeyID = enc.Algorithm, enc.KMSKeyID
			}
			return e.out.Result(rec, fmt.Sprintf("`%s`: %s.", t, encryptionText(enc)))
		},
	}
}

func newEncryptClearCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "clear ALIAS/BUCKET",
		Short:             "Remove the default encryption of a bucket",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "encryption")
			if err != nil {
				return err
			}
			if err := client.DeleteBucketEncryption(ctx, t.Bucket); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(encryptionRecord{Target: t.String(), Bucket: t.Bucket}, fmt.Sprintf("Removed the default encryption of `%s`.", t))
		},
	}
}
