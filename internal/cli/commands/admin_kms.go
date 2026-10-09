package commands

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
)

// newAdminKMSCmd returns admin kms: the KMS of the server-side encryption
// (status, keys).
func newAdminKMSCmd(e *env) *cobra.Command {
	cmd := groupCmd("kms", "Show the KMS of the server-side encryption and manage its keys")
	cmd.Long = `Show the KMS that seals the keys of the SSE-S3 and SSE-KMS objects (the builtin
KMS of NEXO_KMS_SECRET_KEY or an external KES server) and manage its keys: a
key created here is usable as SSE-KMS key ("nexoctl encrypt set sse-kms KEY",
"nexoctl cp --enc-kms").`
	key := groupCmd("key", "Create and list the keys of the KMS")
	key.AddCommand(newAdminKMSKeyCreateCmd(e), newAdminKMSKeyListCmd(e))
	cmd.AddCommand(newAdminKMSStatusCmd(e), key)
	return cmd
}

func newAdminKMSStatusCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "status ALIAS",
		Short:             "Show the state of the KMS",
		Example:           "  nexoctl admin kms status local",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			st, err := client.KMSStatus(ctx)
			if err != nil {
				return err
			}
			if !st.Configured {
				return e.out.Result(kmsStatusRecord{Alias: alias, KMSStatus: *st},
					"KMS: not configured (SSE-S3 and SSE-KMS are refused, SSE-C works)")
			}
			state := e.out.Paint(cli.ColorGreen, "online")
			if !st.Online {
				state = e.out.Paint(cli.ColorRed, "offline")
			}
			lines := []string{
				fmt.Sprintf("KMS: %s (%s)", orDash(st.Backend), state),
				"Default key: " + orDash(st.DefaultKeyID),
			}
			if st.Version != "" {
				lines = append(lines, "Version: "+st.Version)
			}
			for _, ep := range st.Endpoints {
				epState := "online"
				if !ep.Online {
					epState = "offline"
				}
				lines = append(lines, fmt.Sprintf("Endpoint: %s (%s)", ep.URL, epState))
			}
			if st.Error != "" {
				lines = append(lines, "Error: "+st.Error)
			}
			return e.out.Result(kmsStatusRecord{Alias: alias, KMSStatus: *st}, strings.Join(lines, "\n"))
		},
	}
}

func newAdminKMSKeyCreateCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "create ALIAS KEY",
		Short:             "Create a key of the KMS",
		Example:           "  nexoctl admin kms key create local app-key",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			k, err := client.CreateKMSKey(ctx, args[1])
			if err != nil {
				return fmt.Errorf("create KMS key %q: %w", args[1], err)
			}
			return e.out.Info(kmsKeyRecord{Alias: alias, KMSKey: *k}, fmt.Sprintf("Created the KMS key %q.", k.ID))
		},
	}
}

func newAdminKMSKeyListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS [PREFIX]",
		Aliases:           []string{"ls"},
		Short:             "List the keys of the KMS",
		Example:           "  nexoctl admin kms key ls local",
		Args:              func(cmd *cobra.Command, args []string) error { return argsBetween(cmd, args, 1, 2) },
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			prefix := ""
			if len(args) == 2 {
				prefix = args[1]
			}
			keys, err := client.ListKMSKeys(ctx, prefix)
			if err != nil {
				return err
			}
			if len(keys) == 0 {
				return e.out.Note("No KMS key.")
			}
			if err := e.out.Text(fmt.Sprintf("%-32s %-23s %s", "KEY", "CREATED", "DEFAULT")); err != nil {
				return err
			}
			for _, k := range keys {
				created := "-"
				if k.CreatedAt != nil {
					created = cli.FormatTime(*k.CreatedAt)
				}
				def := ""
				if k.Default {
					def = "yes"
				}
				text := strings.TrimRight(fmt.Sprintf("%-32s %-23s %s", k.ID, created, def), " ")
				if err := e.out.Result(kmsKeyRecord{Alias: alias, KMSKey: k}, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// newAdminNotifyCmd returns admin notify: the notification targets of the
// server.
func newAdminNotifyCmd(e *env) *cobra.Command {
	cmd := groupCmd("notify", "Inspect and test the notification targets")
	cmd.Long = `Inspect the notification targets of the server (the notify_* subsystems of the
configuration, see "nexoctl admin config set"): their ARN for "nexoctl event
add", their state and the events waiting in their queue on the answering node.`
	targets := groupCmd("targets", "List and test the notification targets")
	targets.AddCommand(newAdminNotifyListCmd(e), newAdminNotifyTestCmd(e))
	cmd.AddCommand(targets)
	return cmd
}

func newAdminNotifyListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS",
		Aliases:           []string{"ls"},
		Short:             "List the notification targets with their state",
		Example:           "  nexoctl admin notify targets ls local",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			targets, err := client.ListNotifyTargets(ctx)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				return e.out.Note("No notification target (configure one with \"nexoctl admin config set ALIAS notify_webhook endpoint=URL\").")
			}
			if err := e.out.Text(fmt.Sprintf("%-40s %-8s %-8s %-7s %s", "ARN", "TYPE", "STATE", "QUEUED", "ENDPOINT")); err != nil {
				return err
			}
			for _, t := range targets {
				state := e.out.Paint(cli.ColorGreen, fmt.Sprintf("%-8s", "online"))
				switch {
				case !t.Enabled:
					state = fmt.Sprintf("%-8s", "disabled")
				case !t.Online:
					state = e.out.Paint(cli.ColorRed, fmt.Sprintf("%-8s", "offline"))
				}
				text := strings.TrimRight(fmt.Sprintf("%-40s %-8s %s %-7d %s", t.ARN, t.Type, state, t.PendingEvents, t.Endpoint), " ")
				if t.LastError != "" {
					text += "\n  last error: " + t.LastError
				}
				if err := e.out.Result(notifyTargetRecord{Alias: alias, NotifyTarget: t}, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAdminNotifyTestCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "test ALIAS TARGET",
		Short: "Send a test event (s3:TestEvent) to a notification target",
		Long: `Send a test event (s3:TestEvent) to a notification target, named by its ARN or
by "ID:TYPE" (e.g. "_:webhook" for the default webhook). The command fails when
the target does not receive it.`,
		Example:           "  nexoctl admin notify targets test local arn:nexo:sqs:us-east-1:_:webhook",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			res, err := client.TestNotifyTarget(ctx, args[1])
			if err != nil {
				return fmt.Errorf("target %q: %w", args[1], err)
			}
			if !res.Delivered {
				return fmt.Errorf("target %s did not receive the test event: %s", res.Target, orDash(res.Error))
			}
			return e.out.Info(notifyTestRecord{Alias: alias, NotifyTargetTest: *res}, fmt.Sprintf("Test event delivered to %s.", res.Target))
		},
	}
}
