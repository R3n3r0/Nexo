package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// newDistCacheCmd returns dist cache: the cache settings of a
// distribution, of its domains and of its paths (docs/11 §11.1).
func newDistCacheCmd(e *env) *cobra.Command {
	cmd := groupCmd("cache", "Manage the cache settings of a distribution, its domains and paths")
	cmd.Long = `Manage the cache settings of a distribution (admin:ManageCache, docs/11 §11.1):
its defaults, the overrides of a domain (--domain) and of the paths a pattern
selects (--behavior). The settings are inherited from the cluster ("nexoctl
cache settings") to the distribution, the domain and the path; every level may
name a policy ("nexoctl cache policy") and override its fields.`
	cmd.AddCommand(newDistCacheSetCmd(e), newDistCacheShowCmd(e), newDistCacheExportCmd(e), newDistCacheImportCmd(e))
	return cmd
}

// distCacheRecord are the cache settings of a distribution.
type distCacheRecord struct {
	Alias  string                      `json:"alias"`
	Action string                      `json:"action,omitempty"`
	Cache  nexoadmin.DistributionCache `json:"cache"`
	// Effective is the effective policy of each domain (dist cache show).
	Effective []distCacheEffective `json:"effective,omitempty"`
}

// distCacheEffective is the effective policy of a path of a domain.
type distCacheEffective struct {
	Host   string                         `json:"host"`
	Path   string                         `json:"path"`
	Policy nexoadmin.CacheEffectivePolicy `json:"policy"`
}

// editDistCache applies change to the cache settings of a distribution:
// read with their ETag, change, replace with If-Match, retried when they
// changed meanwhile.
func editDistCache(ctx context.Context, client cli.AdminClient, id string, change func(c *nexoadmin.DistributionCache) error) (*nexoadmin.DistributionCache, error) {
	for attempt := 1; ; attempt++ {
		cur, err := client.GetDistributionCache(ctx, id)
		if err != nil {
			return nil, err
		}
		etag := cur.ETag
		if err := change(cur); err != nil {
			return nil, err
		}
		cur.Distribution, cur.UpdatedAt = id, nil
		out, err := client.SetDistributionCache(ctx, *cur, etag)
		if errors.Is(err, domain.ErrPreconditionFailed) && attempt < distAttempts {
			continue
		}
		return out, err
	}
}

// samePattern reports whether two patterns are the same.
func samePattern(a, b nexoadmin.KeyPattern) bool {
	return a.Type == b.Type && a.Pattern == b.Pattern && a.CaseInsensitive == b.CaseInsensitive
}

