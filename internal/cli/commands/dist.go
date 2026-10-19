package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// newDistCmd returns dist: the distributions (docs/10 §10.2, C-DST-01).
func newDistCmd(e *env) *cobra.Command {
	cmd := groupCmd("dist", "Manage the distributions: custom domains of the buckets")
	cmd.Long = `Manage the distributions of a Nexo server (Admin API, admin:ManageDistributions):
a bucket, or a prefix of it, exposed on custom domains as a website (index
document, SPA fallback, error pages, redirects, behaviors per path, signed URLs)
or as an S3 endpoint. The changes reach every node of a cluster without restart.

Every change reads the distribution, changes it and writes it back guarded by
its ETag: a concurrent change is retried. Point the DNS of the domains (CNAME,
A/AAAA) to the cluster or to the proxies in front of it.`
	domainCmd := groupCmd("domain", "Add and remove the domains (aliases) of a distribution")
	domainCmd.AddCommand(newDistDomainCmd(e, true), newDistDomainCmd(e, false))
	behavior := groupCmd("behavior", "Manage the behaviors per path of a distribution")
	behavior.AddCommand(newDistBehaviorAddCmd(e), newDistBehaviorListCmd(e), newDistBehaviorRemoveCmd(e), newDistBehaviorMoveCmd(e))
	errorPage := groupCmd("error-page", "Replace the error answers of a distribution with pages")
	errorPage.AddCommand(newDistErrorPageSetCmd(e), newDistErrorPageRemoveCmd(e))
	redirect := groupCmd("redirect", "Manage the redirections of a distribution")
	redirect.AddCommand(newDistRedirectAddCmd(e), newDistRedirectRemoveCmd(e))
	secret := groupCmd("origin-secret", "Manage the origin secret of the proxies in front of the cluster")
	secret.AddCommand(newDistOriginSecretRotateCmd(e))
	cmd.AddCommand(
		newDistCreateCmd(e), newDistListCmd(e), newDistInfoCmd(e), newDistEditCmd(e),
		newDistEnableCmd(e, true), newDistEnableCmd(e, false), newDistRemoveCmd(e),
		newDistExportCmd(e), newDistImportCmd(e),
		domainCmd, behavior, errorPage, redirect, newDistHeadersCmd(e), newDistRestrictCmd(e),
		newDistKeyGroupCmd(e), newDistSignURLCmd(e), newDistCertCmd(e), secret,
		newDistTestCmd(e), newDistResolveCmd(e), newDistCacheCmd(e), newDistInvalidateCmd(e),
	)
	return cmd
}

// distRecord is a distribution (dist info|create|edit...).
type distRecord struct {
	Alias        string                 `json:"alias"`
	Action       string                 `json:"action,omitempty"`
	Distribution nexoadmin.Distribution `json:"distribution"`
}

// distActionRecord confirms an action on a distribution.
type distActionRecord struct {
	Alias        string `json:"alias"`
	Distribution string `json:"distribution"`
	Action       string `json:"action"`
}

// distTestRecord is how a distribution serves a URL.
type distTestRecord struct {
	Alias        string `json:"alias"`
	Distribution string `json:"distribution"`
	URL          string `json:"url"`
	nexoadmin.DistributionTestResult
}

// distRouteRecord is the resolution of a host.
type distRouteRecord struct {
	Alias string `json:"alias"`
	nexoadmin.Route
}

// distAttempts bounds the read-modify-write of a distribution when it
// changes concurrently.
const distAttempts = 3

// distAlias parses the arguments ALIAS ID.
func (e *env) distAlias(ctx context.Context, args []string) (string, cli.AdminClient, string, error) {
	alias, client, err := e.adminAlias(ctx, args[0])
	if err != nil {
		return "", nil, "", err
	}
	return alias, client, args[1], nil
}

// editDistribution applies change to a distribution: read with its ETag,
// change, replace with If-Match, retried when it changed meanwhile.
func editDistribution(ctx context.Context, client cli.AdminClient, id string, change func(d *nexoadmin.Distribution) error) (*nexoadmin.Distribution, error) {
	for attempt := 1; ; attempt++ {
		d, err := client.GetDistribution(ctx, id)
		if err != nil {
			return nil, err
		}
		etag := d.ETag
		if err := change(d); err != nil {
			return nil, err
		}
		out, err := client.UpdateDistribution(ctx, *d, etag)
		if errors.Is(err, domain.ErrPreconditionFailed) && attempt < distAttempts {
			continue
		}
		return out, err
	}
}

// editAndReport runs editDistribution and prints the confirmation.
func (e *env) editAndReport(cmd *cobra.Command, args []string, action string, change func(d *nexoadmin.Distribution) error, text func(d *nexoadmin.Distribution) string) error {
	ctx := cmd.Context()
	alias, client, id, err := e.distAlias(ctx, args)
	if err != nil {
		return err
	}
	d, err := editDistribution(ctx, client, id, change)
	if err != nil {
		return fmt.Errorf("distribution %q: %w", id, err)
	}
	return e.out.Info(distRecord{Alias: alias, Action: action, Distribution: *d}, text(d))
}

// behaviorFlags are the flags of a behavior (dist create|edit|behavior add).
type behaviorFlags struct {
	access, index, spa, keyGroup, cacheControl string
	methods, headers                           []string
	images                                     bool
}

func (b *behaviorFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&b.access, "access", "", "access: public, signed (URLs/cookies of --key-group), authenticated (SigV4) or deny")
	f.StringVar(&b.index, "index", "", "index document appended to the paths ending with /")
	f.StringVar(&b.spa, "spa", "", "path served (200) instead of a missing object (single page application)")
	f.StringVar(&b.keyGroup, "key-group", "", "key group of the signed URLs and cookies")
	f.StringVar(&b.cacheControl, "cache-control", "", "Cache-Control sent to the browsers")
	f.StringArrayVar(&b.methods, "methods", nil, "allowed methods (default GET,HEAD,OPTIONS)")
	f.StringArrayVar(&b.headers, "header", nil, "response header 'Name: value' (repeatable)")
	f.BoolVar(&b.images, "negotiate-images", false, "serve the variants of the images (media worker image rule) at their own URL:\n"+
		"format from Accept or ?format=, width from ?w=, ?format=original for the original (website mode)")
}

