package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
)

// findFlags are the flags of find.
type findFlags struct {
	name, path, larger, smaller, olderThan, newerThan, exec string
	maxDepth                                                int
}

func newFindCmd(e *env) *cobra.Command {
	var flags findFlags
	cmd := &cobra.Command{
		Use:   "find [flags] TARGET...",
		Short: "Find objects and files by name, size and age",
		Long: `Search the objects under a prefix (or the files under a local directory)
recursively and print the matches, or run a command for each one with --exec
("{}" is replaced by the path; the command is split on spaces, no shell).`,
		Example: `  nexoctl find local/logs --name "*.log" --older-than 1d
  nexoctl find local/media --larger 1GiB
  nexoctl find local/logs --name "*.gz" --exec "nexoctl rm {}"`,
		Args:              minArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.find(cmd.Context(), args, flags)
		},
	}
	f := cmd.Flags()
	f.StringVar(&flags.name, "name", "", `glob on the base name (e.g. "*.log")`)
	f.StringVar(&flags.path, "path", "", `glob on the path under the target (e.g. "2026/*/*.csv")`)
	f.StringVar(&flags.larger, "larger", "", "only objects larger than the size (e.g. 10MiB)")
	f.StringVar(&flags.smaller, "smaller", "", "only objects smaller than the size")
	f.StringVar(&flags.olderThan, "older-than", "", "only objects older than the duration (e.g. 7d)")
	f.StringVar(&flags.newerThan, "newer-than", "", "only objects newer than the duration")
	f.IntVar(&flags.maxDepth, "maxdepth", 0, "descend at most this many levels (0: unlimited)")
	f.StringVar(&flags.exec, "exec", "", `run a command for each match ("{}" is the path)`)
	return cmd
}

func (e *env) find(ctx context.Context, args []string, flags findFlags) error {
	filter := cli.FindFilter{Name: flags.name, Path: flags.path, Larger: -1, Smaller: -1, MaxDepth: flags.maxDepth, Now: e.deps.Clock.Now()}
	var err error
	if flags.larger != "" {
		if filter.Larger, err = cli.ParseSize(flags.larger); err != nil {
			return err
		}
	}
	if flags.smaller != "" {
		if filter.Smaller, err = cli.ParseSize(flags.smaller); err != nil {
			return err
		}
	}
	if flags.olderThan != "" {
		if filter.OlderThan, err = cli.ParseDuration(flags.olderThan); err != nil {
			return err
		}
	}
	if flags.newerThan != "" {
		if filter.NewerThan, err = cli.ParseDuration(flags.newerThan); err != nil {
			return err
		}
	}
	if err := filter.Validate(); err != nil {
		return err
	}
	var command []string
	if flags.exec != "" {
		command = strings.Fields(flags.exec)
	}
	targets, err := e.targets(ctx, args, false)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if t.IsRemote() && t.Bucket == "" {
			return cli.UsageError("%s: find needs a bucket", t)
		}
		for en, err := range cli.Walk(ctx, e.resolver(), t) {
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if !filter.Match(en) {
				continue
			}
			full, err := cli.JoinTarget(t, en.Rel)
			if err != nil {
				return err
			}
			if command != nil {
				if err := e.runExec(ctx, command, full.String()); err != nil {
					return err
				}
				continue
			}
			rec := entryRecord{Type: typeFile, Target: full.String(), Key: full.Key, Name: en.Rel, Size: en.Size, LastModified: timePtr(en.ModTime), ETag: en.ETag}
			if err := e.out.Result(rec, full.String()); err != nil {
				return err
			}
		}
	}
	return nil
}

// runExec runs the --exec command of find for one path.
func (e *env) runExec(ctx context.Context, command []string, path string) error {
	args := make([]string, 0, len(command)-1)
	for _, a := range command[1:] {
		args = append(args, strings.ReplaceAll(a, "{}", path))
	}
	name := strings.ReplaceAll(command[0], "{}", path)
	if err := e.deps.Exec(ctx, name, args, e.deps.Stdout, e.deps.Stderr); err != nil {
		return fmt.Errorf("--exec %s: %w", name, err)
	}
	return nil
}

