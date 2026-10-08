package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/version"
)

// cmdDeps are the process-level dependencies of the commands, injected so
// that the commands can be tested without touching the real environment.
type cmdDeps struct {
	stdout io.Writer
	stderr io.Writer
	// lookupEnv reads the environment (nil: the process environment).
	lookupEnv cli.LookupEnv
	// aliases builds the alias service; nil uses the JSON file store at
	// cli.DefaultConfigPath.
	aliases func() (cli.AliasService, error)
}

// aliasService returns the alias service of the process.
func (d cmdDeps) aliasService() (cli.AliasService, error) {
	if d.aliases != nil {
		return d.aliases()
	}
	lookup := d.lookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	path, err := cli.DefaultConfigPath(lookup)
	if err != nil {
		return nil, err
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
