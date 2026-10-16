package commands

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/version"
)

// newVersionCmd prints the version of nexoctl and groups the versioning
// commands of the buckets (version enable|suspend|info).
func newVersionCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the nexoctl version, or manage bucket versioning",
		Long: `Without arguments, print the version of nexoctl. The subcommands manage the
versioning of buckets: enable, suspend, info.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return cli.UsageError("unknown command %q for %q", args[0], cmd.CommandPath())
			}
			return nil
		},
		RunE: func(*cobra.Command, []string) error {
			info := version.Get()
			return e.out.Result(info, "nexoctl "+info.String()+"\n"+info.LicenseLine())
		},
	}
	cmd.AddCommand(
		newVersioningCmd(e, "enable", "Enable the versioning of a bucket", cli.VersioningEnabled),
		newVersioningCmd(e, "suspend", "Suspend the versioning of a bucket", cli.VersioningSuspended),
		newVersioningCmd(e, "info", "Show the versioning state of a bucket", ""),
	)
	return cmd
}

func newVersioningCmd(e *env, name, short string, state cli.Versioning) *cobra.Command {
	return &cobra.Command{
		Use:               name + " ALIAS/BUCKET",
		Short:             short,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, err := e.remoteTarget(ctx, args[0], true)
			if err != nil {
				return err
			}
			if t.Key != "" {
				return cli.UsageError("%s: versioning applies to ALIAS/BUCKET", args[0])
			}
			client, err := e.client(ctx, t.Alias)
			if err != nil {
				return err
			}
			if state != "" {
				if err := client.SetVersioning(ctx, t.Bucket, state); err != nil {
					return fmt.Errorf("%s: %w", t, err)
				}
				verb := "enabled"
				if state == cli.VersioningSuspended {
					verb = "suspended"
				}
				rec := versioningRecord{Target: t.String(), Bucket: t.Bucket, Versioning: string(state)}
				return e.out.Info(rec, fmt.Sprintf("Versioning of `%s` is %s.", t, verb))
			}
			v, err := client.GetVersioning(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			text := fmt.Sprintf("Versioning of `%s` is %s.", t, versioningWord(v))
			return e.out.Result(versioningRecord{Target: t.String(), Bucket: t.Bucket, Versioning: versioningName(v)}, text)
		},
	}
}

func versioningName(v cli.Versioning) string {
	if v == cli.VersioningOff {
		return "Unversioned"
	}
	return string(v)
}

func versioningWord(v cli.Versioning) string {
	switch v {
	case cli.VersioningEnabled:
		return "enabled"
	case cli.VersioningSuspended:
		return "suspended"
	default:
		return "not enabled (unversioned)"
	}
}

func newTagCmd(e *env) *cobra.Command {
	cmd := groupCmd("tag", "Manage the tags of buckets and objects")
	cmd.AddCommand(newTagSetCmd(e), newTagListCmd(e), newTagRemoveCmd(e))
	return cmd
}

func newTagSetCmd(e *env) *cobra.Command {
	var versionID string
	cmd := &cobra.Command{
		Use:               `set ALIAS/BUCKET[/OBJECT] "KEY1=VALUE1&KEY2=VALUE2"`,
		Short:             "Replace the tags of a bucket or object",
		Example:           `  nexoctl tag set local/docs/report.pdf "project=nexo&status=final"`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			tags, err := cli.ParseTags(args[1])
			if err != nil {
				return err
			}
			t, ref, client, err := e.tagTarget(cmd, args[0], versionID)
			if err != nil {
				return err
			}
			if err := client.PutTags(ctx, ref, tags); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(tagsRecord{Target: t.String(), VersionID: versionID, Tags: tags}, fmt.Sprintf("Tags set for `%s`.", t))
		},
	}
	cmd.Flags().StringVar(&versionID, "version-id", "", "tag this version of the object")
	return cmd
}

func newTagListCmd(e *env) *cobra.Command {
	var versionID string
	cmd := &cobra.Command{
		Use:               "list ALIAS/BUCKET[/OBJECT]",
		Aliases:           []string{"ls"},
		Short:             "List the tags of a bucket or object",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, ref, client, err := e.tagTarget(cmd, args[0], versionID)
			if err != nil {
				return err
			}
			tags, err := client.GetTags(cmd.Context(), ref)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if tags == nil {
				tags = map[string]string{}
			}
			text := fmt.Sprintf("No tags for `%s`.", t)
			if len(tags) > 0 {
				lines := make([]string, 0, len(tags))
				for _, k := range cli.SortedKeys(tags) {
					lines = append(lines, fmt.Sprintf("%s: %s", k, tags[k]))
				}
				text = strings.Join(lines, "\n")
			}
			return e.out.Result(tagsRecord{Target: t.String(), VersionID: versionID, Tags: tags}, text)
		},
	}
	cmd.Flags().StringVar(&versionID, "version-id", "", "tags of this version of the object")
	return cmd
}

func newTagRemoveCmd(e *env) *cobra.Command {
	var versionID string
	cmd := &cobra.Command{
		Use:               "remove ALIAS/BUCKET[/OBJECT]",
		Aliases:           []string{"rm"},
		Short:             "Remove the tags of a bucket or object",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, ref, client, err := e.tagTarget(cmd, args[0], versionID)
			if err != nil {
				return err
			}
			if err := client.DeleteTags(cmd.Context(), ref); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(tagsRecord{Target: t.String(), VersionID: versionID, Tags: map[string]string{}}, fmt.Sprintf("Tags removed for `%s`.", t))
		},
	}
	cmd.Flags().StringVar(&versionID, "version-id", "", "untag this version of the object")
	return cmd
}

// tagTarget parses the target of the tag commands.
func (e *env) tagTarget(cmd *cobra.Command, arg, versionID string) (cli.Target, cli.ObjectRef, cli.S3Client, error) {
	ctx := cmd.Context()
	t, err := e.remoteTarget(ctx, arg, true)
	if err != nil {
		return t, cli.ObjectRef{}, nil, err
	}
	if t.Key != "" && t.IsDirLike() {
		return t, cli.ObjectRef{}, nil, cli.UsageError("%s: tags apply to a bucket or an object, not to a prefix", arg)
	}
	if versionID != "" && t.Key == "" {
		return t, cli.ObjectRef{}, nil, cli.UsageError("--version-id applies to objects")
	}
	client, err := e.client(ctx, t.Alias)
	if err != nil {
		return t, cli.ObjectRef{}, nil, err
	}
	return t, cli.ObjectRef{Bucket: t.Bucket, Key: t.Key, VersionID: versionID}, client, nil
}
