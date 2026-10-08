package commands

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

func newAdminGroupCmd(e *env) *cobra.Command {
	cmd := groupCmd("group", "Manage the IAM groups and their members")
	cmd.AddCommand(
		newAdminGroupAddCmd(e), newAdminGroupRemoveCmd(e), newAdminGroupListCmd(e), newAdminGroupInfoCmd(e),
		newAdminGroupStatusCmd(e, "enable", nexoadmin.StatusEnabled, "enabled"),
		newAdminGroupStatusCmd(e, "disable", nexoadmin.StatusDisabled, "disabled"),
		newAdminGroupMembersCmd(e),
	)
	return cmd
}

func newAdminGroupAddCmd(e *env) *cobra.Command {
	var policies []string
	cmd := &cobra.Command{
		Use:   "add [flags] ALIAS GROUP [MEMBER...]",
		Short: "Create a group or add members to it",
		Long: `Add users to a group, creating the group when it does not exist. --policy attaches
policies to the group.`,
		Example:           "  nexoctl admin group add local devs alice bob --policy readwrite",
		Args:              minArgs(2),
		ValidArgsFunction: completeAdminGroupArgs(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			name, members, pols := args[1], args[2:], splitList(policies)
			g, err := client.GetGroup(ctx, name)
			switch {
			case errors.Is(err, domain.ErrGroupNotFound):
				if g, err = client.CreateGroup(ctx, nexoadmin.CreateGroupRequest{Name: name, Members: members, PolicyNames: pols}); err != nil {
					return fmt.Errorf("create group %q: %w", name, err)
				}
				return e.out.Info(groupRecord(alias, g), fmt.Sprintf("Created group %q with %d member(s).", name, len(g.Members)))
			case err != nil:
				return fmt.Errorf("group %q: %w", name, err)
			}
			if len(members) == 0 && len(pols) == 0 {
				return cli.UsageError("group %q exists: name the members to add (or --policy)", name)
			}
			if len(members) > 0 {
				if g, err = client.UpdateGroup(ctx, name, nexoadmin.UpdateGroupRequest{AddMembers: members}); err != nil {
					return fmt.Errorf("add members to group %q: %w", name, err)
				}
			}
			if len(pols) > 0 {
				if err := client.AttachPolicy(ctx, nexoadmin.AttachRequest{PolicyNames: pols, Group: name}); err != nil {
					return fmt.Errorf("attach policies to group %q: %w", name, err)
				}
				if g, err = client.GetGroup(ctx, name); err != nil {
					return fmt.Errorf("group %q: %w", name, err)
				}
			}
			return e.out.Info(groupRecord(alias, g), fmt.Sprintf("Updated group %q: %d member(s), policies %s.", name, len(g.Members), joinOrDash(g.PolicyNames)))
		},
	}
	cmd.Flags().StringSliceVar(&policies, "policy", nil, "policies to attach to the group (repeatable or comma-separated)")
	_ = cmd.RegisterFlagCompletionFunc("policy", completeAdminFlag(e, policyNames))
	return cmd
}

func newAdminGroupRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "remove ALIAS GROUP [MEMBER...]",
		Aliases: []string{"rm"},
		Short:   "Remove members from a group, or the group itself",
		Long: `Remove the named members from a group; without members, remove the group
itself (its members lose the policies of the group).`,
		Args:              minArgs(2),
		ValidArgsFunction: completeAdminGroupArgs(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			name, members := args[1], args[2:]
			if len(members) == 0 {
				if err := client.DeleteGroup(ctx, name); err != nil {
					return fmt.Errorf("remove group %q: %w", name, err)
				}
				return e.out.Info(adminActionRecord{Alias: alias, Type: kindGroup, Name: name, Action: "removed"},
					fmt.Sprintf("Removed group %q.", name))
			}
			g, err := client.UpdateGroup(ctx, name, nexoadmin.UpdateGroupRequest{RemoveMembers: members})
			if err != nil {
				return fmt.Errorf("remove members from group %q: %w", name, err)
			}
			return e.out.Info(groupRecord(alias, g), fmt.Sprintf("Removed %d member(s) from group %q.", len(members), name))
		},
	}
}

func newAdminGroupListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS",
		Aliases:           []string{"ls"},
		Short:             "List the groups",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			groups, err := listGroups(ctx, client)
			if err != nil {
				return err
			}
			if len(groups) > 0 {
				if err := e.out.Text(fmt.Sprintf("%-24s %-9s %7s  %s", "GROUP", "STATUS", "MEMBERS", "POLICIES")); err != nil {
					return err
				}
			}
			for i := range groups {
				g := &groups[i]
				text := fmt.Sprintf("%s %s %7d  %s", e.out.Paint(cli.ColorCyan, fmt.Sprintf("%-24s", g.Name)), statusText(e, g.Status),
					len(g.Members), joinOrDash(g.PolicyNames))
				if err := e.out.Result(groupRecord(alias, g), text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAdminGroupInfoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "info ALIAS GROUP",
		Short:             "Show a group",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdminGroupArgs(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			g, err := client.GetGroup(ctx, args[1])
			if err != nil {
				return fmt.Errorf("group %q: %w", args[1], err)
			}
			lines := []string{
				"Group: " + g.Name,
				"Status: " + g.Status,
				"Policies: " + joinOrDash(g.PolicyNames),
				"Members: " + joinOrDash(g.Members),
				"Created: " + cli.FormatTime(g.CreatedAt),
				"Updated: " + cli.FormatTime(g.UpdatedAt),
			}
			return e.out.Result(groupRecord(alias, g), strings.Join(lines, "\n"))
		},
	}
}

func newAdminGroupStatusCmd(e *env, verb, status, done string) *cobra.Command {
	return &cobra.Command{
		Use:               verb + " ALIAS GROUP...",
		Short:             capitalize(verb) + " groups (the policies of a disabled group do not apply)",
		Args:              minArgs(2),
		ValidArgsFunction: completeAdmin(e, groupNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			for _, name := range args[1:] {
				st := status
				if _, err := client.UpdateGroup(ctx, name, nexoadmin.UpdateGroupRequest{Status: &st}); err != nil {
					return fmt.Errorf("%s group %q: %w", verb, name, err)
				}
				if err := e.out.Info(adminActionRecord{Alias: alias, Type: kindGroup, Name: name, Action: done},
					fmt.Sprintf("Group %q is %s.", name, done)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// adminMemberRecord is a member of a group.
type adminMemberRecord struct {
	Alias  string `json:"alias"`
	Group  string `json:"group"`
	Member string `json:"member"`
}

func newAdminGroupMembersCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "members ALIAS GROUP",
		Short:             "List the members of a group",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdminGroupArgs(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			g, err := client.GetGroup(ctx, args[1])
			if err != nil {
				return fmt.Errorf("group %q: %w", args[1], err)
			}
			if len(g.Members) == 0 {
				return e.out.Note(fmt.Sprintf("Group %q has no members.", g.Name))
			}
			for _, m := range g.Members {
				if err := e.out.Result(adminMemberRecord{Alias: alias, Group: g.Name, Member: m}, m); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// completeAdminGroupArgs completes ALIAS GROUP [USER...].
func completeAdminGroupArgs(e *env) cobra.CompletionFunc {
	groups := completeAdmin(e, groupNames)
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) < 2 {
			return groups(cmd, args, toComplete)
		}
		return completeAdminNames(e, cmd, args[0], toComplete, userNames)
	}
}