// apply sets the changed flags on b.
func (b *behaviorFlags) apply(cmd *cobra.Command, beh *nexoadmin.Behavior) error {
	f := cmd.Flags()
	if f.Changed("access") {
		beh.Access = b.access
	}
	if f.Changed("index") {
		beh.IndexDocument = b.index
	}
	if f.Changed("spa") {
		beh.SPAFallback = b.spa
	}
	if f.Changed("key-group") {
		if b.keyGroup == "" {
			beh.Signing = nil
		} else {
			beh.Signing = &nexoadmin.Signing{KeyGroup: b.keyGroup}
		}
	}
	if f.Changed("cache-control") {
		if beh.Cache == nil {
			beh.Cache = &nexoadmin.BehaviorCache{}
		}
		beh.Cache.CacheControl = b.cacheControl
	}
	if f.Changed("methods") {
		beh.AllowedMethods = splitList(b.methods)
	}
	if f.Changed("negotiate-images") {
		beh.Images = &nexoadmin.BehaviorImages{Negotiate: b.images}
	}
	if f.Changed("header") {
		h, err := parseHeaderFlags("header", b.headers)
		if err != nil {
			return err
		}
		if beh.ResponseHeaders == nil {
			beh.ResponseHeaders = map[string]string{}
		}
		for k, v := range h {
			beh.ResponseHeaders[k] = v
		}
	}
	return nil
}

func newDistCreateCmd(e *env) *cobra.Command {
	var (
		bucket, prefix, mode, certificate, comment string
		domains                                    []string
		disabled                                   bool
		bf                                         behaviorFlags
	)
	cmd := &cobra.Command{
		Use:   "create ALIAS ID --bucket BUCKET --domain DOMAIN... [--mode website|s3]",
		Short: "Create a distribution",
		Long: `Create a distribution of a bucket (or of a prefix of it, website mode) on one or
more domains, exact or wildcard (*.preview.example.com). The flags of the
default behavior (--access, --index, --spa, --cache-control...) apply to every
path the behaviors do not override (see "nexoctl dist behavior add").`,
		Example: `  nexoctl dist create local website --bucket web --prefix /prod --mode website \
      --domain www.example.com --domain example.com --index index.html --spa /index.html`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if bucket == "" {
				return cli.UsageError("--bucket is required")
			}
			if len(splitList(domains)) == 0 {
				return cli.UsageError("at least one --domain is required")
			}
			d := nexoadmin.Distribution{
				ID: args[1], Enabled: !disabled, Comment: comment, Aliases: splitList(domains),
				Origin: nexoadmin.Origin{Bucket: bucket, PathPrefix: prefix}, Mode: mode,
				TLS: nexoadmin.DistributionTLS{Certificate: certificate},
			}
			if !cmd.Flags().Changed("access") {
				d.DefaultBehavior.Access = nexoadmin.AccessPublic
			}
			if err := bf.apply(cmd, &d.DefaultBehavior); err != nil {
				return err
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			out, err := client.CreateDistribution(ctx, d)
			if err != nil {
				return fmt.Errorf("distribution %q: %w", d.ID, err)
			}
			return e.out.Info(distRecord{Alias: alias, Action: "created", Distribution: *out},
				fmt.Sprintf("Created the distribution `%s` (%s) of `%s` on %s.", out.ID, out.Mode, originOf(out), strings.Join(out.Aliases, ", ")))
		},
	}
	f := cmd.Flags()
	f.StringVar(&bucket, "bucket", "", "origin bucket")
	f.StringVar(&prefix, "prefix", "", "origin prefix (website mode): the distribution serves only the keys under it")
	f.StringVar(&mode, "mode", "", "website (default) or s3")
	f.StringArrayVar(&domains, "domain", nil, "domain (alias) of the distribution, exact or *.wildcard (repeatable)")
	f.StringVar(&certificate, "certificate", "", "TLS certificate: auto (ACME), none, upload:ID")
	f.StringVar(&comment, "comment", "", "free text")
	f.BoolVar(&disabled, "disabled", false, "create the distribution disabled")
	bf.register(cmd)
	return cmd
}

// originOf renders the origin of a distribution ("web/prod").
func originOf(d *nexoadmin.Distribution) string {
	return d.Origin.Bucket + d.Origin.PathPrefix
}

func newDistListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS",
		Aliases:           []string{"ls"},
		Short:             "List the distributions",
		Example:           "  nexoctl dist ls local",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			list, err := client.ListDistributions(ctx)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				return e.out.Note("No distribution.")
			}
			if err := e.out.Text(fmt.Sprintf("%-20s %-8s %-8s %-20s %-18s %s", "ID", "STATE", "MODE", "ORIGIN", "TLS", "DOMAINS")); err != nil {
				return err
			}
			for _, d := range list {
				state := e.out.Paint(cli.ColorGreen, fmt.Sprintf("%-8s", "enabled"))
				if !d.Enabled {
					state = fmt.Sprintf("%-8s", "disabled")
				}
				text := fmt.Sprintf("%-20s %s %-8s %-20s %-18s %s", d.ID, state, d.Mode, originOf(&d), orDash(d.TLS.Certificate), strings.Join(d.Aliases, ", "))
				if err := e.out.Result(distRecord{Alias: alias, Distribution: d}, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newDistInfoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "info ALIAS ID",
		Short:             "Show a distribution",
		Example:           "  nexoctl dist info local website",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, id, err := e.distAlias(ctx, args)
			if err != nil {
				return err
			}
			d, err := client.GetDistribution(ctx, id)
			if err != nil {
				return fmt.Errorf("distribution %q: %w", id, err)
			}
			return e.out.Result(distRecord{Alias: alias, Distribution: *d}, formatDistribution(d))
		},
	}
}