func newDistCacheSetCmd(e *env) *cobra.Command {
	sf := settingsFlags{withQuota: true, withWebhooks: true, withAutoInvalidate: true}
	var (
		domainName, behavior, behaviorType string
		ignoreCase, reset                  bool
		warm                               []string
		noWarm                             bool
	)
	cmd := &cobra.Command{
		Use:   "set ALIAS ID [--domain HOST | --behavior PATTERN] [flags]",
		Short: "Change the cache settings of a distribution, a domain or a path",
		Long: `Change the cache settings of the distribution, or with --domain the override of
one of its domains (an alias, exact or wildcard, or a host a wildcard alias
matches), or with --behavior the override of the paths a pattern selects (a glob
by default, see --behavior-type; the first matching behavior wins). Only the
flags given change; --reset clears the settings of the level first (alone, it
removes the override). The change reaches every node at once and drops the
entries whose settings changed.`,
		Example: `  nexoctl dist cache set local sito-web --policy standard
  nexoctl dist cache set local sito-web --domain www.example.com --policy aggressive --quota 20GiB
  nexoctl dist cache set local sito-web --domain '*.preview.example.com' --disable
  nexoctl dist cache set local sito-web --behavior '/assets/**' --ttl-default 365d
  nexoctl dist cache set local sito-web --behavior '/api/' --behavior-type prefix --disable`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if domainName != "" && behavior != "" {
				return cli.UsageError("--domain and --behavior are mutually exclusive")
			}
			switch behaviorType {
			case nexoadmin.PatternGlob, nexoadmin.PatternPrefix, nexoadmin.PatternRegex:
			default:
				return cli.UsageError("--behavior-type %q: expected glob, prefix or regex", behaviorType)
			}
			if (domainName != "" || behavior != "") && (cmd.Flags().Changed("warm-on-write") || noWarm) {
				return cli.UsageError("--warm-on-write is a setting of the distribution")
			}
			ctx := cmd.Context()
			alias, client, id, err := e.distAlias(ctx, args)
			if err != nil {
				return err
			}
			level := "distribution"
			var settings nexoadmin.CacheSettings
			out, err := editDistCache(ctx, client, id, func(c *nexoadmin.DistributionCache) error {
				var target *nexoadmin.CacheSettings
				removed := false
				switch {
				case domainName != "":
					host := strings.ToLower(strings.TrimSpace(domainName))
					level = "domain " + host
					i := -1
					for j := range c.Domains {
						if strings.EqualFold(c.Domains[j].Host, host) {
							i = j
						}
					}
					if i < 0 {
						c.Domains = append(c.Domains, nexoadmin.CacheDomain{Host: host})
						i = len(c.Domains) - 1
					}
					if reset {
						c.Domains[i].Cache = nexoadmin.CacheSettings{}
					}
					target = &c.Domains[i].Cache
					defer func() {
						if removed {
							c.Domains = append(c.Domains[:i], c.Domains[i+1:]...)
						}
					}()
				case behavior != "":
					p := nexoadmin.KeyPattern{Type: behaviorType, Pattern: behavior, CaseInsensitive: ignoreCase}
					level = "behavior " + formatPattern(&p)
					i := -1
					for j := range c.Behaviors {
						if samePattern(c.Behaviors[j].PathPattern, p) {
							i = j
						}
					}
					if i < 0 {
						c.Behaviors = append(c.Behaviors, nexoadmin.CacheBehavior{PathPattern: p})
						i = len(c.Behaviors) - 1
					}
					if reset {
						c.Behaviors[i].Cache = nexoadmin.CacheSettings{}
					}
					target = &c.Behaviors[i].Cache
					defer func() {
						if removed {
							c.Behaviors = append(c.Behaviors[:i], c.Behaviors[i+1:]...)
						}
					}()
				default:
					if reset {
						c.Cache = nexoadmin.CacheSettings{}
					}
					target = &c.Cache
				}
				changed, err := sf.apply(cmd, target)
				if err != nil {
					return err
				}
				if level == "distribution" {
					if cmd.Flags().Changed("warm-on-write") {
						for _, g := range splitList(warm) {
							c.WarmOnWrite = append(c.WarmOnWrite, nexoadmin.KeyPattern{Type: nexoadmin.PatternGlob, Pattern: g})
						}
						changed = true
					}
					if noWarm {
						c.WarmOnWrite, changed = nil, true
					}
				}
				switch {
				case !changed && !reset:
					return cli.UsageError("nothing to change: give at least one setting (or --reset)")
				case !changed && level != "distribution":
					removed = true
				}
				settings = *target
				return nil
			})
			if err != nil {
				return fmt.Errorf("cache of distribution %q: %w", id, err)
			}
			text := fmt.Sprintf("Cache of `%s`, %s: %s.", id, level, formatSettings(settings))
			if reset && formatSettings(settings) == "inherited" && level != "distribution" {
				text = fmt.Sprintf("Cache of `%s`: removed the override of the %s.", id, level)
			}
			return e.out.Info(distCacheRecord{Alias: alias, Action: "set", Cache: *out}, text)
		},
	}
	f := cmd.Flags()
	f.StringVar(&domainName, "domain", "", "change the override of this domain")
	f.StringVar(&behavior, "behavior", "", "change the override of the paths this pattern selects")
	f.StringVar(&behaviorType, "behavior-type", nexoadmin.PatternGlob, "type of the --behavior pattern: glob, prefix or regex")
	f.BoolVar(&ignoreCase, "ignore-case", false, "match the --behavior pattern case-insensitively")
	f.BoolVar(&reset, "reset", false, "clear the settings of the level first (alone: remove the override)")
	f.StringArrayVar(&warm, "warm-on-write", nil, "cache at once the objects written whose path matches this glob (repeatable)")
	f.BoolVar(&noWarm, "no-warm-on-write", false, "remove the warm-on-write patterns")
	sf.register(cmd)
	return cmd
}

