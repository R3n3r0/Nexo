package main

import (
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/app"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/transport/http/console"
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
			var consoleAssets console.Assets
			if fsys, ok := assets(); ok {
				consoleAssets = console.NewAssets(fsys)
			}
			application, err := app.Build(ctx, cfg, app.WithLogger(logger), app.WithAssets(consoleAssets))
			if err != nil {
				return err
			}
			if dryRun {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "configuration OK: mode=%s s3=%s console=%s endpoints=%s\n",
					application.Mode(), cfg.Address, cfg.ConsoleAddress, strings.Join(cfg.Endpoints, " "))
				return err
			}
			return application.Run(ctx)
		},
	}
	config.BindFlags(cmd.Flags())
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate the configuration and topology, then exit")
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
