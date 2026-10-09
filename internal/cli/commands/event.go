package commands

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

// defaultEvents are the event types of "event add" without --event.
const defaultEvents = "put,delete,get"

func newEventCmd(e *env) *cobra.Command {
	cmd := groupCmd("event", "Manage the event notifications of buckets")
	cmd.Long = `Manage the event subscriptions of buckets: the object events (put, delete, get,
or S3 event types such as s3:ObjectCreated:Put) sent to a notification target,
optionally filtered by key prefix and suffix.

The targets are configured on the server (notify_webhook, notify_kafka, ... with
"nexoctl admin config set"); "nexoctl admin notify targets ls" prints their ARN.`
	cmd.AddCommand(newEventAddCmd(e), newEventRemoveCmd(e), newEventListCmd(e))
	return cmd
}

func newEventAddCmd(e *env) *cobra.Command {
	var events, prefix, suffix, id string
	var ignoreExisting bool
	cmd := &cobra.Command{
		Use:   "add [flags] ALIAS/BUCKET ARN",
		Short: "Subscribe a notification target to the events of a bucket",
		Example: `  nexoctl event add local/photos arn:nexo:sqs:us-east-1:_:webhook --event put --suffix .jpg
  nexoctl event add local/logs arn:nexo:sqs:us-east-1:audit:kafka --event put,delete --prefix app/`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			types, err := cli.ParseEventTypes(events)
			if err != nil {
				return err
			}
			arn := strings.TrimSpace(args[1])
			if err := cli.ValidateARN(arn); err != nil {
				return err
			}
			ctx := cmd.Context()
			t, client, err := e.ilmBucket(ctx, args[0], "event")
			if err != nil {
				return err
			}
			cfg, err := client.GetNotifications(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			sub := cli.EventSubscription{ID: id, ARN: arn, Events: types, Prefix: prefix, Suffix: suffix}
			for _, s := range cfg.Subscriptions {
				if s.ARN == arn && s.Prefix == prefix && s.Suffix == suffix && cli.SameEvents(s.Events, types) {
					if ignoreExisting {
						return e.out.Info(eventRecord{Target: t.String(), Bucket: t.Bucket, EventSubscription: s},
							fmt.Sprintf("`%s` is already subscribed to %s.", arn, t))
					}
					return fmt.Errorf("%s: the subscription of %s already exists: %w", t, arn, domain.ErrConflict)
				}
				if id != "" && s.ID == id {
					return cli.UsageError("%s: a subscription with ID %q already exists", t, id)
				}
			}
			cfg.Subscriptions = append(cfg.Subscriptions, sub)
			if err := client.PutNotifications(ctx, t.Bucket, *cfg); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(eventRecord{Target: t.String(), Bucket: t.Bucket, EventSubscription: sub},
				fmt.Sprintf("Subscribed `%s` to the %s events of `%s`.", arn, strings.Join(types, ","), t))
		},
	}
	f := cmd.Flags()
	f.StringVar(&events, "event", defaultEvents, "events: put, delete, get, replica, ilm or S3 event types (comma-separated)")
	f.StringVar(&prefix, "prefix", "", "only the objects whose key starts with the prefix")
	f.StringVar(&suffix, "suffix", "", "only the objects whose key ends with the suffix")
	f.StringVar(&id, "id", "", "ID of the subscription (default: assigned by the server)")
	f.BoolVarP(&ignoreExisting, "ignore-existing", "p", false, "do not fail when the subscription exists")
	return cmd
}

