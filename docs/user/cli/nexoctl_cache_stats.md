## nexoctl cache stats

Show the statistics of the cache

### Synopsis

Show the answers of the cache by X-Cache value, the hit ratio, the bytes served
from the cache and from the origin, the entries and their size, per domain and
per node, with the most requested objects (docs/11 §11.8). --watch refreshes
them every --interval until interrupted.

```
nexoctl cache stats ALIAS [DIST] [--domain HOST] [--watch] [flags]
```

### Examples

```
  nexoctl cache stats local
  nexoctl cache stats local sito-web --domain www.example.com --watch
```

### Options

```
      --count int           with --watch, stop after this many reports (0: never)
      --domain string       statistics of this domain only
  -h, --help                help for stats
      --interval duration   refresh interval of --watch (default 2s)
      --top int             number of top objects (default 10)
      --watch               refresh the statistics until interrupted
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

