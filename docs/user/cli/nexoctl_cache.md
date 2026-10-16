# nexoctl cache

Manage the cache of the distributions

## Synopsis

Manage the HTTP cache of the distributions of a Nexo server (docs/11): the
reusable cache policies, the settings of the cluster, the invalidations (purge),
the warm-ups, the purge webhooks of the CDNs and proxies in front of the
cluster, the inspector of a URL and the statistics. The settings of a
distribution, of its domains and paths are "nexoctl dist cache".

The configuration needs admin:ManageCache; the invalidations and the warm-ups
need admin:InvalidateCache alone (a deploy pipeline).

```
nexoctl cache [flags]
```

## Options

```
  -h, --help   help for cache
```

## Options inherited from parent commands

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

## SEE ALSO

* [nexoctl](nexoctl.md)	 - Nexo command-line client
* [nexoctl cache inspect](nexoctl_cache_inspect.md)	 - Show how the cache serves a URL
* [nexoctl cache policy](nexoctl_cache_policy.md)	 - Manage the reusable cache policies
* [nexoctl cache purge](nexoctl_cache_purge.md)	 - Invalidate cached content
* [nexoctl cache settings](nexoctl_cache_settings.md)	 - Show or change the cache settings of the cluster
* [nexoctl cache stats](nexoctl_cache_stats.md)	 - Show the statistics of the cache
* [nexoctl cache warm](nexoctl_cache_warm.md)	 - Cache objects of a domain before they are requested
* [nexoctl cache webhook](nexoctl_cache_webhook.md)	 - Manage the purge webhooks of a distribution

