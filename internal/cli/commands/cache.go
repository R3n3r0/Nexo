package commands

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// newCacheCmd returns cache: the cache of the distributions (docs/11,
// C-CCH-01).
func newCacheCmd(e *env) *cobra.Command {
	cmd := groupCmd("cache", "Manage the cache of the distributions")
	cmd.Long = `Manage the HTTP cache of the distributions of a Nexo server (docs/11): the
reusable cache policies, the settings of the cluster, the invalidations (purge),
the warm-ups, the purge webhooks of the CDNs and proxies in front of the
cluster, the inspector of a URL and the statistics. The settings of a
distribution, of its domains and paths are "nexoctl dist cache".

The configuration needs admin:ManageCache; the invalidations and the warm-ups
need admin:InvalidateCache alone (a deploy pipeline).`
	policy := groupCmd("policy", "Manage the reusable cache policies")
	policy.AddCommand(newCachePolicyCreateCmd(e), newCachePolicyListCmd(e), newCachePolicyInfoCmd(e), newCachePolicyEditCmd(e),
		newCachePolicyRemoveCmd(e), newCachePolicyExportCmd(e), newCachePolicyImportCmd(e))
	webhook := groupCmd("webhook", "Manage the purge webhooks of a distribution")
	webhook.AddCommand(newCacheWebhookAddCmd(e), newCacheWebhookListCmd(e), newCacheWebhookRemoveCmd(e), newCacheWebhookTestCmd(e))
	cmd.AddCommand(policy, newCacheSettingsCmd(e), newCachePurgeCmd(e), newCacheInspectCmd(e), newCacheWarmCmd(e), newCacheStatsCmd(e), webhook)
	return cmd
}

// cachePollInterval is the interval of the --wait polls.
const cachePollInterval = 200 * time.Millisecond

// --- policies --------------------------------------------------------------------------

// cachePolicyRecord is a cache policy (cache policy info|create|edit).
type cachePolicyRecord struct {
	Alias  string                `json:"alias"`
	Action string                `json:"action,omitempty"`
	Policy nexoadmin.CachePolicy `json:"policy"`
	// CacheControl is the header the policy sends to the clients.
	CacheControl string `json:"cacheControl,omitempty"`
}

// policyFlags are the flags of a policy (create, edit).
type policyFlags struct {
	spec    specFlags
	comment string
	file    string
}

func (p *policyFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&p.comment, "comment", "", "free text")
	cmd.Flags().StringVar(&p.file, "file", "", "YAML or JSON document of the policy (the flags then change it)")
	p.spec.register(cmd)
}

// readPolicyDocument reads the policy of --file.
func (e *env) readPolicyDocument(path string) (nexoadmin.CachePolicy, error) {
	docs, err := e.readYAMLDocuments(path)
	if err != nil {
		return nexoadmin.CachePolicy{}, err
	}
	if len(docs) != 1 {
		return nexoadmin.CachePolicy{}, fmt.Errorf("%w: %s holds %d documents, expected one", domain.ErrInvalidArgument, path, len(docs))
	}
	var p nexoadmin.CachePolicy
	if err := decodeStrict(docs[0], &p); err != nil {
		return nexoadmin.CachePolicy{}, fmt.Errorf("%w: %s: %v", domain.ErrInvalidArgument, path, err)
	}
	return p, nil
}

// build returns the policy of the flags: the document of --file, or base,
// changed by the flags.
func (p *policyFlags) build(e *env, cmd *cobra.Command, base nexoadmin.CachePolicy) (nexoadmin.CachePolicy, bool, error) {
	changed := false
	if p.file != "" {
		doc, err := e.readPolicyDocument(p.file)
		if err != nil {
			return base, false, err
		}
		doc.Name, doc.ETag, doc.UpdatedAt, doc.Builtin = base.Name, base.ETag, nil, false
		base, changed = doc, true
	}
	if cmd.Flags().Changed("comment") {
		base.Comment, changed = p.comment, true
	}
	set, err := p.spec.apply(cmd, &base.CachePolicySpec)
	return base, changed || set, err
}

// cacheControlOf renders the Cache-Control a policy sends.
func (e *env) cacheControlOf(ctx context.Context, client cli.AdminClient, p nexoadmin.CachePolicy) string {
	prev, err := client.PreviewCachePolicy(ctx, p)
	if err != nil {
		return ""
	}
	if prev.Inherited {
		return "(the one of the objects)"
	}
	return prev.CacheControl
}

func newCachePolicyCreateCmd(e *env) *cobra.Command {
	var pf policyFlags
	cmd := &cobra.Command{
		Use:   "create ALIAS NAME [flags]",
		Short: "Define a cache policy",
		Long: `Define a reusable cache policy (docs/11 §11.2), referenced by name by the settings
of the cluster, of a distribution, of a domain or of a path. Its unset fields
take the values of the built-in policy "standard".`,
		Example: `  nexoctl cache policy create local long --ttl-default 1d --ttl-max 30d --query allowlist:v --stale-while-revalidate 5m
  nexoctl cache policy create local assets --file assets-policy.yaml`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, _, err := pf.build(e, cmd, nexoadmin.CachePolicy{Name: args[1]})
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			out, err := client.CreateCachePolicy(ctx, p)
			if err != nil {
				return fmt.Errorf("cache policy %q: %w", p.Name, err)
			}
			cc := e.cacheControlOf(ctx, client, *out)
			return e.out.Info(cachePolicyRecord{Alias: alias, Action: "created", Policy: *out, CacheControl: cc},
				fmt.Sprintf("Created the cache policy `%s` (Cache-Control %s).", out.Name, orDash(cc)))
		},
	}
	pf.register(cmd)
	return cmd
}

func newCachePolicyEditCmd(e *env) *cobra.Command {
	var pf policyFlags
	cmd := &cobra.Command{
		Use:   "edit ALIAS NAME [flags]",
		Short: "Change a defined cache policy",
		Long: `Change the fields of a defined cache policy given as flags (or replace it with the
document of --file). The entries cached under the policy are dropped. The change
is guarded by the ETag of the policy: a concurrent change is retried.`,
		Example:           "  nexoctl cache policy edit local long --ttl-max 7d --browser-cache-control 'public, max-age=3600'",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			var out *nexoadmin.CachePolicy
			for attempt := 1; ; attempt++ {
				cur, err := client.GetCachePolicy(ctx, args[1])
				if err != nil {
					return fmt.Errorf("cache policy %q: %w", args[1], err)
				}
				p, changed, err := pf.build(e, cmd, *cur)
				if err != nil {
					return err
				}
				if !changed {
					return cli.UsageError("nothing to change: give at least one flag")
				}
				p.UpdatedAt = nil
				out, err = client.UpdateCachePolicy(ctx, p, cur.ETag)
				if errors.Is(err, domain.ErrPreconditionFailed) && attempt < distAttempts {
					continue
				}
				if err != nil {
					return fmt.Errorf("cache policy %q: %w", args[1], err)
				}
				break
			}
			cc := e.cacheControlOf(ctx, client, *out)
			return e.out.Info(cachePolicyRecord{Alias: alias, Action: "edited", Policy: *out, CacheControl: cc},
				fmt.Sprintf("Changed the cache policy `%s` (Cache-Control %s).", out.Name, orDash(cc)))
		},
	}
	pf.register(cmd)
	return cmd
}

func newCachePolicyListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS",
		Aliases:           []string{"ls"},
		Short:             "List the cache policies, built-in and defined",
		Example:           "  nexoctl cache policy ls local",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			list, err := client.ListCachePolicies(ctx)
			if err != nil {
				return err
			}
			if err := e.out.Text(fmt.Sprintf("%-20s %-8s %-10s %-10s %s", "NAME", "TYPE", "TTL", "TTL MAX", "COMMENT")); err != nil {
				return err
			}
			for _, p := range list {
				kind := "defined"
				if p.Builtin {
					kind = "built-in"
				}
				def, maxTTL := "-", "-"
				if p.TTL != nil {
					def, maxTTL = orDash(p.TTL.Default), orDash(p.TTL.Max)
				}
				text := fmt.Sprintf("%-20s %-8s %-10s %-10s %s", p.Name, kind, def, maxTTL, p.Comment)
				if err := e.out.Result(cachePolicyRecord{Alias: alias, Policy: p}, strings.TrimRight(text, " ")); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newCachePolicyInfoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "info ALIAS NAME",
		Short:             "Show a cache policy and the Cache-Control it sends",
		Example:           "  nexoctl cache policy info local aggressive",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			p, err := client.GetCachePolicy(ctx, args[1])
			if err != nil {
				return fmt.Errorf("cache policy %q: %w", args[1], err)
			}
			cc := e.cacheControlOf(ctx, client, *p)
			lines := []string{"Name: " + p.Name, "Type: " + map[bool]string{true: "built-in", false: "defined"}[p.Builtin]}
			if p.Comment != "" {
				lines = append(lines, "Comment: "+p.Comment)
			}
			lines = append(lines, "Cache-Control: "+orDash(cc))
			for _, f := range specFields(p.CachePolicySpec) {
				lines = append(lines, capitalize(f.name)+": "+f.value)
			}
			return e.out.Result(cachePolicyRecord{Alias: alias, Policy: *p, CacheControl: cc}, strings.Join(lines, "\n"))
		},
	}
}

func newCachePolicyRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "remove ALIAS NAME",
		Aliases:           []string{"rm"},
		Short:             "Remove a defined cache policy no setting names",
		Example:           "  nexoctl cache policy rm local long",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.DeleteCachePolicy(ctx, args[1]); err != nil {
				return fmt.Errorf("cache policy %q: %w", args[1], err)
			}
			return e.out.Info(cachePolicyRecord{Alias: alias, Action: "removed", Policy: nexoadmin.CachePolicy{Name: args[1]}},
				fmt.Sprintf("Removed the cache policy `%s`.", args[1]))
		},
	}
}

// policyExportDocument is a policy without the members the server
// computes, as JSON.
func policyExportDocument(p nexoadmin.CachePolicy) ([]byte, error) {
	p.ETag, p.UpdatedAt, p.Builtin = "", nil, false
	return json.Marshal(p)
}

// cacheExportRecord reports an export to a file.
type cacheExportRecord struct {
	Alias     string `json:"alias"`
	File      string `json:"file"`
	Documents int    `json:"documents"`
}

// writeYAMLDocuments writes JSON documents as YAML to output ("" or "-":
// stdout).
func (e *env) writeYAMLDocuments(alias, output string, docs [][]byte, what string) error {
	var out []byte
	for i, doc := range docs {
		y, err := cli.JSONToYAML(doc)
		if err != nil {
			return err
		}
		if i > 0 {
			out = append(out, "---\n"...)
		}
		out = append(out, y...)
	}
	if output == "" || output == "-" {
		_, err := e.out.Stdout().Write(out)
		return err
	}
	if err := os.WriteFile(output, out, 0o644); err != nil {
		return err
	}
	return e.out.Info(cacheExportRecord{Alias: alias, File: output, Documents: len(docs)},
		fmt.Sprintf("Exported %d %s to %s.", len(docs), what, output))
}

func newCachePolicyExportCmd(e *env) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "export ALIAS [NAME] [-o FILE]",
		Short: "Export the defined cache policies as YAML",
		Long: `Export a defined cache policy (every defined policy without NAME, one YAML
document each): the documents of "nexoctl cache policy import".`,
		Example:           "  nexoctl cache policy export local -o policies.yaml",
		Args:              func(cmd *cobra.Command, args []string) error { return argsBetween(cmd, args, 1, 2) },
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			var list []nexoadmin.CachePolicy
			if len(args) == 2 {
				p, err := client.GetCachePolicy(ctx, args[1])
				if err != nil {
					return fmt.Errorf("cache policy %q: %w", args[1], err)
				}
				list = []nexoadmin.CachePolicy{*p}
			} else {
				all, err := client.ListCachePolicies(ctx)
				if err != nil {
					return err
				}
				for _, p := range all {
					if !p.Builtin {
						list = append(list, p)
					}
				}
			}
			if (output == "" || output == "-") && e.out.JSON() {
				for _, p := range list {
					if err := e.out.Result(cachePolicyRecord{Alias: alias, Policy: p}, ""); err != nil {
						return err
					}
				}
				return nil
			}
			docs := make([][]byte, 0, len(list))
			for _, p := range list {
				doc, err := policyExportDocument(p)
				if err != nil {
					return err
				}
				docs = append(docs, doc)
			}
			return e.writeYAMLDocuments(alias, output, docs, plural(len(docs), "cache policy", "cache policies"))
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "write the documents to this file")
	return cmd
}

func newCachePolicyImportCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "import ALIAS FILE",
		Short:             "Create or replace the cache policies of a YAML file",
		Example:           "  nexoctl cache policy import local policies.yaml",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			docs, err := e.readYAMLDocuments(args[1])
			if err != nil {
				return err
			}
			list := make([]nexoadmin.CachePolicy, 0, len(docs))
			for i, doc := range docs {
				var p nexoadmin.CachePolicy
				if err := decodeStrict(doc, &p); err != nil {
					return fmt.Errorf("%w: %s: document %d: %v", domain.ErrInvalidArgument, args[1], i+1, err)
				}
				if p.Name == "" {
					return fmt.Errorf("%w: %s: document %d has no name", domain.ErrInvalidArgument, args[1], i+1)
				}
				p.ETag, p.UpdatedAt, p.Builtin = "", nil, false
				list = append(list, p)
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			for _, p := range list {
				action := "updated"
				cur, err := client.GetCachePolicy(ctx, p.Name)
				var out *nexoadmin.CachePolicy
				switch {
				case err == nil:
					out, err = client.UpdateCachePolicy(ctx, p, cur.ETag)
				case errors.Is(err, domain.ErrNotFound):
					action = "created"
					out, err = client.CreateCachePolicy(ctx, p)
				}
				if err != nil {
					return fmt.Errorf("cache policy %q: %w", p.Name, err)
				}
				if err := e.out.Info(cachePolicyRecord{Alias: alias, Action: action, Policy: *out}, fmt.Sprintf("Cache policy `%s` %s.", p.Name, action)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// --- settings of the cluster -----------------------------------------------------------

// cacheSettingsRecord are the cache settings of the cluster.
type cacheSettingsRecord struct {
	Alias    string                        `json:"alias"`
	Action   string                        `json:"action,omitempty"`
	Settings nexoadmin.CacheGlobalSettings `json:"settings"`
}

func formatGlobalSettings(g *nexoadmin.CacheGlobalSettings) string {
	lines := []string{"Cache: " + formatSettings(g.Cache), "Node header: " + onOff(g.NodeHeader)}
	if d := g.Debug; d != nil {
		header := d.Header
		if header == "" {
			header = "X-Nexo-Cache-Debug"
		}
		lines = append(lines, fmt.Sprintf("Debug header: %s (secret %s)", header, orDash(d.Secret)))
	}
	return strings.Join(lines, "\n")
}

func newCacheSettingsCmd(e *env) *cobra.Command {
	sf := settingsFlags{withAutoInvalidate: true}
	var (
		nodeHeader                          bool
		debugHeader, debugSecret, debugFile string
		noDebug                             bool
	)
	cmd := &cobra.Command{
		Use:   "settings ALIAS [flags]",
		Short: "Show or change the cache settings of the cluster",
		Long: `Show the cache settings of the cluster (the global level the distributions,
their domains and their paths inherit from), or change the fields given as
flags.`,
		Example: `  nexoctl cache settings local
  nexoctl cache settings local --policy standard --node-header
  nexoctl cache settings local --debug-secret-file debug.secret`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			secret, err := readSecretFlag(cmd, debugSecret, debugFile, "debug-secret")
			if err != nil {
				return err
			}
			var out *nexoadmin.CacheGlobalSettings
			for attempt := 1; ; attempt++ {
				g, err := client.GetCacheSettings(ctx)
				if err != nil {
					return err
				}
				changed, err := sf.apply(cmd, &g.Cache)
				if err != nil {
					return err
				}
				f := cmd.Flags()
				if f.Changed("node-header") {
					g.NodeHeader, changed = nodeHeader, true
				}
				if f.Changed("debug-header") || f.Changed("debug-secret") || f.Changed("debug-secret-file") {
					if g.Debug == nil {
						g.Debug = &nexoadmin.CacheDebugHeader{}
					}
					if f.Changed("debug-header") {
						g.Debug.Header = debugHeader
					}
					if f.Changed("debug-secret") || f.Changed("debug-secret-file") {
						g.Debug.Secret = secret
					}
					changed = true
				}
				if noDebug {
					g.Debug, changed = nil, true
				}
				if !changed {
					// No setting given: show them.
					return e.out.Result(cacheSettingsRecord{Alias: alias, Settings: *g}, formatGlobalSettings(g))
				}
				etag := g.ETag
				g.UpdatedAt = nil
				out, err = client.SetCacheSettings(ctx, *g, etag)
				if errors.Is(err, domain.ErrPreconditionFailed) && attempt < distAttempts {
					continue
				}
				if err != nil {
					return err
				}
				break
			}
			return e.out.Info(cacheSettingsRecord{Alias: alias, Action: "changed", Settings: *out},
				"Changed the cache settings of the cluster.\n"+formatGlobalSettings(out))
		},
	}
	sf.register(cmd)
	f := cmd.Flags()
	f.BoolVar(&nodeHeader, "node-header", false, "add X-Cache-Node to every answer")
	f.StringVar(&debugHeader, "debug-header", "", "request header unlocking X-Cache-Key and X-Cache-Policy (default X-Nexo-Cache-Debug)")
	f.StringVar(&debugSecret, "debug-secret", "", "value of the debug header (or ${secret:NAME})")
	f.StringVar(&debugFile, "debug-secret-file", "", "file holding the value of the debug header")
	f.BoolVar(&noDebug, "no-debug", false, "remove the debug header")
	return cmd
}

// --- invalidations ---------------------------------------------------------------------

// cacheInvalidationRecord is an invalidation (cache purge, dist
// invalidate, purge status).
type cacheInvalidationRecord struct {
	Alias        string                      `json:"alias"`
	Invalidation nexoadmin.CacheInvalidation `json:"invalidation"`
}

// purgeFlags are the flags of an invalidation.
type purgeFlags struct {
	domain           string
	tags             []string
	prefixes, regexs []string
	all, soft, wait  bool
	timeout          time.Duration
}

func (p *purgeFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&p.domain, "domain", "", "limit the invalidation to a domain of the distribution (alone: the whole domain)")
	f.StringArrayVar(&p.tags, "tag", nil, "cache tag (x-amz-meta-cache-tag) to invalidate (repeatable)")
	f.StringArrayVar(&p.prefixes, "prefix", nil, "path prefix to invalidate (repeatable)")
	f.StringArrayVar(&p.regexs, "regex", nil, "RE2 expression of the paths to invalidate (repeatable)")
	f.BoolVar(&p.all, "all", false, "invalidate everything (of the domain, of the distribution, or of every distribution)")
	f.BoolVar(&p.soft, "soft", false, "mark the entries expired (served stale while revalidated) instead of dropping them")
	f.BoolVar(&p.wait, "wait", false, "wait until every node applied the invalidation")
	f.DurationVar(&p.timeout, "timeout", time.Minute, "how long --wait waits")
}

// hasGlob reports a glob metacharacter.
func hasGlob(s string) bool { return strings.ContainsAny(s, "*?[{") }

// request builds the invalidation of the path arguments: absolute URLs and
// exact paths, the others (with a glob metacharacter) as glob patterns.
func (p *purgeFlags) request(targets []string) (nexoadmin.CacheInvalidationRequest, error) {
	req := nexoadmin.CacheInvalidationRequest{Domain: p.domain, Tags: splitList(p.tags), All: p.all, Soft: p.soft}
	for _, t := range targets {
		lower := strings.ToLower(t)
		switch {
		case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
			req.Paths = append(req.Paths, t)
		case !strings.HasPrefix(t, "/"):
			return req, cli.UsageError("%q: a path starts with / (or give a URL)", t)
		case hasGlob(t):
			req.Patterns = append(req.Patterns, nexoadmin.KeyPattern{Type: nexoadmin.PatternGlob, Pattern: t})
		default:
			req.Paths = append(req.Paths, t)
		}
	}
	for _, v := range p.prefixes {
		req.Patterns = append(req.Patterns, nexoadmin.KeyPattern{Type: nexoadmin.PatternPrefix, Pattern: v})
	}
	for _, v := range p.regexs {
		req.Patterns = append(req.Patterns, nexoadmin.KeyPattern{Type: nexoadmin.PatternRegex, Pattern: v})
	}
	if len(req.Paths)+len(req.Patterns)+len(req.Tags) == 0 && !req.All && req.Domain == "" {
		return req, cli.UsageError("nothing to invalidate: give paths, patterns (--prefix, --regex), --tag, --domain or --all")
	}
	return req, nil
}

// completedNodes counts the nodes that applied an invalidation.
func completedNodes(inv *nexoadmin.CacheInvalidation) int {
	n := 0
	for _, node := range inv.Nodes {
		if node.State == nexoadmin.NodeCompleted {
			n++
		}
	}
	return n
}

// removedEntries sums the entries removed on the nodes.
func removedEntries(inv *nexoadmin.CacheInvalidation) int {
	n := 0
	for _, node := range inv.Nodes {
		n += node.Removed
	}
	return n
}

// waitInvalidation polls an invalidation until every node applied it.
func waitInvalidation(ctx context.Context, client cli.AdminClient, inv *nexoadmin.CacheInvalidation, timeout time.Duration) (*nexoadmin.CacheInvalidation, error) {
	deadline := time.Now().Add(timeout)
	for inv.State != nexoadmin.InvalidationCompleted {
		if time.Now().After(deadline) {
			return inv, fmt.Errorf("invalidation %s still in progress on %d of %d nodes (nexoctl cache purge status): %w",
				inv.ID, len(inv.Nodes)-completedNodes(inv), len(inv.Nodes), context.DeadlineExceeded)
		}
		t := time.NewTimer(cachePollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return inv, fmt.Errorf("invalidation %s still in progress (nexoctl cache purge status): %w", inv.ID, ctx.Err())
		case <-t.C:
		}
		next, err := client.GetCacheInvalidation(ctx, inv.Request.Distribution, inv.ID)
		if err != nil {
			return inv, err
		}
		inv = next
	}
	return inv, nil
}

// scopeOf renders what an invalidation covers.
func scopeOf(r nexoadmin.CacheInvalidationRecord) string {
	var parts []string
	if r.Distribution == "" {
		parts = append(parts, "every distribution")
	} else {
		parts = append(parts, "distribution "+r.Distribution)
	}
	if r.Domain != "" {
		parts = append(parts, "domain "+r.Domain)
	}
	if len(r.Paths) > 0 {
		parts = append(parts, plural(len(r.Paths), "path ", "paths ")+strings.Join(r.Paths, " "))
	}
	for _, p := range r.Patterns {
		parts = append(parts, formatPattern(&p))
	}
	if len(r.Tags) > 0 {
		parts = append(parts, plural(len(r.Tags), "tag ", "tags ")+strings.Join(r.Tags, " "))
	}
	if r.All {
		parts = append(parts, "everything")
	}
	if r.Soft {
		parts = append(parts, "soft")
	}
	return strings.Join(parts, ", ")
}

// formatInvalidation renders an invalidation with its state per node.
func formatInvalidation(inv *nexoadmin.CacheInvalidation) string {
	lines := []string{
		"Invalidation: " + inv.ID,
		"Scope: " + scopeOf(inv.Request),
		fmt.Sprintf("Requested: %s by %s", cli.FormatTime(inv.CreatedAt), orDash(inv.Request.Actor)),
		fmt.Sprintf("State: %s (%d/%d nodes, %d %s removed)", inv.State, completedNodes(inv), len(inv.Nodes), removedEntries(inv),
			plural(removedEntries(inv), "entry", "entries")),
	}
	for _, n := range inv.Nodes {
		line := fmt.Sprintf("Node %s: %s", n.Node, n.State)
		if n.State == nexoadmin.NodeCompleted {
			line += fmt.Sprintf(", %d removed", n.Removed)
		}
		if n.Error != "" {
			line += " (" + n.Error + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// purge runs an invalidation and prints it.
func (e *env) purge(cmd *cobra.Command, alias string, client cli.AdminClient, dist string, req nexoadmin.CacheInvalidationRequest, pf *purgeFlags) error {
	ctx := cmd.Context()
	inv, err := client.InvalidateCache(ctx, dist, req)
	if err != nil {
		if dist != "" {
			return fmt.Errorf("distribution %q: %w", dist, err)
		}
		return err
	}
	var waitErr error
	if pf.wait {
		inv, waitErr = waitInvalidation(ctx, client, inv, pf.timeout)
	}
	text := fmt.Sprintf("Invalidation %s (%s): %s on %d/%d %s, %d %s removed.", inv.ID, scopeOf(inv.Request), inv.State,
		completedNodes(inv), len(inv.Nodes), plural(len(inv.Nodes), "node", "nodes"), removedEntries(inv), plural(removedEntries(inv), "entry", "entries"))
	if err := e.out.Info(cacheInvalidationRecord{Alias: alias, Invalidation: *inv}, text); err != nil {
		return err
	}
	return waitErr
}

func newCachePurgeCmd(e *env) *cobra.Command {
	var pf purgeFlags
	cmd := &cobra.Command{
		Use:   "purge ALIAS [DIST] [PATH|GLOB|URL...] [flags]",
		Short: "Invalidate cached content",
		Long: `Invalidate the entries of a distribution on every node (docs/11 §11.5): exact
paths (or absolute URLs of a domain), glob patterns of paths ("/assets/**"),
prefixes (--prefix), regular expressions (--regex), cache tags (--tag), a whole
domain (--domain alone) or everything (--all). Without DIST, --all purges every
distribution. --soft marks the entries expired instead of dropping them. The
purge webhooks of the domains are called too.

"purge status" follows an invalidation, "purge list" lists the history.`,
		Example: `  nexoctl cache purge local sito-web --domain www.example.com '/assets/**' --soft
  nexoctl cache purge local sito-web --tag prodotto-42
  nexoctl cache purge local sito-web --domain static.example.it --all
  nexoctl cache purge local sito-web https://www.example.com/index.html --wait
  nexoctl cache purge local --all`,
		Args:              minArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			dist, targets := "", []string(nil)
			if len(args) > 1 {
				dist, targets = args[1], args[2:]
				if strings.HasPrefix(dist, "/") || strings.Contains(dist, "://") {
					return cli.UsageError("%q: give the distribution before the paths (nexoctl cache purge ALIAS DIST PATH...)", dist)
				}
			}
			req, err := pf.request(targets)
			if err != nil {
				return err
			}
			if dist == "" && (!req.All || req.Domain != "" || len(req.Tags)+len(req.Patterns)+len(req.Paths) > 0) {
				return cli.UsageError("without a distribution only --all (every distribution) is allowed")
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			return e.purge(cmd, alias, client, dist, req, &pf)
		},
	}
	pf.register(cmd)
	cmd.AddCommand(newCachePurgeStatusCmd(e), newCachePurgeListCmd(e))
	return cmd
}

func newCachePurgeStatusCmd(e *env) *cobra.Command {
	var (
		dist    string
		wait    bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:               "status ALIAS ID [--dist DIST] [--wait]",
		Short:             "Show the state of an invalidation on every node",
		Example:           "  nexoctl cache purge status local inv-18a2b3c4d5e6f-0a1b2c3d",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			inv, err := client.GetCacheInvalidation(ctx, dist, args[1])
			if err != nil {
				return fmt.Errorf("invalidation %q: %w", args[1], err)
			}
			var waitErr error
			if wait {
				inv, waitErr = waitInvalidation(ctx, client, inv, timeout)
			}
			if err := e.out.Result(cacheInvalidationRecord{Alias: alias, Invalidation: *inv}, formatInvalidation(inv)); err != nil {
				return err
			}
			return waitErr
		},
	}
	f := cmd.Flags()
	f.StringVar(&dist, "dist", "", "distribution of the invalidation (default: found by its id)")
	f.BoolVar(&wait, "wait", false, "wait until every node applied the invalidation")
	f.DurationVar(&timeout, "timeout", time.Minute, "how long --wait waits")
	return cmd
}

func newCachePurgeListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS [DIST]",
		Aliases:           []string{"ls"},
		Short:             "List the invalidations of a distribution (the global purges without DIST)",
		Example:           "  nexoctl cache purge ls local sito-web",
		Args:              func(cmd *cobra.Command, args []string) error { return argsBetween(cmd, args, 1, 2) },
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			dist := ""
			if len(args) == 2 {
				dist = args[1]
			}
			list, err := client.ListCacheInvalidations(ctx, dist)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				return e.out.Note("No invalidation.")
			}
			for _, inv := range list {
				text := fmt.Sprintf("%s  %s  %-11s %d/%d  %s", inv.ID, cli.FormatTime(inv.CreatedAt), inv.State, completedNodes(&inv), len(inv.Nodes), scopeOf(inv.Request))
				if err := e.out.Result(cacheInvalidationRecord{Alias: alias, Invalidation: inv}, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// --- inspector -------------------------------------------------------------------------

// cacheInspectRecord is the inspection of a URL.
type cacheInspectRecord struct {
	Alias      string                    `json:"alias"`
	Inspection nexoadmin.CacheInspection `json:"inspection"`
}

// levelOf renders a value with the level it comes from ("short
// (domain)").
func levelOf(v, level string) string {
	if level == "" {
		return v
	}
	return v + " (" + level + ")"
}

// formatInspection renders an inspection.
func formatInspection(in *nexoadmin.CacheInspection) string {
	lines := []string{"URL: " + in.URL}
	if in.Distribution == "" {
		lines = append(lines, "Distribution: - ("+orDash(in.Reason)+")")
		return strings.Join(lines, "\n")
	}
	lines = append(lines, fmt.Sprintf("Distribution: %s (host %s, mode %s)", in.Distribution, in.Host, in.Mode))
	if in.Key != "" {
		lines = append(lines, fmt.Sprintf("Object: %s/%s", in.Bucket, in.Key))
	}
	lines = append(lines, "Cache key: "+orDash(in.CacheKey))
	p := in.Policy
	state := "enabled"
	if !p.Enabled {
		state = "disabled"
	}
	lines = append(lines, "Cache: "+levelOf(state, p.EnabledLevel))
	if in.Bypass {
		lines = append(lines, "Bypass: "+orDash(in.Reason))
	}
	lines = append(lines, "Policy: "+levelOf(p.PolicyName, p.PolicyLevel))
	if t := p.Policy.TTL; t != nil {
		lines = append(lines, fmt.Sprintf("TTL: default %s (%s), min %s, max %s", t.Default, p.Fields["ttl.default"], t.Min, t.Max))
	}
	cc := p.CacheControl
	if cc == "" {
		cc = "(the one of the object)"
	}
	lines = append(lines, "Cache-Control: "+cc)
	for _, n := range in.Nodes {
		line := "Node " + n.Node + ": "
		switch {
		case !n.Online:
			line += "offline"
		case n.Error != "":
			line += "error: " + n.Error
		case !n.Entry.Present:
			line += "not cached"
		default:
			e := n.Entry
			line += fmt.Sprintf("cached in %s (%s), age %ds, ", e.Tier, e.Kind, e.AgeSeconds)
			if e.Stale {
				line += fmt.Sprintf("stale for %ds", -e.ExpiresInSeconds)
			} else {
				line += fmt.Sprintf("fresh for %ds", e.ExpiresInSeconds)
			}
			line += ", " + cli.HumanSize(e.SizeBytes)
			if len(e.Encodings) > 0 {
				line += ", variants " + strings.Join(e.Encodings, ",")
			}
		}
		if n.Owner {
			line += " [owner]"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func newCacheInspectCmd(e *env) *cobra.Command {
	var (
		dist    string
		headers []string
	)
	cmd := &cobra.Command{
		Use:   "inspect ALIAS URL [--dist DIST] [--header 'Name: value']...",
		Short: "Show how the cache serves a URL",
		Long: `Show the cache key of a URL, the effective cache policy with the level each
setting comes from, why the cache would be bypassed, and the entry on every
node (tier, age, freshness, size). With --dist, URL may be a path of the
distribution (completed with its first domain).`,
		Example: `  nexoctl cache inspect local https://www.example.com/assets/app.js
  nexoctl cache inspect local /assets/app.js --dist sito-web --header 'Accept-Encoding: br'`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := parseHeaderFlags("header", headers); err != nil {
				return err
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			in, err := client.InspectCache(ctx, nexoadmin.CacheInspectRequest{URL: args[1], Distribution: dist, Headers: headers})
			if err != nil {
				return fmt.Errorf("%s: %w", args[1], err)
			}
			return e.out.Result(cacheInspectRecord{Alias: alias, Inspection: *in}, formatInspection(in))
		},
	}
	cmd.Flags().StringVar(&dist, "dist", "", "distribution of the URL (a path is completed with its first domain)")
	cmd.Flags().StringArrayVar(&headers, "header", nil, "request header of the cache key 'Name: value' (repeatable)")
	return cmd
}

// --- warm-up -------------------------------------------------------------------------

// cacheWarmRecord is a warm-up.
type cacheWarmRecord struct {
	Alias string                 `json:"alias"`
	Job   nexoadmin.CacheWarmJob `json:"job"`
}

// readWarmList reads a list of URLs or paths, one per line ("#"
// comments).
func (e *env) readWarmList(path string, req *nexoadmin.CacheWarmRequest) error {
	data, err := e.readDocument(path)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := addWarmTarget(req, line); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return sc.Err()
}

// addWarmTarget adds a URL or a path to a warm-up.
func addWarmTarget(req *nexoadmin.CacheWarmRequest, t string) error {
	lower := strings.ToLower(t)
	switch {
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		req.URLs = append(req.URLs, t)
	case strings.HasPrefix(t, "/"):
		req.Paths = append(req.Paths, t)
	default:
		return cli.UsageError("%q: a path starts with / (or give a URL)", t)
	}
	return nil
}

// warmDone counts the objects warmed and failed on every node.
func warmDone(job *nexoadmin.CacheWarmJob) (total, done, failed int, bytes int64) {
	for _, n := range job.Nodes {
		total, done, failed, bytes = total+n.Total, done+n.Done, failed+n.Failed, bytes+n.Bytes
	}
	return total, done, failed, bytes
}

// waitWarm polls a warm-up until it finishes.
func waitWarm(ctx context.Context, client cli.AdminClient, job *nexoadmin.CacheWarmJob, timeout time.Duration) (*nexoadmin.CacheWarmJob, error) {
	deadline := time.Now().Add(timeout)
	for job.State == nexoadmin.WarmRunning {
		if time.Now().After(deadline) {
			return job, fmt.Errorf("warm-up %s still running (nexoctl cache warm status): %w", job.ID, context.DeadlineExceeded)
		}
		t := time.NewTimer(cachePollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return job, fmt.Errorf("warm-up %s still running (nexoctl cache warm status): %w", job.ID, ctx.Err())
		case <-t.C:
		}
		next, err := client.GetCacheWarmJob(ctx, job.Request.Distribution, job.ID)
		if err != nil {
			return job, err
		}
		job = next
	}
	return job, nil
}

// formatWarm renders a warm-up.
func formatWarm(job *nexoadmin.CacheWarmJob) string {
	total, done, failed, bytes := warmDone(job)
	lines := []string{
		"Warm-up: " + job.ID,
		fmt.Sprintf("Distribution: %s (domain %s)", job.Request.Distribution, orDash(job.Request.Domain)),
		fmt.Sprintf("State: %s (%d/%d objects, %d failed, %s)", job.State, done, total, failed, cli.HumanSize(bytes)),
	}
	for _, n := range job.Nodes {
		line := fmt.Sprintf("Node %s: %s, %d/%d objects", n.Node, n.State, n.Done, n.Total)
		if n.Failed > 0 {
			line += fmt.Sprintf(", %d failed", n.Failed)
		}
		if n.Error != "" {
			line += " (" + n.Error + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// warmResult prints a warm-up; a failed one is an error after the report.
func (e *env) warmResult(alias string, job *nexoadmin.CacheWarmJob, waitErr error, info bool) error {
	emit := e.out.Result
	text := formatWarm(job)
	if info {
		emit = e.out.Info
		total, done, _, _ := warmDone(job)
		text = fmt.Sprintf("Warm-up %s of `%s`: %s, %d/%d objects.", job.ID, job.Request.Distribution, job.State, done, total)
	}
	if err := emit(cacheWarmRecord{Alias: alias, Job: *job}, text); err != nil {
		return err
	}
	if waitErr != nil {
		return waitErr
	}
	if job.State == nexoadmin.WarmFailed {
		return fmt.Errorf("warm-up %s failed", job.ID)
	}
	return nil
}

func newCacheWarmCmd(e *env) *cobra.Command {
	var (
		domainName, from, prefix, glob, regex, bandwidth string
		concurrency                                      int
		wait                                             bool
		timeout                                          time.Duration
	)
	cmd := &cobra.Command{
		Use:   "warm ALIAS DIST [PATH|URL...] [--from FILE] [--prefix P] [flags]",
		Short: "Cache objects of a domain before they are requested",
		Long: `Cache objects of a domain of a distribution before they are requested (docs/11
§11.6), e.g. after the deploy of a static site: paths and URLs given as
arguments or listed in a file (--from, one per line), every object below a path
prefix (--prefix) or matching a pattern (--glob, --regex), with bounded
concurrency and bandwidth. In the local mode every node warms its own cache.`,
		Example: `  nexoctl cache warm local sito-web --domain www.example.com --from urls.txt --wait
  nexoctl cache warm local sito-web --prefix /assets/ --bandwidth 20MiB`,
		Args:              minArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := nexoadmin.CacheWarmRequest{Domain: domainName, Prefix: prefix, Concurrency: concurrency, Bandwidth: bandwidth}
			for _, t := range args[2:] {
				if err := addWarmTarget(&req, t); err != nil {
					return err
				}
			}
			if from != "" {
				if err := e.readWarmList(from, &req); err != nil {
					return err
				}
			}
			switch {
			case glob != "" && regex != "":
				return cli.UsageError("--glob and --regex are mutually exclusive")
			case glob != "":
				req.Pattern = &nexoadmin.KeyPattern{Type: nexoadmin.PatternGlob, Pattern: glob}
			case regex != "":
				req.Pattern = &nexoadmin.KeyPattern{Type: nexoadmin.PatternRegex, Pattern: regex}
			}
			if len(req.Paths)+len(req.URLs) == 0 && req.Prefix == "" && req.Pattern == nil {
				return cli.UsageError("nothing to warm: give paths, URLs, --from, --prefix, --glob or --regex")
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			job, err := client.WarmCache(ctx, args[1], req)
			if err != nil {
				return fmt.Errorf("distribution %q: %w", args[1], err)
			}
			var waitErr error
			if wait {
				job, waitErr = waitWarm(ctx, client, job, timeout)
			}
			return e.warmResult(alias, job, waitErr, true)
		},
	}
	f := cmd.Flags()
	f.StringVar(&domainName, "domain", "", "domain the entries are cached for (default the first domain of the distribution)")
	f.StringVar(&from, "from", "", "file of URLs or paths to warm, one per line (\"-\": stdin)")
	f.StringVar(&prefix, "prefix", "", "warm every object below this path prefix")
	f.StringVar(&glob, "glob", "", "warm every object whose path matches this glob")
	f.StringVar(&regex, "regex", "", "warm every object whose path matches this RE2 expression")
	f.IntVar(&concurrency, "concurrency", 0, "parallel reads (default 4)")
	f.StringVar(&bandwidth, "bandwidth", "", "bytes read per second at most (default 64MiB)")
	f.BoolVar(&wait, "wait", false, "wait until the warm-up finishes")
	f.DurationVar(&timeout, "timeout", 10*time.Minute, "how long --wait waits")
	cmd.AddCommand(newCacheWarmStatusCmd(e))
	return cmd
}

func newCacheWarmStatusCmd(e *env) *cobra.Command {
	var (
		wait    bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:               "status ALIAS DIST JOB [--wait]",
		Short:             "Show the progress of a warm-up on every node",
		Example:           "  nexoctl cache warm status local sito-web warm-18a2b3c4d5e6f-0a1b2c3d",
		Args:              exactArgs(3),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			job, err := client.GetCacheWarmJob(ctx, args[1], args[2])
			if err != nil {
				return fmt.Errorf("warm-up %q: %w", args[2], err)
			}
			var waitErr error
			if wait {
				job, waitErr = waitWarm(ctx, client, job, timeout)
			}
			return e.warmResult(alias, job, waitErr, false)
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the warm-up finishes")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long --wait waits")
	return cmd
}

// --- statistics ------------------------------------------------------------------------

// cacheStatsRecord are the statistics of the cache.
type cacheStatsRecord struct {
	Alias string               `json:"alias"`
	At    time.Time            `json:"at"`
	Stats nexoadmin.CacheStats `json:"stats"`
}

// formatCounters renders the counters of the answers.
func formatCounters(c nexoadmin.CacheCounters) string {
	var total int64
	var parts []string
	for _, r := range []string{nexoadmin.XCacheHit, nexoadmin.XCacheMiss, nexoadmin.XCacheStale, nexoadmin.XCacheBypass,
		nexoadmin.XCacheExpired, nexoadmin.XCacheRevalidated} {
		if n := c.Requests[r]; n > 0 {
			total += n
			parts = append(parts, fmt.Sprintf("%s %d", r, n))
		}
	}
	detail := ""
	if len(parts) > 0 {
		detail = " (" + strings.Join(parts, ", ") + ")"
	}
	return fmt.Sprintf("%d %s%s, hit ratio %.1f%%", total, plural(int(total), "request", "requests"), detail, c.HitRatio*100)
}

// formatStats renders the statistics.
func formatStats(at time.Time, s *nexoadmin.CacheStats) string {
	lines := []string{
		"At: " + cli.FormatTime(at),
		"Requests: " + formatCounters(s.CacheCounters),
		fmt.Sprintf("Bytes: %s from the cache, %s from the origin", cli.HumanSize(s.BytesCache), cli.HumanSize(s.BytesOrigin)),
		fmt.Sprintf("Entries: %d (memory %s, disk %s)", s.Entries, cli.HumanSize(s.MemoryBytes), cli.HumanSize(s.DiskBytes)),
	}
	for _, h := range s.Hosts {
		lines = append(lines, fmt.Sprintf("Domain %s (%s): %s, %d %s", h.Host, h.Distribution, formatCounters(h.CacheCounters), h.Entries, plural(h.Entries, "entry", "entries")))
	}
	for _, o := range s.TopObjects {
		lines = append(lines, fmt.Sprintf("Top %s%s: %d %s, %d %s", o.Host, o.Path, o.Requests, plural(int(o.Requests), "request", "requests"),
			o.Misses, plural(int(o.Misses), "miss", "misses")))
	}
	online, synced := 0, 0
	for _, n := range s.Nodes {
		if n.Online {
			online++
		}
		if n.Online && n.Synced {
			synced++
		}
	}
	count := func(n int) string {
		if n == len(s.Nodes) {
			return "all"
		}
		return fmt.Sprint(n)
	}
	lines = append(lines, fmt.Sprintf("Nodes: %d (%s online, %s synced)", len(s.Nodes), count(online), count(synced)))
	for _, n := range s.Nodes {
		state := "online"
		switch {
		case !n.Online:
			state = "offline"
		case !n.Synced:
			state = "online, not synced"
		}
		line := fmt.Sprintf("Node %s (%s, %s): %d %s, memory %s/%s", n.Node, n.Mode, state, n.Entries, plural(n.Entries, "entry", "entries"),
			cli.HumanSize(n.MemoryBytes), cli.HumanSize(n.MemoryLimitBytes))
		if n.DiskLimitBytes > 0 {
			line += fmt.Sprintf(", disk %s/%s", cli.HumanSize(n.DiskBytes), cli.HumanSize(n.DiskLimitBytes))
		}
		line += fmt.Sprintf(", %d evicted, %d invalidated", n.Evictions, n.Invalidations)
		if n.Error != "" {
			line += " (" + n.Error + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func newCacheStatsCmd(e *env) *cobra.Command {
	var (
		domainName string
		top, count int
		watch      bool
		interval   time.Duration
	)
	cmd := &cobra.Command{
		Use:   "stats ALIAS [DIST] [--domain HOST] [--watch]",
		Short: "Show the statistics of the cache",
		Long: `Show the answers of the cache by X-Cache value, the hit ratio, the bytes served
from the cache and from the origin, the entries and their size, per domain and
per node, with the most requested objects (docs/11 §11.8). --watch refreshes
them every --interval until interrupted.`,
		Example: `  nexoctl cache stats local
  nexoctl cache stats local sito-web --domain www.example.com --watch`,
		Args:              func(cmd *cobra.Command, args []string) error { return argsBetween(cmd, args, 1, 2) },
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if interval <= 0 {
				return cli.UsageError("--interval must be positive")
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			q := nexoadmin.CacheStatsQuery{Host: domainName, Top: top}
			if len(args) == 2 {
				q.Distribution = args[1]
			}
			for n := 1; ; n++ {
				st, err := client.CacheStats(ctx, q)
				if err != nil {
					return err
				}
				at := e.deps.Clock.Now()
				text := formatStats(at, st)
				if watch && n > 1 {
					text = "\n" + text
				}
				if err := e.out.Result(cacheStatsRecord{Alias: alias, At: at.UTC(), Stats: *st}, text); err != nil {
					return err
				}
				if !watch || (count > 0 && n >= count) {
					return nil
				}
				t := time.NewTimer(interval)
				select {
				case <-ctx.Done():
					t.Stop()
					return nil
				case <-t.C:
				}
			}
		},
	}
	f := cmd.Flags()
	f.StringVar(&domainName, "domain", "", "statistics of this domain only")
	f.IntVar(&top, "top", 0, "number of top objects (default 10)")
	f.BoolVar(&watch, "watch", false, "refresh the statistics until interrupted")
	f.DurationVar(&interval, "interval", 2*time.Second, "refresh interval of --watch")
	f.IntVar(&count, "count", 0, "with --watch, stop after this many reports (0: never)")
	return cmd
}

// --- webhooks --------------------------------------------------------------------------

// cacheWebhookRecord is a purge webhook.
type cacheWebhookRecord struct {
	Alias        string                 `json:"alias"`
	Distribution string                 `json:"distribution"`
	Action       string                 `json:"action,omitempty"`
	Webhook      nexoadmin.CacheWebhook `json:"webhook"`
}

// cacheWebhookTestRecord is the outcome of the test call of a webhook.
type cacheWebhookTestRecord struct {
	Alias        string                           `json:"alias"`
	Distribution string                           `json:"distribution"`
	Webhook      string                           `json:"webhook"`
	Result       nexoadmin.CacheWebhookTestResult `json:"result"`
}

// targetOf renders the endpoint of a webhook.
func targetOf(w nexoadmin.CacheWebhook) string {
	switch {
	case w.URL != "":
		return w.URL
	case w.Zone != "":
		return "zone " + w.Zone
	}
	return "-"
}

func newCacheWebhookAddCmd(e *env) *cobra.Command {
	var (
		w                     nexoadmin.CacheWebhook
		domains, headers      []string
		tokenFile, secretFile string
		bodyFile              string
	)
	cmd := &cobra.Command{
		Use:   "add ALIAS DIST --type TYPE [--id ID] [--domain HOST]... [flags]",
		Short: "Add a purge webhook to a distribution",
		Long: `Add a webhook purging a CDN or a proxy in front of the cluster on the
invalidations of the distribution, manual and automatic (docs/11 §11.5), with
retries and a persistent queue: cloudflare (--zone, --token), fastly (--zone is
the service id, --token), akamai (--url, --token, --client-token,
--client-secret), nginx (proxy_cache_purge, --url), varnish (BAN, --url) or
generic (--url and --body templates). Without --domain, the webhook purges the
domains not bound to another webhook. Secrets may be ${secret:webhook-NAME}
(NEXO_SECRET_WEBHOOK_NAME on the server); the endpoint must be a public
address or a network of --cache-webhook-allow.`,
		Example: `  nexoctl cache webhook add local sito-web --domain static.example.it --type cloudflare --zone 0123abcd --token-file cf.token
  nexoctl cache webhook add local sito-web --type varnish --url http://varnish:6081 --id varnish`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if w.Type == "" {
				return cli.UsageError("--type is required")
			}
			if w.ID == "" {
				w.ID = w.Type
			}
			var err error
			if w.Token, err = readSecretFlag(cmd, w.Token, tokenFile, "token"); err != nil {
				return err
			}
			if w.ClientSecret, err = readSecretFlag(cmd, w.ClientSecret, secretFile, "client-secret"); err != nil {
				return err
			}
			if w.Headers, err = parseHeaderFlags("header", headers); err != nil {
				return err
			}
			if bodyFile != "" {
				data, err := e.readDocument(bodyFile)
				if err != nil {
					return err
				}
				w.Body = string(data)
			}
			w.Domains = splitList(domains)
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			out, err := client.AddCacheWebhook(ctx, args[1], w)
			if err != nil {
				return fmt.Errorf("distribution %q: %w", args[1], err)
			}
			scope := "every domain"
			if len(out.Domains) > 0 {
				scope = strings.Join(out.Domains, ", ")
			}
			return e.out.Info(cacheWebhookRecord{Alias: alias, Distribution: args[1], Action: "added", Webhook: *out},
				fmt.Sprintf("Added the purge webhook `%s` (%s) of `%s` for %s.", out.ID, out.Type, args[1], scope))
		},
	}
	f := cmd.Flags()
	f.StringVar(&w.ID, "id", "", "id of the webhook (default its type)")
	f.StringVar(&w.Type, "type", "", "cloudflare, fastly, akamai, nginx, varnish or generic")
	f.StringArrayVar(&domains, "domain", nil, "domain whose invalidations call the webhook (repeatable; default every domain)")
	f.StringVar(&w.URL, "url", "", "API base, address of the proxy, or URL template (generic)")
	f.StringVar(&w.Zone, "zone", "", "Cloudflare zone id, Fastly service id")
	f.StringVar(&w.Token, "token", "", "API token (or ${secret:webhook-NAME})")
	f.StringVar(&tokenFile, "token-file", "", "file holding the API token")
	f.StringVar(&w.ClientToken, "client-token", "", "Akamai EdgeGrid client token")
	f.StringVar(&w.ClientSecret, "client-secret", "", "Akamai EdgeGrid client secret (or ${secret:webhook-NAME})")
	f.StringVar(&secretFile, "client-secret-file", "", "file holding the Akamai client secret")
	f.StringVar(&w.Network, "network", "", "Akamai network: production (default) or staging")
	f.StringVar(&w.CPCode, "cp-code", "", "Akamai CP code (purge of a whole domain)")
	f.StringVar(&w.Method, "method", "", "HTTP method (nginx: PURGE, varnish: BAN, generic: POST)")
	f.StringArrayVar(&headers, "header", nil, "header 'Name: value' added to every call (repeatable)")
	f.StringVar(&bodyFile, "body-file", "", "body template of a generic webhook (Go text/template)")
	f.StringVar(&w.Scheme, "scheme", "", "scheme of the purged URLs (default https)")
	return cmd
}

func newCacheWebhookListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS DIST",
		Aliases:           []string{"ls"},
		Short:             "List the purge webhooks of a distribution",
		Example:           "  nexoctl cache webhook ls local sito-web",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			list, err := client.ListCacheWebhooks(ctx, args[1])
			if err != nil {
				return fmt.Errorf("distribution %q: %w", args[1], err)
			}
			if len(list) == 0 {
				return e.out.Note("No purge webhook.")
			}
			for _, w := range list {
				domains := "every domain"
				if len(w.Domains) > 0 {
					domains = strings.Join(w.Domains, ",")
				}
				text := fmt.Sprintf("%-16s %-10s %-30s %s", w.ID, w.Type, domains, targetOf(w))
				if err := e.out.Result(cacheWebhookRecord{Alias: alias, Distribution: args[1], Webhook: w}, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newCacheWebhookRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "remove ALIAS DIST ID",
		Aliases:           []string{"rm"},
		Short:             "Remove a purge webhook",
		Example:           "  nexoctl cache webhook rm local sito-web cloudflare",
		Args:              exactArgs(3),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.DeleteCacheWebhook(ctx, args[1], args[2]); err != nil {
				return fmt.Errorf("webhook %q of %q: %w", args[2], args[1], err)
			}
			return e.out.Info(cacheWebhookRecord{Alias: alias, Distribution: args[1], Action: "removed", Webhook: nexoadmin.CacheWebhook{ID: args[2]}},
				fmt.Sprintf("Removed the purge webhook `%s` of `%s`.", args[2], args[1]))
		},
	}
}

func newCacheWebhookTestCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "test ALIAS DIST ID",
		Short:             "Call a purge webhook once (connection test)",
		Example:           "  nexoctl cache webhook test local sito-web cloudflare",
		Args:              exactArgs(3),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			res, err := client.TestCacheWebhook(ctx, args[1], args[2])
			if err != nil {
				return fmt.Errorf("webhook %q of %q: %w", args[2], args[1], err)
			}
			text := fmt.Sprintf("Webhook `%s`: OK (%d, %d ms).", args[2], res.Status, res.DurationMs)
			if !res.OK {
				text = fmt.Sprintf("Webhook `%s`: failed (%s, %d ms).", args[2], orDash(res.Error), res.DurationMs)
			}
			if err := e.out.Result(cacheWebhookTestRecord{Alias: alias, Distribution: args[1], Webhook: args[2], Result: *res}, text); err != nil {
				return err
			}
			if !res.OK {
				return fmt.Errorf("webhook %s: %s", args[2], orDash(res.Error))
			}
			return nil
		},
	}
}
