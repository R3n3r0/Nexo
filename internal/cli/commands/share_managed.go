package commands

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The managed share links (docs/09 §9.1, S-OBJ-08c): created, listed and
// revoked through the Admin API, each signed by a dedicated service
// account of the server.

// printManagedShare prints a managed link in the style of share, with its
// ID (what share revoke takes).
func (e *env) printManagedShare(t cli.Target, sh *nexoadmin.Share, method string, headers map[string]string, expiry time.Duration, command string) error {
	rec := managedRecord(t.Alias, sh)
	rec.Target, rec.Method, rec.Expire, rec.Command, rec.Headers = t.String(), method, cli.HumanDuration(expiry), command, headers
	lines := []string{
		"URL: " + t.String(),
		"Expire: " + cli.HumanDuration(expiry),
		"Share: " + sh.URL,
	}
	if rec.Player != "" {
		lines = append(lines, "Player: "+rec.Player)
	}
	if command != "" {
		lines = append(lines, "Command: "+command)
	}
	lines = append(lines, fmt.Sprintf("ID: %s (revoke with: nexoctl share revoke %s %s)", sh.ID, t.Alias, sh.ID))
	return e.out.Result(rec, strings.Join(lines, "\n"))
}

// managedRecord converts a managed share link.
func managedRecord(alias string, sh *nexoadmin.Share) managedShareRecord {
	return managedShareRecord{
		Alias: alias, ID: sh.ID, Target: cli.JoinRemote(alias, sh.Bucket, sh.Key), Owner: sh.Owner, Bucket: sh.Bucket,
		Key: sh.Key, Prefix: sh.Prefix, Permission: sh.Permission, Managed: true, URL: sh.URL, VersionID: sh.VersionID,
		ExpiresAt: sh.ExpiresAt.UTC(), CreatedAt: timePtr(sh.CreatedAt), AccessKey: sh.AccessKey,
		Stream: sh.Stream, Player: playerURL(sh),
	}
}

// playerURL returns the public player page of a stream share: the master
// playlist (the URL of the share) goes in its fragment, which never
// reaches a server log; "" for the other shares.
func playerURL(sh *nexoadmin.Share) string {
	if !sh.Stream {
		return ""
	}
	u, err := url.Parse(sh.URL)
	if err != nil || u.Host == "" {
		return ""
	}
	master := u.EscapedPath()
	if u.RawQuery != "" {
		master += "?" + u.RawQuery
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: domain.MediaPlayerPath}).String() + "#" + master
}

func newShareListCmd(e *env) *cobra.Command {
	var user string
	cmd := &cobra.Command{
		Use:     "list [flags] ALIAS[/BUCKET]",
		Aliases: []string{"ls"},
		Short:   "List the active managed share links",
		Long: `List the active managed share links, the most recent first: your own links, or
with the admin:ManageShares permission every link (--user selects the links of
one user). A BUCKET restricts the list to the links of that bucket.`,
		Example: `  nexoctl share list local
  nexoctl share list --user alice local/photos`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, err := e.remoteTarget(ctx, args[0], false)
			if err != nil {
				return err
			}
			if t.Key != "" {
				return cli.UsageError("%s: share list takes ALIAS or ALIAS/BUCKET", args[0])
			}
			client, err := e.admin(ctx, t.Alias)
			if err != nil {
				return err
			}
			shares, err := listShares(ctx, client, user, t.Bucket)
			if err != nil {
				return err
			}
			if len(shares) == 0 {
				return e.out.Note("No active share links.")
			}
			if err := e.out.Text(fmt.Sprintf("%-14s %-16s %-8s %-23s %s", "ID", "OWNER", "ACCESS", "EXPIRES", "TARGET")); err != nil {
				return err
			}
			for i := range shares {
				sh := &shares[i]
				rec := managedRecord(t.Alias, sh)
				target := rec.Target
				if sh.Prefix {
					target += "*"
				}
				text := fmt.Sprintf("%s %-16s %-8s %-23s %s", e.out.Paint(cli.ColorCyan, fmt.Sprintf("%-14s", sh.ID)), sh.Owner,
					sh.Permission, cli.FormatTime(sh.ExpiresAt), target)
				if err := e.out.Result(rec, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "list the links of this user (requires admin:ManageShares)")
	_ = cmd.RegisterFlagCompletionFunc("user", completeAdminFlag(e, userNames))
	return cmd
}

func newShareRevokeCmd(e *env) *cobra.Command {
	var (
		all  bool
		user string
	)
	cmd := &cobra.Command{
		Use:   "revoke [flags] ALIAS ID... | --all [--user USER] ALIAS",
		Short: "Revoke managed share links (immediately)",
		Long: `Revoke managed share links by ID (see "share list"): the service account that
signs a link is removed, so the link stops working at once. --all revokes every
link of the caller, or of --user (requires admin:ManageShares).`,
		Example: `  nexoctl share revoke local 3fz8k2m1q9xw
  nexoctl share revoke --all --user alice local`,
		Args:              minArgs(1),
		ValidArgsFunction: completeAdmin(e, shareIDs),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case all && len(args) > 1:
				return cli.UsageError("--all takes no share ID")
			case !all && len(args) < 2:
				return cli.UsageError("name the share IDs to revoke, or use --all")
			case !all && user != "":
				return cli.UsageError("--user needs --all")
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if all {
				n, err := client.RevokeAllShares(ctx, user)
				if err != nil {
					return fmt.Errorf("revoke the share links: %w", err)
				}
				text := fmt.Sprintf("Revoked %d of your share link(s).", n)
				if user != "" {
					text = fmt.Sprintf("Revoked %d share link(s) of user %q.", n, user)
				}
				return e.out.Info(adminActionRecord{Alias: alias, Type: kindShare, Name: user, Action: "revoked", Count: intPtr(n)}, text)
			}
			for _, id := range args[1:] {
				if err := client.RevokeShare(ctx, id); err != nil {
					return fmt.Errorf("revoke share %q: %w", id, err)
				}
				if err := e.out.Info(adminActionRecord{Alias: alias, Type: kindShare, Name: id, Action: "revoked"},
					fmt.Sprintf("Revoked share %s.", id)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "revoke every link of the caller (or of --user)")
	cmd.Flags().StringVar(&user, "user", "", "with --all: revoke the links of this user")
	_ = cmd.RegisterFlagCompletionFunc("user", completeAdminFlag(e, userNames))
	return cmd
}