// formatBehavior renders the settings of a behavior on one line.
func formatBehavior(b nexoadmin.Behavior) string {
	var parts []string
	if b.Access != "" {
		parts = append(parts, "access "+b.Access)
	}
	if b.Signing != nil {
		parts = append(parts, "key group "+b.Signing.KeyGroup)
	}
	if len(b.AllowedMethods) > 0 {
		parts = append(parts, "methods "+strings.Join(b.AllowedMethods, ","))
	}
	if b.IndexDocument != "" {
		parts = append(parts, "index "+b.IndexDocument)
	}
	if b.SPAFallback != "" {
		parts = append(parts, "spa "+b.SPAFallback)
	}
	if b.Cache != nil && b.Cache.CacheControl != "" {
		parts = append(parts, fmt.Sprintf("cache-control %q", b.Cache.CacheControl))
	}
	if b.Images != nil {
		if b.Images.Negotiate {
			parts = append(parts, "images negotiated")
		} else {
			parts = append(parts, "images original")
		}
	}
	for _, k := range cli.SortedKeys(b.ResponseHeaders) {
		parts = append(parts, fmt.Sprintf("header %s: %s", k, b.ResponseHeaders[k]))
	}
	if len(parts) == 0 {
		return "(inherited)"
	}
	return strings.Join(parts, ", ")
}

// formatDistribution renders a distribution.
func formatDistribution(d *nexoadmin.Distribution) string {
	lines := []string{
		"ID: " + d.ID,
		"State: " + map[bool]string{true: "enabled", false: "disabled"}[d.Enabled],
		"Mode: " + d.Mode,
		"Origin: " + originOf(d),
		"Domains: " + strings.Join(d.Aliases, ", "),
		"Default behavior: " + formatBehavior(d.DefaultBehavior),
	}
	if d.Comment != "" {
		lines = append(lines, "Comment: "+d.Comment)
	}
	for i, b := range d.Behaviors {
		lines = append(lines, fmt.Sprintf("Behavior %d: %s → %s", i+1, formatPattern(b.PathPattern), formatBehavior(b)))
	}
	for _, p := range d.ErrorPages {
		resp := ""
		if p.ResponseStatus != 0 {
			resp = fmt.Sprintf(" (answered %d)", p.ResponseStatus)
		}
		lines = append(lines, fmt.Sprintf("Error page %d: %s%s", p.Status, p.Path, resp))
	}
	for i, r := range d.Redirects {
		lines = append(lines, fmt.Sprintf("Redirect %d: %s", i+1, formatRedirect(r)))
	}
	for _, k := range cli.SortedKeys(d.Headers.Response) {
		lines = append(lines, fmt.Sprintf("Header %s: %s", k, d.Headers.Response[k]))
	}
	if c := d.Headers.CORS; c != nil {
		lines = append(lines, fmt.Sprintf("CORS: origins %s, methods %s, max age %d", strings.Join(c.AllowedOrigins, ","), orDash(strings.Join(c.AllowedMethods, ",")), c.MaxAge))
	}
	if r := d.Restrictions.SourceIP; r != nil {
		lines = append(lines, fmt.Sprintf("Source IP: allow %s, deny %s", orDash(strings.Join(r.Allow, ",")), orDash(strings.Join(r.Deny, ","))))
	}
	if s := d.Restrictions.OriginSecretHeader; s != nil {
		lines = append(lines, fmt.Sprintf("Origin secret: header %s, %d active %s", s.Name, len(s.Values), plural(len(s.Values), "secret", "secrets")))
	}
	if rl := d.Restrictions.RateLimit; rl != nil {
		per := rl.Per
		if per == "" {
			per = "ip"
		}
		burst := rl.Burst
		if burst == 0 {
			burst = rl.RequestsPerSecond
		}
		lines = append(lines, fmt.Sprintf("Rate limit: %d/s, burst %d, per %s (each node)", rl.RequestsPerSecond, burst, per))
	}
	lines = append(lines, "TLS: "+orDash(d.TLS.Certificate))
	return strings.Join(lines, "\n")
}

// formatRedirect renders a redirection.
func formatRedirect(r nexoadmin.Redirect) string {
	status := r.Status
	if status == 0 {
		status = 301
	}
	switch {
	case r.HTTPSOnly:
		return fmt.Sprintf("HTTP → HTTPS (%d)", status)
	case r.HostFrom != "":
		return fmt.Sprintf("host %s → %s (%d)", r.HostFrom, r.ToHost, status)
	}
	return fmt.Sprintf("%s → %s (%d)", r.From, r.To, status)
}

func newDistEditCmd(e *env) *cobra.Command {
	var (
		bucket, prefix, mode, comment, certificate string
		bf                                         behaviorFlags
	)
	cmd := &cobra.Command{
		Use:   "edit ALIAS ID [--bucket B] [--prefix P] [--mode M] [--access A] [--index I] [--spa S] ...",
		Short: "Change the origin, the mode or the default behavior of a distribution",
		Example: `  nexoctl dist edit local website --prefix /v2 --cache-control 'public, max-age=300'
  nexoctl dist edit local website --access signed --key-group rilasci`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().NFlag() == 0 {
				return cli.UsageError("nothing to change: give at least one flag")
			}
			return e.editAndReport(cmd, args, "edited", func(d *nexoadmin.Distribution) error {
				f := cmd.Flags()
				if f.Changed("bucket") {
					d.Origin.Bucket = bucket
				}
				if f.Changed("prefix") {
					d.Origin.PathPrefix = prefix
				}
				if f.Changed("mode") {
					d.Mode = mode
				}
				if f.Changed("comment") {
					d.Comment = comment
				}
				if f.Changed("certificate") {
					d.TLS.Certificate = certificate
				}
				return bf.apply(cmd, &d.DefaultBehavior)
			}, func(d *nexoadmin.Distribution) string { return fmt.Sprintf("Updated the distribution `%s`.", d.ID) })
		},
	}
	f := cmd.Flags()
	f.StringVar(&bucket, "bucket", "", "origin bucket")
	f.StringVar(&prefix, "prefix", "", "origin prefix")
	f.StringVar(&mode, "mode", "", "website or s3")
	f.StringVar(&comment, "comment", "", "free text")
	f.StringVar(&certificate, "certificate", "", "TLS certificate: auto, none, upload:ID")
	bf.register(cmd)
	return cmd
}