func newDuCmd(e *env) *cobra.Command {
	var depth int
	cmd := &cobra.Command{
		Use:   "du [flags] TARGET...",
		Short: "Summarize the disk usage of prefixes and directories",
		Example: `  nexoctl du local/photos
  nexoctl du --depth 1 local/photos`,
		Args:              minArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if depth < 0 {
				return cli.UsageError("--depth must not be negative")
			}
			targets, err := e.targets(ctx, args, false)
			if err != nil {
				return err
			}
			for _, t := range targets {
				if t.IsRemote() && t.Bucket == "" {
					return cli.UsageError("%s: du needs a bucket", t)
				}
				usage := cli.NewDiskUsage(depth)
				for en, err := range cli.Walk(ctx, e.resolver(), t) {
					if err != nil {
						return fmt.Errorf("%s: %w", t, err)
					}
					usage.Add(en)
				}
				for _, row := range usage.Rows() {
					path := t.String()
					if row.Prefix != "" {
						joined, err := cli.JoinTarget(t, row.Prefix)
						if err != nil {
							return err
						}
						path = joined.String()
					}
					rec := usageRecord{Target: path, Prefix: row.Prefix, Size: row.Size, Objects: row.Objects}
					text := fmt.Sprintf("%8s %7d %-7s %s", cli.HumanSize(row.Size), row.Objects, plural(int(row.Objects), "object", "objects"), path)
					if err := e.out.Result(rec, text); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&depth, "depth", "d", 0, "also print the directories down to this depth")
	return cmd
}

func newTreeCmd(e *env) *cobra.Command {
	var dirsOnly bool
	cmd := &cobra.Command{
		Use:               "tree [flags] TARGET...",
		Short:             "Show the objects and prefixes as a tree",
		Example:           "  nexoctl tree -d local/photos",
		Args:              minArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			targets, err := e.targets(ctx, args, false)
			if err != nil {
				return err
			}
			for _, t := range targets {
				if t.IsRemote() && t.Bucket == "" {
					return cli.UsageError("%s: tree needs a bucket", t)
				}
				var rels []string
				for en, err := range cli.Walk(ctx, e.resolver(), t) {
					if err != nil {
						return fmt.Errorf("%s: %w", t, err)
					}
					rels = append(rels, en.Rel)
				}
				if err := e.out.Text(e.out.Paint(cli.ColorBlue, t.String())); err != nil {
					return err
				}
				for _, n := range cli.BuildTree(rels, dirsOnly) {
					typ, name := typeFile, n.Name
					if n.Dir {
						typ, name = typeFolder, e.out.Paint(cli.ColorBlue, n.Name)
					}
					full, err := cli.JoinTarget(t, n.Path)
					if err != nil {
						return err
					}
					rec := treeRecord{Target: full.String(), Path: n.Path, Type: typ, Depth: n.Depth}
					if err := e.out.Result(rec, n.Indent+name); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&dirsOnly, "dirs-only", "d", false, "show only the directories")
	return cmd
}

func newDiffCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "diff FIRST SECOND",
		Short: "List the differences between two directories or prefixes",
		Long: `Compare two directories or prefixes by name, size and ETag:
  <  only in FIRST
  >  only in SECOND
  !  different size or content`,
		Example:           "  nexoctl diff ./site local/website",
		Args:              exactArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			targets, err := e.targets(ctx, args, false)
			if err != nil {
				return err
			}
			first, second := targets[0], targets[1]
			for _, t := range targets {
				if t.IsRemote() && t.Bucket == "" {
					return cli.UsageError("%s: diff needs a bucket", t)
				}
			}
			a, err := cli.CollectEntries(cli.Walk(ctx, e.resolver(), first))
			if err != nil {
				return fmt.Errorf("%s: %w", first, err)
			}
			b, err := cli.CollectEntries(cli.Walk(ctx, e.resolver(), second))
			if err != nil {
				return fmt.Errorf("%s: %w", second, err)
			}
			for _, d := range cli.Diff(a, b) {
				rec := diffRecord{Diff: string(d.Kind), Path: d.Rel}
				for _, side := range []struct {
					entry *cli.Entry
					dir   cli.Target
					dst   *string
				}{{d.First, first, &rec.First}, {d.Second, second, &rec.Second}} {
					if side.entry == nil {
						continue
					}
					full, err := cli.JoinTarget(side.dir, d.Rel)
					if err != nil {
						return err
					}
					*side.dst = full.String()
				}
				var text string
				switch d.Kind {
				case cli.DiffOnlyFirst:
					text = e.out.Paint(cli.ColorRed, "< "+rec.First)
				case cli.DiffOnlySecond:
					text = e.out.Paint(cli.ColorGreen, "> "+rec.Second)
				case cli.DiffSize:
					text = e.out.Paint(cli.ColorYellow, fmt.Sprintf("! %s (size %s != %s)", rec.First, cli.HumanSize(d.First.Size), cli.HumanSize(d.Second.Size)))
				default:
					text = e.out.Paint(cli.ColorYellow, fmt.Sprintf("! %s (content differs)", rec.First))
				}
				if err := e.out.Result(rec, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
