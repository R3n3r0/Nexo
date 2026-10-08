package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
)

func newAliasCmd(deps cmdDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alias",
		Short: "Manage server aliases (saved in ~/.nexoctl/config.json)",
	}
	cmd.AddCommand(newAliasSetCmd(deps), newAliasListCmd(deps), newAliasRemoveCmd(deps))
	return cmd
}

func newAliasSetCmd(deps cmdDeps) *cobra.Command {
	var api, pathStyle string
	cmd := &cobra.Command{
		Use:     "set NAME URL ACCESS_KEY SECRET_KEY",
		Short:   "Add or update an alias",
		Example: "  nexoctl alias set local http://localhost:9000 admin supersecret",
		Args:    cobra.ExactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := deps.aliasService()
			if err != nil {
				return err
			}
			a := cli.Alias{Name: args[0], URL: args[1], AccessKey: args[2], SecretKey: args[3], API: api, Path: pathStyle}
			if err := svc.Set(cmd.Context(), a); err != nil {
				return err
			}
			if jsonOutput(cmd) {
				a.SecretKey = "***"
				return writeJSON(cmd.OutOrStdout(), a)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Added alias %q for %s.\n", a.Name, a.URL)
			return err
		},
	}
	cmd.Flags().StringVar(&api, "api", "s3v4", "signature version (s3v4)")
	cmd.Flags().StringVar(&pathStyle, "path", "auto", "URL style: auto, on (path-style), off (virtual-host)")
	return cmd
}

// aliasView is the redacted alias shown by list.
type aliasView struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	AccessKey string `json:"accessKey"`
	API       string `json:"api"`
	Path      string `json:"path"`
}

func newAliasListCmd(deps cmdDeps) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List aliases",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := deps.aliasService()
			if err != nil {
				return err
			}
			list, err := svc.List(cmd.Context())
			if err != nil {
				return err
			}
			views := make([]aliasView, 0, len(list))
			for _, a := range list {
				views = append(views, aliasView{Name: a.Name, URL: a.URL, AccessKey: a.AccessKey, API: a.API, Path: a.Path})
			}
			if jsonOutput(cmd) {
				return writeJSON(cmd.OutOrStdout(), views)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			if _, err := fmt.Fprintln(tw, "NAME\tURL\tACCESS KEY\tAPI\tPATH"); err != nil {
				return err
			}
			for _, v := range views {
				if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Name, v.URL, v.AccessKey, v.API, v.Path); err != nil {
					return err
				}
			}
			return tw.Flush()
		},
	}
}

func newAliasRemoveCmd(deps cmdDeps) *cobra.Command {
	return &cobra.Command{
		Use:     "remove NAME",
		Aliases: []string{"rm"},
		Short:   "Remove an alias",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := deps.aliasService()
			if err != nil {
				return err
			}
			if err := svc.Remove(cmd.Context(), args[0]); err != nil {
				return err
			}
			if jsonOutput(cmd) {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"removed": args[0]})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed alias %q.\n", args[0])
			return err
		},
	}
}