func newDistEnableCmd(e *env, enable bool) *cobra.Command {
	use, short, action := "disable ALIAS ID", "Disable a distribution (its domains answer 403)", "disabled"
	if enable {
		use, short, action = "enable ALIAS ID", "Enable a distribution", "enabled"
	}
	return &cobra.Command{
		Use:               use,
		Short:             short,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.editAndReport(cmd, args, action, func(d *nexoadmin.Distribution) error {
				d.Enabled = enable
				return nil
			}, func(d *nexoadmin.Distribution) string { return fmt.Sprintf("Distribution `%s` %s.", d.ID, action) })
		},
	}
}

func newDistRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "remove ALIAS ID",
		Aliases:           []string{"rm"},
		Short:             "Remove a distribution",
		Example:           "  nexoctl dist rm local website",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, id, err := e.distAlias(ctx, args)
			if err != nil {
				return err
			}
			if err := client.DeleteDistribution(ctx, id); err != nil {
				return fmt.Errorf("distribution %q: %w", id, err)
			}
			return e.out.Info(distActionRecord{Alias: alias, Distribution: id, Action: "removed"}, fmt.Sprintf("Removed the distribution `%s`.", id))
		},
	}
}

// exportDocument is a distribution without the members the server
// computes (ETag, dates), as JSON.
func exportDocument(d nexoadmin.Distribution) ([]byte, error) {
	d.ETag, d.CreatedAt, d.UpdatedAt = "", nil, nil
	return json.Marshal(d)
}

func newDistExportCmd(e *env) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "export ALIAS [ID] [-o FILE]",
		Short: "Export distributions as YAML",
		Long: `Export a distribution (every distribution without ID, one YAML document each) to
the standard output, or to FILE with --output: the documents of "nexoctl dist
import", for the GitOps management of the distributions.`,
		Example:           "  nexoctl dist export local website -o website.yaml\n  nexoctl dist export local > distributions.yaml",
		Args:              func(cmd *cobra.Command, args []string) error { return argsBetween(cmd, args, 1, 2) },
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			var list []nexoadmin.Distribution
			if len(args) == 2 {
				d, err := client.GetDistribution(ctx, args[1])
				if err != nil {
					return fmt.Errorf("distribution %q: %w", args[1], err)
				}
				list = []nexoadmin.Distribution{*d}
			} else if list, err = client.ListDistributions(ctx); err != nil {
				return err
			}
			toStdout := output == "" || output == "-"
			if toStdout && e.out.JSON() {
				for _, d := range list {
					if err := e.out.Result(distRecord{Alias: alias, Distribution: d}, ""); err != nil {
						return err
					}
				}
				return nil
			}
			var out []byte
			for i, d := range list {
				doc, err := exportDocument(d)
				if err != nil {
					return err
				}
				y, err := cli.JSONToYAML(doc)
				if err != nil {
					return err
				}
				if i > 0 {
					out = append(out, "---\n"...)
				}
				out = append(out, y...)
			}
			if toStdout {
				_, err := e.out.Stdout().Write(out)
				return err
			}
			if err := os.WriteFile(output, out, 0o600); err != nil {
				return err
			}
			return e.out.Info(distExportRecord{Alias: alias, File: output, Distributions: len(list)},
				fmt.Sprintf("Exported %d %s to %s.", len(list), plural(len(list), "distribution", "distributions"), output))
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "write the documents to this file")
	return cmd
}

// distExportRecord reports an export to a file.
type distExportRecord struct {
	Alias         string `json:"alias"`
	File          string `json:"file"`
	Distributions int    `json:"distributions"`
}

func newDistImportCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "import ALIAS FILE",
		Short: "Create or replace the distributions of a YAML file",
		Long: `Create, or replace when they exist, the distributions of a YAML (or JSON) file of
one or more documents (the format of "nexoctl dist export"). The server
validates every distribution.`,
		Example:           "  nexoctl dist import local distributions.yaml",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			docs, err := e.readYAMLDocuments(args[1])
			if err != nil {
				return err
			}
			list := make([]nexoadmin.Distribution, 0, len(docs))
			for i, doc := range docs {
				var d nexoadmin.Distribution
				if err := json.Unmarshal(doc, &d); err != nil {
					return fmt.Errorf("%w: %s: document %d: %v", domain.ErrInvalidArgument, args[1], i+1, err)
				}
				if d.ID == "" {
					return fmt.Errorf("%w: %s: document %d has no id", domain.ErrInvalidArgument, args[1], i+1)
				}
				list = append(list, d)
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			for _, d := range list {
				action := "updated"
				cur, err := client.GetDistribution(ctx, d.ID)
				switch {
				case err == nil:
					var out *nexoadmin.Distribution
					if out, err = client.UpdateDistribution(ctx, d, cur.ETag); err == nil {
						d = *out
					}
				case errors.Is(err, domain.ErrNotFound):
					action = "created"
					var out *nexoadmin.Distribution
					if out, err = client.CreateDistribution(ctx, d); err == nil {
						d = *out
					}
				}
				if err != nil {
					return fmt.Errorf("distribution %q: %w", d.ID, err)
				}
				if err := e.out.Info(distRecord{Alias: alias, Action: action, Distribution: d}, fmt.Sprintf("Distribution `%s` %s.", d.ID, action)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newDistDomainCmd(e *env, add bool) *cobra.Command {
	use, short, action := "remove ALIAS ID DOMAIN...", "Remove domains from a distribution", "domains removed"
	aliases := []string{"rm"}
	if add {
		use, short, action, aliases = "add ALIAS ID DOMAIN...", "Add domains (exact or *.wildcard) to a distribution", "domains added", nil
	}
	return &cobra.Command{
		Use:               use,
		Aliases:           aliases,
		Short:             short,
		Example:           "  nexoctl dist domain add local website '*.preview.example.com'\n  nexoctl dist domain rm local website example.com",
		Args:              minArgs(3),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			domains := args[2:]
			return e.editAndReport(cmd, args, action, func(d *nexoadmin.Distribution) error {
				for _, name := range domains {
					name = strings.ToLower(strings.TrimSpace(name))
					i := slices.Index(d.Aliases, name)
					switch {
					case add && i < 0:
						d.Aliases = append(d.Aliases, name)
					case !add && i < 0:
						return fmt.Errorf("domain %s of the distribution: %w", name, domain.ErrNotFound)
					case !add:
						d.Aliases = slices.Delete(d.Aliases, i, i+1)
					}
				}
				return nil
			}, func(d *nexoadmin.Distribution) string {
				return fmt.Sprintf("Domains of `%s`: %s.", d.ID, strings.Join(d.Aliases, ", "))
			})
		},
	}
}

func newDistBehaviorAddCmd(e *env) *cobra.Command {
	var (
		pf       patternFlags
		bf       behaviorFlags
		position int
	)
	cmd := &cobra.Command{
		Use:   "add ALIAS ID (--prefix|--glob|--regex PATTERN) [--access A] [--cache-control CC] ...",
		Short: "Add a behavior for the paths of a pattern",
		Long: `Add a behavior: how the paths of a pattern are served. The behaviors are evaluated
in order and the first match wins; the fields a behavior does not set inherit
from the default behavior. The paths include their leading "/".`,
		Example: `  nexoctl dist behavior add local website --glob '/assets/**' --cache-control 'public, max-age=31536000, immutable'
  nexoctl dist behavior add local website --regex '^/downloads/.*\.zip$' --access signed --key-group rilasci
  nexoctl dist behavior add local website --prefix /internal/ --access deny --position 1
  nexoctl dist behavior add local website --prefix /photos/ --negotiate-images`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern, err := pf.pattern(cmd)
			if err != nil {
				return err
			}
			if pattern == nil {
				return cli.UsageError("a behavior needs --prefix, --glob or --regex")
			}
			b := nexoadmin.Behavior{PathPattern: pattern}
			if err := bf.apply(cmd, &b); err != nil {
				return err
			}
			return e.editAndReport(cmd, args, "behavior added", func(d *nexoadmin.Distribution) error {
				at := len(d.Behaviors)
				if position > 0 && position <= len(d.Behaviors) {
					at = position - 1
				}
				d.Behaviors = slices.Insert(d.Behaviors, at, b)
				return nil
			}, func(d *nexoadmin.Distribution) string {
				return fmt.Sprintf("Added a behavior for %s to `%s` (%d %s).", formatPattern(pattern), d.ID, len(d.Behaviors), plural(len(d.Behaviors), "behavior", "behaviors"))
			})
		},
	}
	pf.register(cmd, "paths of the behavior")
	bf.register(cmd)
	cmd.Flags().IntVar(&position, "position", 0, "1-based position among the behaviors (default: last)")
	return cmd
}

func newDistBehaviorListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS ID",
		Aliases:           []string{"ls"},
		Short:             "List the behaviors of a distribution, in evaluation order",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, id, err := e.distAlias(ctx, args)
			if err != nil {
				return err
			}
			d, err := client.GetDistribution(ctx, id)
			if err != nil {
				return fmt.Errorf("distribution %q: %w", id, err)
			}
			type behaviorRecord struct {
				Alias        string `json:"alias"`
				Distribution string `json:"distribution"`
				// Index is 1-based, 0 for the default behavior.
				Index    int                `json:"index"`
				Behavior nexoadmin.Behavior `json:"behavior"`
			}
			for i, b := range d.Behaviors {
				text := fmt.Sprintf("%d. %s → %s", i+1, formatPattern(b.PathPattern), formatBehavior(b))
				if err := e.out.Result(behaviorRecord{Alias: alias, Distribution: id, Index: i + 1, Behavior: b}, text); err != nil {
					return err
				}
			}
			return e.out.Result(behaviorRecord{Alias: alias, Distribution: id, Behavior: d.DefaultBehavior},
				"default → "+formatBehavior(d.DefaultBehavior))
		},
	}
}

// behaviorIndex parses a 1-based index of the behaviors of d.
func behaviorIndex(d *nexoadmin.Distribution, arg string) (int, error) {
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > len(d.Behaviors) {
		return 0, cli.UsageError("behavior %q: expected 1 to %d", arg, len(d.Behaviors))
	}
	return n - 1, nil
}

func newDistBehaviorRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "remove ALIAS ID N",
		Aliases:           []string{"rm"},
		Short:             "Remove the behavior N (1-based, see behavior ls)",
		Args:              exactArgs(3),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.editAndReport(cmd, args, "behavior removed", func(d *nexoadmin.Distribution) error {
				i, err := behaviorIndex(d, args[2])
				if err != nil {
					return err
				}
				d.Behaviors = slices.Delete(d.Behaviors, i, i+1)
				return nil
			}, func(d *nexoadmin.Distribution) string {
				return fmt.Sprintf("Removed the behavior %s of `%s`.", args[2], d.ID)
			})
		},
	}
}

func newDistBehaviorMoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "move ALIAS ID FROM TO",
		Short:             "Move a behavior (1-based positions): the first match wins",
		Example:           "  nexoctl dist behavior move local website 3 1",
		Args:              exactArgs(4),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.editAndReport(cmd, args, "behavior moved", func(d *nexoadmin.Distribution) error {
				from, err := behaviorIndex(d, args[2])
				if err != nil {
					return err
				}
				to, err := behaviorIndex(d, args[3])
				if err != nil {
					return err
				}
				b := d.Behaviors[from]
				d.Behaviors = slices.Insert(slices.Delete(d.Behaviors, from, from+1), to, b)
				return nil
			}, func(d *nexoadmin.Distribution) string {
				return fmt.Sprintf("Moved the behavior %s of `%s` to position %s.", args[2], d.ID, args[3])
			})
		},
	}
}

func newDistErrorPageSetCmd(e *env) *cobra.Command {
	var responseStatus int
	cmd := &cobra.Command{
		Use:     "set ALIAS ID STATUS PATH [--response-status N]",
		Short:   "Answer an error status with a page of the distribution",
		Example: "  nexoctl dist error-page set local website 404 /404.html\n  nexoctl dist error-page set local website 403 /404.html --response-status 404",
		Args:    exactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			status, err := strconv.Atoi(args[2])
			if err != nil {
				return cli.UsageError("status %q is not a number", args[2])
			}
			page := nexoadmin.ErrorPage{Status: status, Path: args[3], ResponseStatus: responseStatus}
			return e.editAndReport(cmd, args, "error page set", func(d *nexoadmin.Distribution) error {
				i := slices.IndexFunc(d.ErrorPages, func(p nexoadmin.ErrorPage) bool { return p.Status == status })
				if i >= 0 {
					d.ErrorPages[i] = page
				} else {
					d.ErrorPages = append(d.ErrorPages, page)
				}
				return nil
			}, func(d *nexoadmin.Distribution) string {
				return fmt.Sprintf("Errors %d of `%s` answer %s.", status, d.ID, page.Path)
			})
		},
	}
	cmd.Flags().IntVar(&responseStatus, "response-status", 0, "status sent with the page (default: the error status)")
	return cmd
}

func newDistErrorPageRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "remove ALIAS ID STATUS",
		Aliases: []string{"rm"},
		Short:   "Remove the error page of a status",
		Args:    exactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			status, err := strconv.Atoi(args[2])
			if err != nil {
				return cli.UsageError("status %q is not a number", args[2])
			}
			return e.editAndReport(cmd, args, "error page removed", func(d *nexoadmin.Distribution) error {
				i := slices.IndexFunc(d.ErrorPages, func(p nexoadmin.ErrorPage) bool { return p.Status == status })
				if i < 0 {
					return fmt.Errorf("error page %d: %w", status, domain.ErrNotFound)
				}
				d.ErrorPages = slices.Delete(d.ErrorPages, i, i+1)
				return nil
			}, func(d *nexoadmin.Distribution) string {
				return fmt.Sprintf("Removed the error page %d of `%s`.", status, d.ID)
			})
		},
	}
}

func newDistRedirectAddCmd(e *env) *cobra.Command {
	var r nexoadmin.Redirect
	cmd := &cobra.Command{
		Use:   "add ALIAS ID (--from RE --to TARGET | --host-from HOST --to-host HOST | --https-only) [--status N]",
		Short: "Add a redirection (the first that applies wins)",
		Example: `  nexoctl dist redirect add local website --from '^/old/(.*)$' --to '/new/$1'
  nexoctl dist redirect add local website --host-from example.com --to-host www.example.com
  nexoctl dist redirect add local website --https-only`,
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kinds := 0
			if r.From != "" || r.To != "" {
				kinds++
			}
			if r.HostFrom != "" || r.ToHost != "" {
				kinds++
			}
			if r.HTTPSOnly {
				kinds++
			}
			if kinds != 1 {
				return cli.UsageError("give one of --from/--to, --host-from/--to-host and --https-only")
			}
			return e.editAndReport(cmd, args, "redirect added", func(d *nexoadmin.Distribution) error {
				d.Redirects = append(d.Redirects, r)
				return nil
			}, func(d *nexoadmin.Distribution) string {
				return fmt.Sprintf("Added the redirection %d of `%s`: %s.", len(d.Redirects), d.ID, formatRedirect(r))
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&r.From, "from", "", "RE2 expression of the paths")
	f.StringVar(&r.To, "to", "", "target, with $1 and ${name} from the groups of --from")
	f.StringVar(&r.HostFrom, "host-from", "", "host to redirect")
	f.StringVar(&r.ToHost, "to-host", "", "target host (path and query kept)")
	f.BoolVar(&r.HTTPSOnly, "https-only", false, "redirect plain HTTP to HTTPS")
	f.IntVar(&r.Status, "status", 0, "301 (default), 302, 303, 307 or 308")
	return cmd
}

func newDistRedirectRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "remove ALIAS ID N",
		Aliases: []string{"rm"},
		Short:   "Remove the redirection N (1-based, see dist info)",
		Args:    exactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.editAndReport(cmd, args, "redirect removed", func(d *nexoadmin.Distribution) error {
				n, err := strconv.Atoi(args[2])
				if err != nil || n < 1 || n > len(d.Redirects) {
					return cli.UsageError("redirection %q: expected 1 to %d", args[2], len(d.Redirects))
				}
				d.Redirects = slices.Delete(d.Redirects, n-1, n)
				return nil
			}, func(d *nexoadmin.Distribution) string {
				return fmt.Sprintf("Removed the redirection %s of `%s`.", args[2], d.ID)
			})
		},
	}
}

// securityHeaders are the recommended security headers (dist headers
// --security).
func securityHeaders() map[string]string {
	return map[string]string{
		"Strict-Transport-Security": "max-age=63072000; includeSubDomains",
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "SAMEORIGIN",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
	}
}