// formatDistCache renders the cache settings of a distribution and the
// effective policy of its domains.
func formatDistCache(c *nexoadmin.DistributionCache, eff []distCacheEffective) string {
	lines := []string{"Distribution: " + c.Distribution, "Default: " + formatSettings(c.Cache)}
	for _, d := range c.Domains {
		lines = append(lines, fmt.Sprintf("Domain %s: %s", d.Host, formatSettings(d.Cache)))
	}
	for i, b := range c.Behaviors {
		lines = append(lines, fmt.Sprintf("Behavior %d (%s): %s", i+1, formatPattern(&b.PathPattern), formatSettings(b.Cache)))
	}
	for _, w := range c.Webhooks {
		domains := "every domain"
		if len(w.Domains) > 0 {
			domains = strings.Join(w.Domains, ",")
		}
		lines = append(lines, fmt.Sprintf("Webhook %s: %s for %s (%s)", w.ID, w.Type, domains, targetOf(w)))
	}
	for _, p := range c.WarmOnWrite {
		lines = append(lines, "Warm on write: "+formatPattern(&p))
	}
	for _, x := range eff {
		p := x.Policy
		state := "enabled"
		if !p.Enabled {
			state = "disabled"
		}
		cc := p.CacheControl
		if cc == "" {
			cc = "the object's"
		}
		ttl := ""
		if p.Policy.TTL != nil {
			ttl = ", TTL " + p.Policy.TTL.Default
		}
		lines = append(lines, fmt.Sprintf("Effective %s%s: %s, policy %s%s, Cache-Control %s", x.Host, x.Path,
			levelOf(state, p.EnabledLevel), levelOf(p.PolicyName, p.PolicyLevel), ttl, cc))
	}
	return strings.Join(lines, "\n")
}

func newDistCacheShowCmd(e *env) *cobra.Command {
	var (
		domains []string
		path    string
	)
	cmd := &cobra.Command{
		Use:     "show ALIAS ID [--domain HOST]... [--path PATH]",
		Aliases: []string{"info"},
		Short:   "Show the cache settings of a distribution and the effective policy of its domains",
		Long: `Show the cache settings of a distribution, of its domains and paths, and the
effective policy (with the level each part comes from) of a path (default /)
of each exact domain, or of the domains given.`,
		Example:           "  nexoctl dist cache show local sito-web --path /assets/app.js",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, id, err := e.distAlias(ctx, args)
			if err != nil {
				return err
			}
			c, err := client.GetDistributionCache(ctx, id)
			if err != nil {
				return fmt.Errorf("distribution %q: %w", id, err)
			}
			hosts := splitList(domains)
			if len(hosts) == 0 {
				d, err := client.GetDistribution(ctx, id)
				if err != nil {
					return fmt.Errorf("distribution %q: %w", id, err)
				}
				for _, a := range d.Aliases {
					if !strings.HasPrefix(a, "*.") {
						hosts = append(hosts, a)
					}
				}
			}
			if path == "" {
				path = "/"
			}
			var eff []distCacheEffective
			for _, h := range hosts {
				p, err := client.EffectiveCachePolicy(ctx, id, h, path)
				if err != nil {
					return fmt.Errorf("effective policy of %s%s: %w", h, path, err)
				}
				eff = append(eff, distCacheEffective{Host: h, Path: path, Policy: *p})
			}
			return e.out.Result(distCacheRecord{Alias: alias, Cache: *c, Effective: eff}, formatDistCache(c, eff))
		},
	}
	cmd.Flags().StringArrayVar(&domains, "domain", nil, "show the effective policy of this domain (repeatable; default the exact domains)")
	cmd.Flags().StringVar(&path, "path", "/", "path whose effective policy is shown")
	return cmd
}

