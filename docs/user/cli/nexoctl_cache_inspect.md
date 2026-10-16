# nexoctl cache inspect

Show how the cache serves a URL

## Synopsis

Show the cache key of a URL, the effective cache policy with the level each
setting comes from, why the cache would be bypassed, and the entry on every
node (tier, age, freshness, size). With --dist, URL may be a path of the
distribution (completed with its first domain).

```
nexoctl cache inspect ALIAS URL [--dist DIST] [--header 'Name: value']... [flags]
```

## Examples

```
  nexoctl cache inspect local https://www.example.com/assets/app.js
  nexoctl cache inspect local /assets/app.js --dist sito-web --header 'Accept-Encoding: br'
```

## Options

```
      --dist string          distribution of the URL (a path is completed with its first domain)
      --header stringArray   request header of the cache key 'Name: value' (repeatable)
  -h, --help                 help for inspect
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