func newDistHeadersCmd(e *env) *cobra.Command {
	var (
		set, unset, origins, methods []string
		maxAge                       int
		noCORS, security             bool
	)
	cmd := &cobra.Command{
		Use:   "headers ALIAS ID [--set 'Name: value']... [--unset NAME]... [--security] [--cors-origin O]... [--no-cors]",
		Short: "Set the response headers and the CORS policy of a distribution",
		Example: `  nexoctl dist headers local website --security
  nexoctl dist headers local website --set 'Cache-Control: no-cache' --cors-origin https://app.example.com --cors-method GET`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().NFlag() == 0 {
				return cli.UsageError("nothing to change: give at least one flag")
			}
			add, err := parseHeaderFlags("set", set)
			if err != nil {
				return err
			}
			return e.editAndReport(cmd, args, "headers set", func(d *nexoadmin.Distribution) error {
				if d.Headers.Response == nil {
					d.Headers.Response = map[string]string{}
				}
				if security {
					for k, v := range securityHeaders() {
						d.Headers.Response[k] = v
					}
				}
				for k, v := range add {
					d.Headers.Response[k] = v
				}
				for _, name := range unset {
					for k := range d.Headers.Response {
						if strings.EqualFold(k, name) {
							delete(d.Headers.Response, k)
						}
					}
				}
				switch {
				case noCORS:
					d.Headers.CORS = nil
				case len(origins) > 0 || cmd.Flags().Changed("cors-method") || cmd.Flags().Changed("cors-max-age"):
					c := d.Headers.CORS
					if c == nil {
						c = &nexoadmin.DistributionCORS{}
					}
					if len(origins) > 0 {
						c.AllowedOrigins = splitList(origins)
					}
					if cmd.Flags().Changed("cors-method") {
						c.AllowedMethods = splitList(methods)
					}
					if cmd.Flags().Changed("cors-max-age") {
						c.MaxAge = maxAge
					}
					d.Headers.CORS = c
				}
				return nil
			}, func(d *nexoadmin.Distribution) string {
				return fmt.Sprintf("Updated the headers of `%s` (%d response %s).", d.ID, len(d.Headers.Response), plural(len(d.Headers.Response), "header", "headers"))
			})
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&set, "set", nil, "response header 'Name: value' (repeatable)")
	f.StringArrayVar(&unset, "unset", nil, "remove a response header (repeatable)")
	f.BoolVar(&security, "security", false, "add the recommended security headers (HSTS, nosniff, frame options, referrer policy)")
	f.StringArrayVar(&origins, "cors-origin", nil, "CORS: allowed origin (repeatable)")
	f.StringArrayVar(&methods, "cors-method", nil, "CORS: allowed method (repeatable)")
	f.IntVar(&maxAge, "cors-max-age", 0, "CORS: preflight cache duration in seconds")
	f.BoolVar(&noCORS, "no-cors", false, "remove the CORS policy")
	return cmd
}

func newDistRestrictCmd(e *env) *cobra.Command {
	var (
		allow, deny       []string
		clearIP, noSecret bool
		secretHeader      string
		rate, burst       int
		per               string
		noRate            bool
	)
	cmd := &cobra.Command{
		Use:   "restrict ALIAS ID [--allow-ip CIDR]... [--deny-ip CIDR]... [--origin-secret-header NAME] [--rate-limit N [--burst N] [--rate-limit-per ip|distribution]]",
		Short: "Restrict the clients of a distribution (source IP, origin secret, rate limit)",
		Long: `Restrict the clients of a distribution: --allow-ip/--deny-ip filter the client
behind the trusted proxies; --origin-secret-header requires the origin secret of
the proxies in that header (generate the secret with "nexoctl dist origin-secret
rotate"); --rate-limit admits N requests per second on each node (--burst at
once), per client address or shared by the whole distribution
(--rate-limit-per): beyond it a website answers 429, an s3 distribution 503
SlowDown, with Retry-After; --no-rate-limit removes it.`,
		Example: `  nexoctl dist restrict local website --deny-ip 203.0.113.0/24
  nexoctl dist restrict local website --origin-secret-header X-Origin-Secret
  nexoctl dist restrict local website --rate-limit 50 --burst 100`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().NFlag() == 0 {
				return cli.UsageError("nothing to change: give at least one flag")
			}
			return e.editAndReport(cmd, args, "restricted", func(d *nexoadmin.Distribution) error {
				r := &d.Restrictions
				if clearIP {
					r.SourceIP = nil
				}
				if len(allow) > 0 || len(deny) > 0 {
					if r.SourceIP == nil {
						r.SourceIP = &nexoadmin.IPRestriction{}
					}
					r.SourceIP.Allow = append(r.SourceIP.Allow, splitList(allow)...)
					r.SourceIP.Deny = append(r.SourceIP.Deny, splitList(deny)...)
				}
				switch {
				case noSecret:
					r.OriginSecretHeader = nil
				case secretHeader != "":
					if r.OriginSecretHeader == nil {
						r.OriginSecretHeader = &nexoadmin.OriginSecret{}
					}
					r.OriginSecretHeader.Name = secretHeader
				}
				switch {
				case noRate:
					r.RateLimit = nil
				case cmd.Flags().Changed("rate-limit") || cmd.Flags().Changed("burst") || cmd.Flags().Changed("rate-limit-per"):
					if r.RateLimit == nil {
						r.RateLimit = &nexoadmin.RateLimit{Per: "ip"}
					}
					if cmd.Flags().Changed("rate-limit") {
						r.RateLimit.RequestsPerSecond = rate
					}
					if cmd.Flags().Changed("burst") {
						r.RateLimit.Burst = burst
					}
					if cmd.Flags().Changed("rate-limit-per") {
						r.RateLimit.Per = per
					}
				}
				return nil
			}, func(d *nexoadmin.Distribution) string { return fmt.Sprintf("Updated the restrictions of `%s`.", d.ID) })
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&allow, "allow-ip", nil, "allow only the clients of this CIDR (repeatable)")
	f.StringArrayVar(&deny, "deny-ip", nil, "refuse the clients of this CIDR (repeatable)")
	f.BoolVar(&clearIP, "clear-ip", false, "remove the source IP restrictions")
	f.StringVar(&secretHeader, "origin-secret-header", "", "require the origin secret in this header")
	f.BoolVar(&noSecret, "no-origin-secret", false, "remove the origin secret requirement")
	f.IntVar(&rate, "rate-limit", 0, "requests per second admitted by each node (0: no limit)")
	f.IntVar(&burst, "burst", 0, "requests admitted at once above the rate (0: the rate)")
	f.StringVar(&per, "rate-limit-per", "", "rate limit per client address (ip) or for the whole distribution (distribution)")
	f.BoolVar(&noRate, "no-rate-limit", false, "remove the rate limit")
	return cmd
}

