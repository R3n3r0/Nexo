// Package commands holds the cobra commands of nexoctl (docs/02 §2.3). The
// commands only decode their arguments and flags, call the building blocks
// of internal/cli (S3Client, Transfer, PlanCopy, PlanMirror, ...) and
// encode the results with a cli.Printer: tables for humans, JSON Lines
// with --json. Their dependencies are injected through Deps so that the
// tests replace the S3 client and the alias store with the generated mocks
// (docs/12 §12.5).
package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/version"
)

// Run is the entry point of the nexoctl binary: Main with a context that
// the first SIGINT or SIGTERM cancels (the running command stops and
// aborts its multipart uploads); a second signal kills the process.
func Run(deps Deps, args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // restore the default behavior: a second Ctrl-C kills
	}()
	return Main(ctx, deps, args)
}

// Main runs nexoctl with args (without the program name) and returns the
// exit code of docs/06 TCL-U-04: 0 success, 1 error, 2 usage error, 3 not
// found, 4 access denied (130 interrupted). Errors are printed on stderr
// (a JSON line with --json).
func Main(ctx context.Context, deps Deps, args []string) int {
	deps = deps.withDefaults()
	e := newEnv(deps)
	root := newRootCmd(e)
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return cli.ExitOK
	}
	if e.out == nil && cmd != nil {
		// The command failed before PersistentPreRunE: the global flags
		// parsed so far still select the output format.
		_ = e.init(cmd)
	}
	if !e.started && cli.ExitCode(err) != cli.ExitUsage {
		// Flag parsing, argument validation and unknown commands fail
		// before the command runs: usage errors.
		err = cli.UsageError("%v", err)
	}
	e.printer().Error(err)
	if cli.ExitCode(err) == cli.ExitUsage && !e.printer().JSON() && cmd != nil {
		_, _ = fmt.Fprintf(deps.Stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
	}
	return cli.ExitCode(err)
}

// NewRootCmd returns the nexoctl command tree over deps (for the tests and
// for the generation of documentation and completions).
func NewRootCmd(deps Deps) *cobra.Command {
	return newRootCmd(newEnv(deps.withDefaults()))
}

func newRootCmd(e *env) *cobra.Command {
	root := &cobra.Command{
		Use:   "nexoctl",
		Short: "Nexo command-line client",
		Long: `nexoctl manages Nexo servers and any S3-compatible endpoint: aliases,
buckets, objects, sharing, anonymous access and administration.

Remote locations are written ALIAS[/BUCKET[/PREFIX]] (see "nexoctl alias");
anything else is a local path.`,
		Version:           version.Get().Version,
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			e.started = true
			return e.init(cmd)
		},
	}
	root.SetVersionTemplate("nexoctl {{.Version}}\n")
	root.SetOut(e.deps.Stdout)
	root.SetErr(e.deps.Stderr)
	root.SetIn(e.deps.Stdin)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return cli.UsageError("%v", err)
	})
	pf := root.PersistentFlags()
	pf.Bool(flagJSON, false, "print JSON Lines (one object per line with a \"status\" member)")
	pf.BoolP(flagQuiet, "q", false, "hide confirmations and progress (results and errors are still printed)")
	pf.Bool(flagNoColor, false, "disable colors (also with the NO_COLOR environment variable)")
	pf.Bool(flagDebug, false, "dump the HTTP requests and responses on stderr, with the secrets redacted")
	pf.String(flagConfigDir, "", "configuration directory (default $"+cli.EnvConfigDir+" or ~/"+cli.ConfigDirName+")")

	root.AddGroup(
		&cobra.Group{ID: groupObjects, Title: "Buckets and objects:"},
		&cobra.Group{ID: groupSharing, Title: "Sharing and access:"},
		&cobra.Group{ID: groupSettings, Title: "Bucket and object settings:"},
		&cobra.Group{ID: groupAdmin, Title: "Administration:"},
		&cobra.Group{ID: groupConfig, Title: "Configuration:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add(groupObjects,
		newLsCmd(e), newMbCmd(e), newRbCmd(e), newCpCmd(e), newMvCmd(e), newPutCmd(e), newRmCmd(e),
		newCatCmd(e), newHeadCmd(e), newPipeCmd(e), newStatCmd(e), newMirrorCmd(e),
		newFindCmd(e), newDuCmd(e), newTreeCmd(e), newDiffCmd(e),
	)
	add(groupSharing, newShareCmd(e), newPresignCmd(e), newAnonymousCmd(e), newCORSCmd(e), newAccessCmd(e), newDistCmd(e), newCacheCmd(e))
	add(groupSettings, newVersionCmd(e), newTagCmd(e), newRetentionCmd(e), newLegalHoldCmd(e),
		newILMCmd(e), newEventCmd(e), newQuotaCmd(e), newEncryptCmd(e))
	add(groupAdmin, newAdminCmd(e))
	add(groupConfig, newAliasCmd(e))
	root.AddCommand(newGenDocsCmd(e)) // hidden: make docs
	root.SetCompletionCommandGroupID(groupConfig)
	root.SetHelpCommandGroupID(groupConfig)
	return root
}

// Command groups of the help.
const (
	groupObjects  = "objects"
	groupSharing  = "sharing"
	groupSettings = "settings"
	groupAdmin    = "admin"
	groupConfig   = "config"
)

// Global flags.
const (
	flagJSON      = "json"
	flagQuiet     = "quiet"
	flagNoColor   = "no-color"
	flagDebug     = "debug"
	flagConfigDir = "config-dir"
)

// --- argument validators (usage errors, exit code 2) -----------------------------

func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return cli.UsageError("%s takes %d argument(s), got %d", cmd.CommandPath(), n, len(args))
		}
		return nil
	}
}

func minArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < n {
			return cli.UsageError("%s takes at least %d argument(s), got %d", cmd.CommandPath(), n, len(args))
		}
		return nil
	}
}

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return cli.UsageError("%s takes no arguments, got %q", cmd.CommandPath(), strings.Join(args, " "))
	}
	return nil
}

// groupCmd returns a command that only groups subcommands: without a
// subcommand it prints its help, with an unknown one it is a usage error.
func groupCmd(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return cli.UsageError("unknown command %q for %q", args[0], cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
}

// debugWriter returns the destination of the HTTP dumps of --debug
// (stderr), or nil without --debug.
func debugWriter(e *env) io.Writer {
	if !e.debug {
		return nil
	}
	return e.deps.Stderr
}

// batchError reports the failures of a batch (nil when none failed): the
// first error is the cause, so its class sets the exit code.
func batchError(what string, failed, total int, first error) error {
	if failed == 0 {
		return nil
	}
	if first == nil {
		first = errors.New("unknown failure")
	}
	return fmt.Errorf("%d of %d %s failed: %w", failed, total, what, first)
}