// distCacheExportDocument are the cache settings without the members the
// server computes, as JSON.
func distCacheExportDocument(c nexoadmin.DistributionCache) ([]byte, error) {
	c.ETag, c.UpdatedAt = "", nil
	return json.Marshal(c)
}

func newDistCacheExportCmd(e *env) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "export ALIAS ID [-o FILE]",
		Short: "Export the cache settings of a distribution as YAML",
		Long: `Export the cache settings of a distribution (the document of "nexoctl dist cache
import"); the secrets of the webhooks are masked, and kept by an import.`,
		Example:           "  nexoctl dist cache export local sito-web -o sito-web-cache.yaml",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, id, err := e.distAlias(ctx, args)
			if err != nil {
				return err
			}
			c, err := client.GetDistributionCache(ctx, id)
			if err != nil {
				return fmt.Errorf("distribution %q: %w", id, err)
			}
			if (output == "" || output == "-") && e.out.JSON() {
				return e.out.Result(distCacheRecord{Alias: alias, Cache: *c}, "")
			}
			doc, err := distCacheExportDocument(*c)
			if err != nil {
				return err
			}
			return e.writeYAMLDocuments(alias, output, [][]byte{doc}, "cache settings")
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "write the document to this file")
	return cmd
}

func newDistCacheImportCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "import ALIAS FILE",
		Short: "Replace the cache settings of distributions with those of a YAML file",
		Long: `Replace the cache settings of the distributions of a YAML (or JSON) file of one
or more documents (the format of "nexoctl dist cache export"), each naming its
distribution.`,
		Example:           "  nexoctl dist cache import local sito-web-cache.yaml",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			docs, err := e.readYAMLDocuments(args[1])
			if err != nil {
				return err
			}
			list := make([]nexoadmin.DistributionCache, 0, len(docs))
			for i, doc := range docs {
				var c nexoadmin.DistributionCache
				if err := decodeStrict(doc, &c); err != nil {
					return fmt.Errorf("%w: %s: document %d: %v", domain.ErrInvalidArgument, args[1], i+1, err)
				}
				if c.Distribution == "" {
					return fmt.Errorf("%w: %s: document %d has no distribution", domain.ErrInvalidArgument, args[1], i+1)
				}
				list = append(list, c)
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			for _, c := range list {
				doc := c
				out, err := editDistCache(ctx, client, c.Distribution, func(cur *nexoadmin.DistributionCache) error {
					*cur = doc
					return nil
				})
				if err != nil {
					return fmt.Errorf("cache of distribution %q: %w", c.Distribution, err)
				}
				if err := e.out.Info(distCacheRecord{Alias: alias, Action: "imported", Cache: *out},
					fmt.Sprintf("Cache settings of `%s` imported.", c.Distribution)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newDistInvalidateCmd(e *env) *cobra.Command {
	var pf purgeFlags
	cmd := &cobra.Command{
		Use:   "invalidate ALIAS ID [PATH|GLOB|URL...] [flags]",
		Short: "Invalidate cached content of a distribution",
		Long: `Invalidate the cached entries of a distribution on every node: the same as
"nexoctl cache purge ALIAS ID" (paths, glob patterns, --prefix, --regex, --tag,
--domain, --all, --soft, --wait).`,
		Example:           "  nexoctl dist invalidate local sito-web '/assets/**' --wait",
		Args:              minArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := pf.request(args[2:])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			alias, client, id, err := e.distAlias(ctx, args)
			if err != nil {
				return err
			}
			return e.purge(cmd, alias, client, id, req, &pf)
		},
	}
	pf.register(cmd)
	return cmd
}
