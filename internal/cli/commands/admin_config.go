package commands

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

func newAdminConfigCmd(e *env) *cobra.Command {
	cmd := groupCmd("config", "Read and change the dynamic configuration of the server")
	cmd.Long = `Read and change the dynamic configuration of the server, made of subsystems
(api, browser, logger, notify_webhook, region, scanner...) holding key=value
settings. Sensitive values are shown masked; setting a key to the mask keeps
the stored secret. "export" and "import" save and restore the whole
configuration as JSON.`
	cmd.AddCommand(newAdminConfigGetCmd(e), newAdminConfigSetCmd(e), newAdminConfigResetCmd(e),
		newAdminConfigExportCmd(e), newAdminConfigImportCmd(e))
	return cmd
}

func newAdminConfigGetCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "get ALIAS [SUBSYSTEM...]",
		Short:             "Print the settings of every subsystem, or of the named ones",
		Example:           "  nexoctl admin config get local region logger",
		Args:              minArgs(1),
		ValidArgsFunction: completeAdmin(e, configSubsystems),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			var subs []nexoadmin.ConfigSubsystem
			if len(args) == 1 {
				if subs, err = client.ListConfig(ctx); err != nil {
					return err
				}
			}
			for _, name := range args[1:] {
				s, err := client.GetConfig(ctx, name)
				if err != nil {
					return fmt.Errorf("config %q: %w", name, err)
				}
				subs = append(subs, *s)
			}
			for _, s := range subs {
				if err := e.printConfig(alias, &s); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// printConfig prints a subsystem as "name key=value ..." (values quoted
// when they contain spaces or quotes).
func (e *env) printConfig(alias string, s *nexoadmin.ConfigSubsystem) error {
	if s.Settings == nil {
		s.Settings = map[string]string{}
	}
	parts := []string{e.out.Paint(cli.ColorCyan, s.Name)}
	for _, k := range cli.SortedKeys(s.Settings) {
		parts = append(parts, k+"="+quoteValue(s.Settings[k]))
	}
	if len(s.Settings) == 0 {
		parts = append(parts, "(defaults)")
	}
	return e.out.Result(adminConfigRecord{Alias: alias, Subsystem: s.Name, Settings: s.Settings}, strings.Join(parts, " "))
}

// quoteValue quotes a configuration value that would not survive a split
// on spaces.
func quoteValue(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\"'=") {
		return strconv.Quote(v)
	}
	return v
}

func newAdminConfigSetCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "set ALIAS SUBSYSTEM KEY=VALUE...",
		Short:             "Change settings of a subsystem (the other keys are kept)",
		Example:           "  nexoctl admin config set local region name=eu-south-1 comment=\"Milan DC\"",
		Args:              minArgs(3),
		ValidArgsFunction: completeAdmin(e, configSubsystems),
		RunE: func(cmd *cobra.Command, args []string) error {
			settings := map[string]string{}
			for _, kv := range args[2:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || strings.TrimSpace(k) == "" {
					return cli.UsageError("invalid setting %q (use key=value)", kv)
				}
				settings[strings.TrimSpace(k)] = v
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			s, err := client.SetConfig(ctx, args[1], settings)
			if err != nil {
				return fmt.Errorf("config %q: %w", args[1], err)
			}
			if e.out.JSON() {
				return e.printConfig(alias, s)
			}
			return e.out.Info(nil, fmt.Sprintf("Updated %s: %s.", args[1], strings.Join(cli.SortedKeys(settings), ", ")))
		},
	}
}

func newAdminConfigResetCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "reset ALIAS SUBSYSTEM [KEY...]",
		Short:             "Reset a subsystem, or some of its keys, to the defaults",
		Args:              minArgs(2),
		ValidArgsFunction: completeAdmin(e, configSubsystems),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.DeleteConfig(ctx, args[1], args[2:]...); err != nil {
				return fmt.Errorf("config %q: %w", args[1], err)
			}
			what := args[1]
			if len(args) > 2 {
				what += " (" + strings.Join(args[2:], ", ") + ")"
			}
			return e.out.Info(adminActionRecord{Alias: alias, Type: kindConfig, Name: args[1], Action: "reset", Names: args[2:]},
				fmt.Sprintf("Reset %s to the defaults.", what))
		},
	}
}

func newAdminConfigExportCmd(e *env) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "export [flags] ALIAS",
		Short: "Export the whole configuration as JSON (stdout or --output)",
		Long: `Export the whole dynamic configuration as a JSON document, secrets included:
protect the file. Restore it with "nexoctl admin config import".`,
		Example:           "  nexoctl admin config export local --output nexo-config.json",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			data, err := client.ExportConfig(ctx)
			if err != nil {
				return err
			}
			if output != "" {
				if err := os.WriteFile(output, data, 0o600); err != nil {
					return err
				}
				return e.out.Info(adminConfigExportRecord{Alias: alias, File: output, Config: data},
					fmt.Sprintf("Exported the configuration of %q to %s.", alias, output))
			}
			if e.out.JSON() {
				return e.out.Result(adminConfigExportRecord{Alias: alias, Config: data}, "")
			}
			_, err = e.out.Stdout().Write(data)
			return err
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "write the document to this file (mode 0600)")
	return cmd
}

func newAdminConfigImportCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "import ALIAS FILE",
		Short:             "Import a configuration exported by \"admin config export\" (FILE, or - for stdin)",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdminPolicyFile(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			data, err := e.readDocument(args[1])
			if err != nil {
				return err
			}
			if err := checkJSON(args[1], data); err != nil {
				return err
			}
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.ImportConfig(ctx, data); err != nil {
				return fmt.Errorf("import %s: %w", args[1], err)
			}
			return e.out.Info(adminActionRecord{Alias: alias, Type: kindConfig, Action: "imported"},
				fmt.Sprintf("Imported the configuration of %q from %s.", alias, args[1]))
		},
	}
}
