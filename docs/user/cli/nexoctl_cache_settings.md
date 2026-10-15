## nexoctl cache settings

Show or change the cache settings of the cluster

### Synopsis

Show the cache settings of the cluster (the global level the distributions,
their domains and their paths inherit from), or change the fields given as
flags.

```
nexoctl cache settings ALIAS [flags]
```

### Examples

```
  nexoctl cache settings local
  nexoctl cache settings local --policy standard --node-header
  nexoctl cache settings local --debug-secret-file debug.secret
```

### Options

```
      --allow-type stringArray          media type cached (image/*; repeatable; none: every type)
      --auto-invalidate                 drop the entries of the objects written (default true) (default true)
      --block-size string               size of the blocks of the range caching
      --browser-cache-control string    Cache-Control sent to the clients ("" keeps the one of the object)
      --bypass-authorization            skip the cache for the requests with credentials (false: cached per credential)
      --bypass-cookie stringArray       skip the cache for the requests with this cookie ("*": any; repeatable)
      --bypass-header stringArray       skip the cache for the requests with this header (repeatable)
      --bypass-query stringArray        skip the cache for the requests with this query parameter (repeatable)
      --compress stringArray            compression algorithm, in order of preference: gzip, br, zstd (repeatable)
      --compress-min-size string        smallest object compressed
      --compress-type stringArray       compressible media type (text/*; repeatable)
      --compression                     store compressed variants of the textual objects
      --debug-header string             request header unlocking X-Cache-Key and X-Cache-Policy (default X-Nexo-Cache-Debug)
      --debug-secret string             value of the debug header (or ${secret:NAME})
      --debug-secret-file string        file holding the value of the debug header
      --deny-type stringArray           media type never cached (repeatable)
      --disable                         turn the cache off (always BYPASS)
      --enable                          turn the cache on
  -h, --help                            help for settings
      --include-host                    one cache key per domain (false: shared by the aliases)
      --key-cookie stringArray          cookie entering the cache key (repeatable)
      --key-header stringArray          request header entering the cache key (repeatable)
      --max-object-size string          largest object cached
      --min-size string                 smallest object cached
      --negative-ttl string             lifetime of the 404 and 403 answers (0s: not cached)
      --no-debug                        remove the debug header
      --node-header                     add X-Cache-Node to every answer
      --normalize                       normalize the key: sorted query, lowercase, no tracking parameters
      --policy string                   cache policy the settings start from (built-in or defined; "" inherits)
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

* [nexoctl cache](nexoctl_cache.md)	 - Manage the cache of the distributions