func newEventRemoveCmd(e *env) *cobra.Command {
	var events, prefix, suffix, id string
	var force bool
	cmd := &cobra.Command{
		Use:     "remove [flags] ALIAS/BUCKET [ARN]",
		Aliases: []string{"rm"},
		Short:   "Remove event subscriptions of a bucket",
		Long: `Remove the subscriptions of a target (ARN), narrowed by --event, --prefix,
--suffix or --id when given; --force without ARN removes every subscription.`,
		Example: `  nexoctl event rm local/photos arn:nexo:sqs:us-east-1:_:webhook
  nexoctl event rm --force local/photos`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return cli.UsageError("%s takes ALIAS/BUCKET and an optional ARN", cmd.CommandPath())
			}
			return nil
		},
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			arn := ""
			if len(args) == 2 {
				arn = strings.TrimSpace(args[1])
			}
			if arn == "" && !force {
				return cli.UsageError("give the ARN of the subscriptions to remove, or --force to remove them all")
			}
			var types []string
			if cmd.Flags().Changed("event") {
				var err error
				if types, err = cli.ParseEventTypes(events); err != nil {
					return err
				}
			}
			ctx := cmd.Context()
			t, client, err := e.ilmBucket(ctx, args[0], "event")
			if err != nil {
				return err
			}
			cfg, err := client.GetNotifications(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			match := func(s cli.EventSubscription) bool {
				switch {
				case arn != "" && s.ARN != arn,
					id != "" && s.ID != id,
					cmd.Flags().Changed("prefix") && s.Prefix != prefix,
					cmd.Flags().Changed("suffix") && s.Suffix != suffix,
					types != nil && !cli.SameEvents(s.Events, types):
					return false
				}
				return true
			}
			var kept, removed []cli.EventSubscription
			for _, s := range cfg.Subscriptions {
				if match(s) {
					removed = append(removed, s)
				} else {
					kept = append(kept, s)
				}
			}
			if len(removed) == 0 {
				if arn == "" {
					return e.out.Note(fmt.Sprintf("`%s` has no event subscription.", t))
				}
				return fmt.Errorf("%s: no subscription of %s matches: %w", t, arn, domain.ErrNotFound)
			}
			if err := client.PutNotifications(ctx, t.Bucket, cli.BucketNotifications{Subscriptions: kept}); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			for _, s := range removed {
				if err := e.out.Info(eventRecord{Target: t.String(), Bucket: t.Bucket, EventSubscription: s, Removed: true},
					fmt.Sprintf("Removed the subscription of `%s` (%s) from `%s`.", s.ARN, strings.Join(s.Events, ","), t)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&events, "event", defaultEvents, "only the subscriptions to these events")
	f.StringVar(&prefix, "prefix", "", "only the subscriptions with this prefix filter")
	f.StringVar(&suffix, "suffix", "", "only the subscriptions with this suffix filter")
	f.StringVar(&id, "id", "", "only the subscription with this ID")
	f.BoolVar(&force, "force", false, "remove every subscription (without ARN)")
	return cmd
}

func newEventListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS/BUCKET [ARN]",
		Aliases:           []string{"ls"},
		Short:             "List the event subscriptions of a bucket",
		Example:           "  nexoctl event ls local/photos",
		Args:              func(cmd *cobra.Command, args []string) error { return argsBetween(cmd, args, 1, 2) },
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.ilmBucket(ctx, args[0], "event")
			if err != nil {
				return err
			}
			cfg, err := client.GetNotifications(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			listed := 0
			for _, s := range cfg.Subscriptions {
				if len(args) == 2 && s.ARN != args[1] {
					continue
				}
				listed++
				text := fmt.Sprintf("%s   %s", s.ARN, strings.Join(s.Events, ","))
				var filter []string
				if s.Prefix != "" {
					filter = append(filter, fmt.Sprintf("prefix=%q", s.Prefix))
				}
				if s.Suffix != "" {
					filter = append(filter, fmt.Sprintf("suffix=%q", s.Suffix))
				}
				if len(filter) > 0 {
					text += "   Filter: " + strings.Join(filter, ", ")
				}
				if s.ID != "" {
					text += "   ID: " + s.ID
				}
				if err := e.out.Result(eventRecord{Target: t.String(), Bucket: t.Bucket, EventSubscription: s}, text); err != nil {
					return err
				}
			}
			if listed == 0 {
				return e.out.Note(fmt.Sprintf("`%s` has no event subscription.", t))
			}
			return nil
		},
	}
}

// argsBetween requires between min and max positional arguments.
func argsBetween(cmd *cobra.Command, args []string, minN, maxN int) error {
	if len(args) < minN || len(args) > maxN {
		return cli.UsageError("%s takes %d to %d argument(s), got %d", cmd.CommandPath(), minN, maxN, len(args))
	}
	return nil
}
