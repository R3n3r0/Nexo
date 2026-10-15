## nexoctl cache policy create

Define a cache policy

### Synopsis

Define a reusable cache policy (docs/11 §11.2), referenced by name by the settings
of the cluster, of a distribution, of a domain or of a path. Its unset fields
take the values of the built-in policy "standard".

```
nexoctl cache policy create ALIAS NAME [flags]
```

### Examples

```
  nexoctl cache policy create local long --ttl-default 1d --ttl-max 30d --query allowlist:v --stale-while-revalidate 5m
  nexoctl cache policy create local assets --file assets-policy.yaml
```

### Options

```
      --allow-type stringArray          media type cached (image/*; repeatable; none: every type)
      --block-size string               size of the blocks of the range caching
      --browser-cache-control string    Cache-Control sent to the clients ("" keeps the one of the object)
      --bypass-authorization            skip the cache for the requests with credentials (false: cached per credential)
      --bypass-cookie stringArray       skip the cache for the requests with this cookie ("*": any; repeatable)
      --bypass-header stringArray       skip the cache for the requests with this header (repeatable)
      --bypass-query stringArray        skip the cache for the requests with this query parameter (repeatable)
      --comment string                  free text
      --compress stringArray            compression algorithm, in order of preference: gzip, br, zstd (repeatable)
      --compress-min-size string        smallest object compressed
      --compress-type stringArray       compressible media type (text/*; repeatable)
      --compression                     store compressed variants of the textual objects
      --deny-type stringArray           media type never cached (repeatable)
      --file string                     YAML or JSON document of the policy (the flags then change it)
  -h, --help                            help for create
      --include-host                    one cache key per domain (false: shared by the aliases)
      --key-cookie stringArray          cookie entering the cache key (repeatable)
      --key-header stringArray          request header entering the cache key (repeatable)
      --max-object-size string          largest object cached
      --min-size string                 smallest object cached
      --negative-ttl string             lifetime of the 404 and 403 answers (0s: not cached)
      --normalize                       normalize the key: sorted query, lowercase, no tracking parameters
      --query string                    query parameters of the cache key: none, all, allowlist:v,w or denylist:utm_source
      --range-caching                   cache the large objects by blocks (range requests, video)
      --respect-origin                  use the Cache-Control and Expires of the objects (bounded by --ttl-min/--ttl-max)
      --stale-if-error string           serve an expired entry when the origin fails, up to this long
      --stale-while-revalidate string   serve an expired entry while refreshing it, up to this long
      --ttl-default string              lifetime in cache of the objects stating none (90s, 1h, 365d)
      --ttl-max string                  longest lifetime in cache, the objects' own included
      --ttl-min string                  shortest lifetime in cache, the objects' own included
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

* [nexoctl cache policy](nexoctl_cache_policy.md)	 - Manage the reusable cache policies

