## nexoctl dist invalidate

Invalidate cached content of a distribution

### Synopsis

Invalidate the cached entries of a distribution on every node: the same as
"nexoctl cache purge ALIAS ID" (paths, glob patterns, --prefix, --regex, --tag,
--domain, --all, --soft, --wait).

```
nexoctl dist invalidate ALIAS ID [PATH|GLOB|URL...] [flags]
```

### Examples

```
  nexoctl dist invalidate local sito-web '/assets/**' --wait
```

### Options

```
      --all                  invalidate everything (of the domain, of the distribution, or of every distribution)
      --domain string        limit the invalidation to a domain of the distribution (alone: the whole domain)
  -h, --help                 help for invalidate
      --prefix stringArray   path prefix to invalidate (repeatable)
      --regex stringArray    RE2 expression of the paths to invalidate (repeatable)
      --soft                 mark the entries expired (served stale while revalidated) instead of dropping them
      --tag stringArray      cache tag (x-amz-meta-cache-tag) to invalidate (repeatable)
      --timeout duration     how long --wait waits (default 1m0s)
      --wait                 wait until every node applied the invalidation
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

