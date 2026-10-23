package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/app"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/web"
)

// assetsProvider returns the embedded dashboard files, if any.
type assetsProvider func() (fs.FS, bool)

func newServerCmd(deps cmdDeps) *cobra.Command {
	return newServerCmdWith(deps, web.Assets)
}

func newServerCmdWith(deps cmdDeps, assets assetsProvider) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "server [flags] ENDPOINT...",
		Short: "Start the S3 server and the web console",
		Long: `Start the Nexo server. The deployment mode is derived from the endpoints:

  nexo server /data                                  single node, single drive (SNSD)
  nexo server /data{1...4}                           single node, multi drive (SNMD, erasure coding)
  nexo server http://node{1...4}:9000/data{1...4}    distributed cluster
  nexo server http://node{1...4}:9000/data{1...4} \
              http://node{5...8}:9000/data{1...4}    cluster expanded with a second pool

In a cluster every node runs the same command; a node recognizes its own
endpoints by its host names or addresses and the port of --address.
--dry-run checks a configuration without starting the server nor
writing to the drives.

Root credentials come from NEXO_ROOT_USER and NEXO_ROOT_PASSWORD (or the
*_FILE variants); every option is also available as NEXO_<OPTION> (e.g.
NEXO_CONSOLE_ADDRESS). Precedence: flags > environment > --config file.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			loader := config.NewLoader(config.WithEnv(deps.lookupEnv))
			cfg, err := loader.Load(ctx, config.Input{Flags: cmd.Flags(), Args: args})
			if err != nil {
				return err
			}
			logger := newLogger(deps, cfg)
			if dryRun {
				return dryRunReport(cmd, cfg, app.WithLogger(logger))
			}
			// The log stream writes to stderr itself, so that the logger
			// subsystem switches the format at runtime (docs/07 §7.4).
			opts := []app.Option{app.WithLogger(logger), app.WithLogOutput(deps.stderr)}
			if fsys, ok := assets(); ok {
				opts = append(opts, app.WithAssetsFS(fsys))
			}
			hooks, err := testOptions(deps.lookupEnv)
			if err != nil {
				return err
			}
			opts = append(opts, hooks...)
			// The binary is resolved once, before an update replaces it:
			// the process is then replaced with the new one at the same
			// path.
			var exe string
			if deps.executable != nil && deps.exec != nil {
				if exe, err = deps.executable(); err != nil {
					logger.Warn("the path of the binary is unknown: the server cannot update itself", "err", err)
				} else {
					opts = append(opts, app.WithExecutable(exe))
				}
			}
			application, err := app.Build(ctx, cfg, opts...)
			if err != nil {
				return err
			}
			err = application.Run(ctx)
			if errors.Is(err, app.ErrExec) && exe != "" {
				logger.Info("starting the new binary", "path", exe)
				return deps.exec(exe)
			}
			return err
		},
	}
	config.BindFlags(cmd.Flags())
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"validate the configuration, topology, drive permissions, ports and certificates without starting, then exit")
	return cmd
}

func newLogger(deps cmdDeps, cfg config.Static) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.ToLower(cfg.LogFormat) == "text" {
		return slog.New(slog.NewTextHandler(deps.stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(deps.stderr, opts))
}

// dryRunReport validates the configuration (app.DryRun) and prints what
// was checked; any problem fails the command.
func dryRunReport(cmd *cobra.Command, cfg config.Static, opts ...app.Option) error {
	rep, err := app.DryRun(cmd.Context(), cfg, opts...)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "configuration OK: mode=%s s3=%s console=%s endpoints=%s\n",
		rep.Mode, cfg.Address, cfg.ConsoleAddress, strings.Join(cfg.Endpoints, " ")); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "  node: %s (%d node(s))\n", rep.NodeID, rep.Nodes)
	for _, p := range rep.Pools {
		_, _ = fmt.Fprintf(out, "  pool %d: %d erasure set(s) of %d drive(s), parity %d\n", p.Index+1, p.Sets, p.DrivesPerSet, p.Parity)
	}
	_, _ = fmt.Fprintf(out, "  local drives: %d writable, %d formatted\n", len(rep.LocalDrives), rep.Formatted)
	if rep.DeploymentID != "" {
		_, _ = fmt.Fprintf(out, "  deployment: %s\n", rep.DeploymentID)
	}
	for _, w := range rep.Warnings {
		_, _ = fmt.Fprintf(out, "  warning: %s\n", w)
	}
	return nil
}
