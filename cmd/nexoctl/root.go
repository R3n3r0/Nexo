package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/version"
)

// cmdDeps are the process-level dependencies of the commands.
type cmdDeps struct {
	stdout io.Writer
	stderr io.Writer
	// configPath overrides the configuration file (tests); empty uses
	// cli.DefaultConfigPath.
	configPath string
}

func (d cmdDeps) aliases() (cli.AliasService, error) {
	path := d.configPath
	if path == "" {
		var err error
		if path, err = cli.DefaultConfigPath(); err != nil {
			return nil, err
		}
	}
	return cli.NewAliasService(cli.AliasDeps{Store: cli.NewFileStore(path)}), nil
}

func newRootCmd(deps cmdDeps) *cobra.Command {
	root := &cobra.Command{
		Use:           "nexoctl",
		Short:         "Nexo command-line client",
		Long:          "nexoctl manages Nexo servers: aliases, buckets, objects and administration.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(deps.stdout)
	root.SetErr(deps.stderr)
	root.PersistentFlags().Bool("json", false, "print machine-readable JSON output")
	root.AddCommand(newVersionCmd(), newAliasCmd(deps))
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := version.Get()
			if jsonOutput(cmd) {
				return writeJSON(cmd.OutOrStdout(), info)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "nexoctl %s\n", info)
			return err
		},
	}
}

func jsonOutput(cmd *cobra.Command) bool {
	v, err := cmd.Flags().GetBool("json")
	return err == nil && v
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