func newDistOriginSecretRotateCmd(e *env) *cobra.Command {
	var in nexoadmin.RotateOriginSecretRequest
	cmd := &cobra.Command{
		Use:   "rotate ALIAS ID [--header NAME] [--keep N] [--drop-previous]",
		Short: "Generate a new origin secret (shown once), keeping the previous ones active",
		Long: `Generate a new origin secret for the proxies in front of the cluster: the
requests without one of the active secrets in the header are refused. The new
secret is printed once (the server stores it hashed); the previous one stays
active (--keep N, default 1) until the proxies are updated, then --drop-previous
on the next rotation.`,
		Example:           "  nexoctl dist origin-secret rotate local website",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, id, err := e.distAlias(ctx, args)
			if err != nil {
				return err
			}
			res, err := client.RotateOriginSecret(ctx, id, in)
			if err != nil {
				return fmt.Errorf("distribution %q: %w", id, err)
			}
			active := 0
			if s := res.Distribution.Restrictions.OriginSecretHeader; s != nil {
				active = len(s.Values)
			}
			rec := struct {
				Alias        string `json:"alias"`
				Distribution string `json:"distribution"`
				Header       string `json:"header"`
				Secret       string `json:"secret"`
				Active       int    `json:"active"`
			}{alias, id, res.Header, res.Secret, active}
			return e.out.Result(rec, fmt.Sprintf("New origin secret of `%s` (header %s, %d active, shown once):\n%s", id, res.Header, active, res.Secret))
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.Header, "header", "", "header of the secret (default: the current one, or X-Origin-Secret)")
	f.IntVar(&in.Keep, "keep", 0, "previous secrets kept active (default 1)")
	f.BoolVar(&in.DropPrevious, "drop-previous", false, "keep no previous secret")
	return cmd
}

func newDistTestCmd(e *env) *cobra.Command {
	var in nexoadmin.DistributionTestRequest
	cmd := &cobra.Command{
		Use:   "test ALIAS ID URL|PATH [--method M] [--ip IP]",
		Short: "Show how a distribution serves a URL: behavior, access, headers",
		Long: `Show how the distribution serves a URL without reading the object: the behavior
that matches (the first in order, or the default one) with its access, methods
and cache, the object of the path, a redirection or a refusal, and the headers
of the answer. A path is tested on https and the first exact domain.`,
		Example:           "  nexoctl dist test local website https://www.example.com/assets/app.js",
		Args:              exactArgs(3),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, id, err := e.distAlias(ctx, args)
			if err != nil {
				return err
			}
			in.URL = args[2]
			res, err := client.TestDistribution(ctx, id, in)
			if err != nil {
				return fmt.Errorf("distribution %q: %w", id, err)
			}
			return e.out.Result(distTestRecord{Alias: alias, Distribution: id, URL: args[2], DistributionTestResult: *res}, formatDistTest(res))
		},
	}
	cmd.Flags().StringVar(&in.Method, "method", "", "HTTP method (default GET)")
	cmd.Flags().StringVar(&in.ClientIP, "ip", "", "address of the client")
	return cmd
}

// formatDistTest renders how a URL is served.
func formatDistTest(res *nexoadmin.DistributionTestResult) string {
	behavior := "default"
	if res.BehaviorIndex >= 0 {
		behavior = fmt.Sprintf("%d (%s)", res.BehaviorIndex+1, formatPattern(res.Behavior.PathPattern))
	}
	route := fmt.Sprintf("Route: %s → %s", res.Route.Host, res.Route.Kind)
	if res.Route.Distribution != "" {
		route += fmt.Sprintf(" `%s` (alias %s)", res.Route.Distribution, res.Route.Alias)
	}
	lines := []string{
		route,
		"Behavior: " + behavior,
		"Access: " + orDash(res.Behavior.Access),
		"Settings: " + formatBehavior(res.Behavior),
	}
	if res.Key != "" {
		lines = append(lines, fmt.Sprintf("Object: %s/%s", res.Bucket, res.Key))
	}
	if res.RedirectStatus != 0 {
		lines = append(lines, fmt.Sprintf("Redirect: %d %s", res.RedirectStatus, res.RedirectLocation))
	}
	if res.RefusalStatus != 0 {
		lines = append(lines, fmt.Sprintf("Refused: %d %s", res.RefusalStatus, res.Refusal))
	}
	for _, k := range cli.SortedKeys(res.ResponseHeaders) {
		lines = append(lines, fmt.Sprintf("Header %s: %s", k, res.ResponseHeaders[k]))
	}
	return strings.Join(lines, "\n")
}

func newDistResolveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "resolve ALIAS HOST",
		Short:             "Show what serves a host (distribution, virtual host, S3 API)",
		Example:           "  nexoctl dist resolve local www.example.com",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			rt, err := client.ResolveHost(ctx, args[1])
			if err != nil {
				return err
			}
			text := fmt.Sprintf("%s: %s", rt.Host, rt.Kind)
			if rt.Distribution != "" {
				text += fmt.Sprintf(" `%s` (alias %s, %s)", rt.Distribution, rt.Alias, rt.Mode)
			}
			if rt.Bucket != "" {
				text += ", bucket " + rt.Bucket
			}
			return e.out.Result(distRouteRecord{Alias: alias, Route: *rt}, text)
		},
	}
}
