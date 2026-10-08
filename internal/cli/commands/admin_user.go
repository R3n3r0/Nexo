package commands

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

func newAdminUserCmd(e *env) *cobra.Command {
	cmd := groupCmd("user", "Manage the IAM users")
	cmd.AddCommand(
		newAdminUserAddCmd(e), newAdminUserRemoveCmd(e),
		newAdminUserStatusCmd(e, "enable", nexoadmin.StatusEnabled, "enabled"),
		newAdminUserStatusCmd(e, "disable", nexoadmin.StatusDisabled, "disabled"),
		newAdminUserListCmd(e), newAdminUserInfoCmd(e), newAdminUserPolicyCmd(e),
	)
	return cmd
}

func newAdminUserAddCmd(e *env) *cobra.Command {
	var policies, groups []string
	var disabled bool
	cmd := &cobra.Command{
		Use:   "add [flags] ALIAS ACCESS_KEY SECRET_KEY",
		Short: "Create a user",
		Long: `Create a user with its credentials. A user without policies can do nothing until
a policy is attached to it or to one of its groups.`,
		Example:           "  nexoctl admin user add local alice 'S3cr3t-Passw0rd' --policy readwrite",
		Args:              exactArgs(3),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			in := nexoadmin.CreateUserRequest{AccessKey: args[1], SecretKey: args[2], PolicyNames: splitList(policies), Groups: splitList(groups)}
			if disabled {
				in.Status = nexoadmin.StatusDisabled
			}
			u, err := client.CreateUser(ctx, in)
			if err != nil {
				return fmt.Errorf("add user %q: %w", args[1], err)
			}
			return e.out.Info(userRecord(alias, u), fmt.Sprintf("Added user %q (%s).", u.AccessKey, u.Status))
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&policies, "policy", nil, "policies to attach (repeatable or comma-separated)")
	f.StringSliceVar(&groups, "group", nil, "groups to join (repeatable or comma-separated)")
	f.BoolVar(&disabled, "disabled", false, "create the user disabled")
	_ = cmd.RegisterFlagCompletionFunc("policy", completeAdminFlag(e, policyNames))
	_ = cmd.RegisterFlagCompletionFunc("group", completeAdminFlag(e, groupNames))
	return cmd
}

func newAdminUserRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "remove ALIAS ACCESS_KEY...",
		Aliases:           []string{"rm"},
		Short:             "Remove users (with their access keys)",
		Args:              minArgs(2),
		ValidArgsFunction: completeAdmin(e, userNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			for _, name := range args[1:] {
				if err := client.DeleteUser(ctx, name); err != nil {
					return fmt.Errorf("remove user %q: %w", name, err)
				}
				if err := e.out.Info(adminActionRecord{Alias: alias, Type: kindUser, Name: name, Action: "removed"},
					fmt.Sprintf("Removed user %q.", name)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAdminUserStatusCmd(e *env, verb, status, done string) *cobra.Command {
	return &cobra.Command{
		Use:               verb + " ALIAS ACCESS_KEY...",
		Short:             capitalize(verb) + " users (a disabled user cannot sign any request)",
		Args:              minArgs(2),
		ValidArgsFunction: completeAdmin(e, userNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			for _, name := range args[1:] {
				st := status
				if _, err := client.UpdateUser(ctx, name, nexoadmin.UpdateUserRequest{Status: &st}); err != nil {
					return fmt.Errorf("%s user %q: %w", verb, name, err)
				}
				if err := e.out.Info(adminActionRecord{Alias: alias, Type: kindUser, Name: name, Action: done},
					fmt.Sprintf("User %q is %s.", name, done)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAdminUserListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS",
		Aliases:           []string{"ls"},
		Short:             "List the users",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			users, err := listUsers(ctx, client)
			if err != nil {
				return err
			}
			if len(users) > 0 {
				if err := e.out.Text(fmt.Sprintf("%-24s %-9s %-30s %s", "ACCESS KEY", "STATUS", "POLICIES", "GROUPS")); err != nil {
					return err
				}
			}
			for i := range users {
				u := &users[i]
				text := fmt.Sprintf("%s %-9s %-30s %s", e.out.Paint(cli.ColorCyan, fmt.Sprintf("%-24s", u.AccessKey)), statusText(e, u.Status),
					joinOrDash(u.PolicyNames), joinOrDash(u.Groups))
				if err := e.out.Result(userRecord(alias, u), text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAdminUserInfoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "info ALIAS ACCESS_KEY",
		Short:             "Show a user",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdmin(e, userNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			u, err := client.GetUser(ctx, args[1])
			if err != nil {
				return fmt.Errorf("user %q: %w", args[1], err)
			}
			lines := []string{
				"Access Key: " + u.AccessKey,
				"Status: " + u.Status,
				"Policies: " + joinOrDash(u.PolicyNames),
				"Groups: " + joinOrDash(u.Groups),
				"Created: " + cli.FormatTime(u.CreatedAt),
				"Updated: " + cli.FormatTime(u.UpdatedAt),
			}
			return e.out.Result(userRecord(alias, u), strings.Join(lines, "\n"))
		},
	}
}

// adminUserPolicyRecord is the policies of a user: attached directly and
// through its groups.
type adminUserPolicyRecord struct {
	Alias     string              `json:"alias"`
	AccessKey string              `json:"accessKey"`
	Direct    []string            `json:"direct"`
	Groups    map[string][]string `json:"groups"`
	Effective []string            `json:"effective"`
}

func newAdminUserPolicyCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "policy ALIAS ACCESS_KEY",
		Short: "Show the policies of a user, direct and inherited from its groups",
		Long: `Show the policies that apply to a user: those attached to it and those of its
enabled groups. Attach and detach them with "nexoctl admin policy attach|detach".`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdmin(e, userNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			u, err := client.GetUser(ctx, args[1])
			if err != nil {
				return fmt.Errorf("user %q: %w", args[1], err)
			}
			rec := adminUserPolicyRecord{Alias: alias, AccessKey: u.AccessKey, Direct: names(u.PolicyNames), Groups: map[string][]string{}}
			seen := map[string]bool{}
			for _, p := range u.PolicyNames {
				seen[p] = true
			}
			lines := []string{fmt.Sprintf("User %s: %s", u.AccessKey, joinOrDash(u.PolicyNames))}
			for _, gname := range u.Groups {
				g, err := client.GetGroup(ctx, gname)
				if err != nil {
					return fmt.Errorf("group %q of user %q: %w", gname, u.AccessKey, err)
				}
				line := fmt.Sprintf("Group %s: %s", g.Name, joinOrDash(g.PolicyNames))
				if g.Status == nexoadmin.StatusDisabled {
					lines = append(lines, line+" (disabled, not applied)")
					rec.Groups[g.Name] = []string{}
					continue
				}
				lines = append(lines, line)
				rec.Groups[g.Name] = names(g.PolicyNames)
				for _, p := range g.PolicyNames {
					seen[p] = true
				}
			}
			rec.Effective = cli.SortedKeys(seen)
			lines = append(lines, "Effective: "+joinOrDash(rec.Effective))
			return e.out.Result(rec, strings.Join(lines, "\n"))
		},
	}
}

// statusText colors a credential status.
func statusText(e *env, status string) string {
	padded := fmt.Sprintf("%-9s", status)
	if status == nexoadmin.StatusDisabled {
		return e.out.Paint(cli.ColorYellow, padded)
	}
	return padded
}
