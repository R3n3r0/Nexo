package commands

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
)

func newAliasCmd(e *env) *cobra.Command {
	cmd := groupCmd("alias", "Manage server aliases (saved in ~/.nexoctl/config.json)")
	cmd.Long = `An alias names a server endpoint and its credentials. The configuration file
is ~/.nexoctl/config.json (or $NEXOCTL_CONFIG_DIR/config.json, or --config-dir),
written with owner-only permissions (0600).`
	cmd.AddCommand(newAliasSetCmd(e), newAliasListCmd(e), newAliasRemoveCmd(e))
	return cmd
}

func newAliasSetCmd(e *env) *cobra.Command {
	var api, pathStyle, region, adminURL string
	var verify, noVerify bool
	cmd := &cobra.Command{
		Use:   "set NAME URL ACCESS_KEY SECRET_KEY",
		Short: "Add or update an alias",
		Long: `Add or update an alias. The credentials are checked against the server
(ListBuckets) before the alias is saved: an unknown, disabled or expired access
key or a wrong secret key exits with code 4 and saves nothing. Credentials the
server authenticates but may not list the buckets (a policy limited to some
buckets) are accepted, and the message says that the listing was denied.
--no-verify saves the alias without contacting the server (offline setup).

The admin and managed share commands talk to the Admin API of a Nexo server,
served by its console listener: by default the URL of the alias with the next
port (http://host:9000 -> http://host:9001), or the URL itself when it has no
port; --admin-url sets it explicitly.`,
		Example: `  nexoctl alias set local http://localhost:9000 admin supersecret
  nexoctl alias set later https://s3.example.com AKIA... secret --no-verify`,
		Args: exactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			svc, err := e.aliasService()
			if err != nil {
				return err
			}
			a := cli.Alias{Name: args[0], URL: args[1], AccessKey: args[2], SecretKey: args[3], API: api, Path: pathStyle, Region: region, AdminURL: adminURL}
			if err := cli.ValidateAlias(a); err != nil {
				return cli.UsageError("%v", err)
			}
			if noVerify && cmd.Flags().Changed("verify") {
				return cli.UsageError("--no-verify excludes --verify")
			}
			verify = verify && !noVerify
			// listDenied: the server authenticated the credentials and
			// refused the listing by policy; they are not fully verified.
			// adminNote: what the check of an explicit --admin-url found
			// short of a failure.
			listDenied, adminNote := false, ""
			if verify {
				client, err := e.deps.S3.Client(ctx, a, cli.ClientOptions{Debug: debugWriter(e)})
				if err != nil {
					return err
				}
				if _, err := client.ListBuckets(ctx); err != nil {
					if !cli.IsAuthorizationDenial(err) {
						return fmt.Errorf("verify alias %q at %s: %w", a.Name, a.URL, err)
					}
					listDenied = true
				}
				if adminURL != "" {
					// An explicit Admin API URL is checked too
					// (regression: an unreachable one was saved).
					if adminNote, err = verifyAdmin(ctx, e, a); err != nil {
						return fmt.Errorf("verify alias %q: Admin API at %s: %w", a.Name, adminURL, err)
					}
				}
			}
			if err := svc.Set(ctx, a); err != nil {
				return err
			}
			rec := aliasRecord{Type: typeAlias, Name: a.Name, URL: a.URL, AccessKey: a.AccessKey, SecretKey: "***", API: a.API, Path: a.Path, Region: a.Region, AdminURL: a.AdminURL}
			if rec.API == "" {
				rec.API = "s3v4"
			}
			if rec.Path == "" {
				rec.Path = "auto"
			}
			msg := fmt.Sprintf("Added alias %q for %s.", a.Name, a.URL)
			switch {
			case listDenied:
				msg = fmt.Sprintf("Added alias %q for %s (credentials accepted; listing the buckets is denied).", a.Name, a.URL)
			case verify:
				msg = fmt.Sprintf("Added alias %q for %s (credentials verified).", a.Name, a.URL)
			}
			if adminNote != "" {
				msg += " " + adminNote
			}
			return e.out.Info(rec, msg)
		},
	}
	cmd.Flags().StringVar(&api, "api", "s3v4", "signature version (s3v4)")
	cmd.Flags().StringVar(&pathStyle, "path", "auto", "URL style: auto, on (path-style), off (virtual-host)")
	cmd.Flags().StringVar(&region, "region", "", "signing region (default "+cli.DefaultRegion+")")
	cmd.Flags().BoolVar(&verify, "verify", true, "check the connection and the credentials before saving")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "save without contacting the server")
	cmd.Flags().StringVar(&adminURL, "admin-url", "", "URL of the Admin API of a Nexo server (default: the alias URL with the next port)")
	return cmd
}

func newAliasListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List aliases (secret keys are never shown)",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := e.aliasService()
			if err != nil {
				return err
			}
			list, err := svc.List(cmd.Context())
			if err != nil {
				return err
			}
			if !e.out.JSON() && len(list) > 0 {
				if err := e.out.Result(nil, fmt.Sprintf("%-16s %-40s %-20s %-5s %s", "NAME", "URL", "ACCESS KEY", "API", "PATH")); err != nil {
					return err
				}
			}
			for _, a := range list {
				rec := aliasRecord{Type: typeAlias, Name: a.Name, URL: a.URL, AccessKey: a.AccessKey, API: a.API, Path: a.Path, Region: a.Region, AdminURL: a.AdminURL}
				text := fmt.Sprintf("%s %-40s %-20s %-5s %s", e.out.Paint(cli.ColorCyan, fmt.Sprintf("%-16s", a.Name)), a.URL, a.AccessKey, a.API, a.Path)
				if err := e.out.Result(rec, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAliasRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "remove NAME",
		Aliases:           []string{"rm"},
		Short:             "Remove an alias",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := e.aliasService()
			if err != nil {
				return err
			}
			if err := svc.Remove(cmd.Context(), args[0]); err != nil {
				return err
			}
			return e.out.Info(aliasRecord{Type: typeAlias, Name: args[0]}, fmt.Sprintf("Removed alias %q.", args[0]))
		},
	}
}

// verifyAdmin checks the Admin API of a (GET /api/v1/info): an error when
// it cannot be reached or does not answer as the Admin API, a note when
// it answers but refuses the credentials the information (a user without
// admin:ServerInfo, whose admin commands may still work).
func verifyAdmin(ctx context.Context, e *env, a cli.Alias) (string, error) {
	admin, err := e.deps.Admin.Client(ctx, a, cli.ClientOptions{Debug: debugWriter(e)})
	if err != nil {
		return "", err
	}
	if _, err := admin.ServerInfo(ctx); err != nil {
		if cli.IsAuthorizationDenial(err) {
			return "The Admin API answered; the server information is denied to these credentials.", nil
		}
		return "", err
	}
	return "Admin API verified.", nil
}
