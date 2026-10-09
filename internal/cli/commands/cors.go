package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
)

func newCORSCmd(e *env) *cobra.Command {
	cmd := groupCmd("cors", "Manage the CORS configuration of buckets")
	cmd.Long = `Manage the CORS (cross-origin resource sharing) rules of buckets: the origins,
methods and headers that browsers of other sites may use. The configuration is a
JSON document ({"rules": [{"allowedOrigins": [...], "allowedMethods": [...]}]},
the form of "cors get" and of the console; the {"CORSRules": [...]} form of the
AWS CLI is accepted too) or the S3 CORSConfiguration XML.`
	cmd.AddCommand(newCORSSetCmd(e), newCORSGetCmd(e), newCORSRemoveCmd(e))
	return cmd
}

func newCORSSetCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "set ALIAS/BUCKET [FILE]",
		Short: "Replace the CORS configuration of a bucket (JSON or S3 XML)",
		Long: `Replace the CORS configuration of a bucket with a document read from FILE or
from the standard input ("-" or no FILE).`,
		Example: `  nexoctl cors set local/site cors.json
  nexoctl cors set local/site cors.xml`,
		Args:              func(cmd *cobra.Command, args []string) error { return argsBetween(cmd, args, 1, 2) },
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := e.readConfigDocument(args[1:])
			if err != nil {
				return err
			}
			cfg, err := cli.ParseCORS(data)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			t, client, err := e.ilmBucket(ctx, args[0], "cors")
			if err != nil {
				return err
			}
			if err := client.PutCORS(ctx, t.Bucket, cfg); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(corsRecord{Target: t.String(), Bucket: t.Bucket, Rules: cfg.Rules},
				fmt.Sprintf("CORS configuration of `%s` set (%d %s).", t, len(cfg.Rules), plural(len(cfg.Rules), "rule", "rules")))
		},
	}
}

func newCORSGetCmd(e *env) *cobra.Command {
	var asXML bool
	cmd := &cobra.Command{
		Use:     "get [--xml] ALIAS/BUCKET",
		Aliases: []string{"info"},
		Short:   "Print the CORS configuration of a bucket (JSON or S3 XML)",
		Example: `  nexoctl cors get local/site > cors.json
  nexoctl cors get --xml local/site`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.ilmBucket(ctx, args[0], "cors")
			if err != nil {
				return err
			}
			cfg, err := client.GetCORS(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if e.out.JSON() {
				return e.out.Result(corsRecord{Target: t.String(), Bucket: t.Bucket, Rules: cfg.Rules}, "")
			}
			if len(cfg.Rules) == 0 {
				return e.out.Result(nil, fmt.Sprintf("`%s` has no CORS configuration.", t))
			}
			var doc []byte
			if asXML {
				doc, err = cli.EncodeCORSXML(*cfg)
			} else {
				doc, err = encodeJSONDocument(cfg)
			}
			if err != nil {
				return err
			}
			_, err = e.out.Stdout().Write(doc)
			return err
		},
	}
	cmd.Flags().BoolVar(&asXML, "xml", false, "print the S3 CORSConfiguration XML")
	return cmd
}

func newCORSRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "remove ALIAS/BUCKET",
		Aliases:           []string{"rm"},
		Short:             "Remove the CORS configuration of a bucket",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.ilmBucket(ctx, args[0], "cors")
			if err != nil {
				return err
			}
			if err := client.PutCORS(ctx, t.Bucket, cli.CORSConfiguration{}); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(corsRecord{Target: t.String(), Bucket: t.Bucket, Rules: []cli.CORSRule{}},
				fmt.Sprintf("Removed the CORS configuration of `%s`.", t))
		},
	}
}
