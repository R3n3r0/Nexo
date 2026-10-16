# nexoctl cache purge

Invalidate cached content

## Synopsis

Invalidate the entries of a distribution on every node (docs/11 §11.5): exact
paths (or absolute URLs of a domain), glob patterns of paths ("/assets/\*\*"),
prefixes (--prefix), regular expressions (--regex), cache tags (--tag), a whole
domain (--domain alone) or everything (--all). Without DIST, --all purges every
distribution. --soft marks the entries expired instead of dropping them. The
purge webhooks of the domains are called too.

"purge status" follows an invalidation, "purge list" lists the history.

```
nexoctl cache purge ALIAS [DIST] [PATH|GLOB|URL...] [flags]
```

## Examples

```
  nexoctl cache purge local sito-web --domain www.example.com '/assets/**' --soft
  nexoctl cache purge local sito-web --tag prodotto-42
  nexoctl cache purge local sito-web --domain static.example.it --all
  nexoctl cache purge local sito-web https://www.example.com/index.html --wait
  nexoctl cache purge local --all
```

## Options

```
      --all                  invalidate everything (of the domain, of the distribution, or of every distribution)
      --domain string        limit the invalidation to a domain of the distribution (alone: the whole domain)
  -h, --help                 help for purge
      --prefix stringArray   path prefix to invalidate (repeatable)
      --regex stringArray    RE2 expression of the paths to invalidate (repeatable)
      --soft                 mark the entries expired (served stale while revalidated) instead of dropping them
      --tag stringArray      cache tag (x-amz-meta-cache-tag) to invalidate (repeatable)
      --timeout duration     how long --wait waits (default 1m0s)
      --wait                 wait until every node applied the invalidation
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

* [nexoctl cache](nexoctl_cache.md)	 - Manage the cache of the distributions
* [nexoctl cache purge list](nexoctl_cache_purge_list.md)	 - List the invalidations of a distribution (the global purges without DIST)
* [nexoctl cache purge status](nexoctl_cache_purge_status.md)	 - Show the state of an invalidation on every node

