## nexoctl dist cache

Manage the cache settings of a distribution, its domains and paths

### Synopsis

Manage the cache settings of a distribution (admin:ManageCache, docs/11 §11.1):
its defaults, the overrides of a domain (--domain) and of the paths a pattern
selects (--behavior). The settings are inherited from the cluster ("nexoctl
cache settings") to the distribution, the domain and the path; every level may
name a policy ("nexoctl cache policy") and override its fields.

```
nexoctl dist cache [flags]
```

### Options

```
  -h, --help   help for cache
```

### Options inherited from parent commands

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

### SEE ALSO

* [nexoctl dist](nexoctl_dist.md)	 - Manage the distributions: custom domains of the buckets
* [nexoctl dist cache export](nexoctl_dist_cache_export.md)	 - Export the cache settings of a distribution as YAML
* [nexoctl dist cache import](nexoctl_dist_cache_import.md)	 - Replace the cache settings of distributions with those of a YAML file
* [nexoctl dist cache set](nexoctl_dist_cache_set.md)	 - Change the cache settings of a distribution, a domain or a path
* [nexoctl dist cache show](nexoctl_dist_cache_show.md)	 - Show the cache settings of a distribution and the effective policy of its domains

