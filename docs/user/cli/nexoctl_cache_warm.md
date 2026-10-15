## nexoctl cache warm

Cache objects of a domain before they are requested

### Synopsis

Cache objects of a domain of a distribution before they are requested (docs/11
§11.6), e.g. after the deploy of a static site: paths and URLs given as
arguments or listed in a file (--from, one per line), every object below a path
prefix (--prefix) or matching a pattern (--glob, --regex), with bounded
concurrency and bandwidth. In the local mode every node warms its own cache.

```
nexoctl cache warm ALIAS DIST [PATH|URL...] [--from FILE] [--prefix P] [flags]
```

### Examples

```
  nexoctl cache warm local sito-web --domain www.example.com --from urls.txt --wait
  nexoctl cache warm local sito-web --prefix /assets/ --bandwidth 20MiB
```

### Options

```
      --bandwidth string   bytes read per second at most (default 64MiB)
      --concurrency int    parallel reads (default 4)
      --domain string      domain the entries are cached for (default the first domain of the distribution)
      --from string        file of URLs or paths to warm, one per line ("-": stdin)
      --glob string        warm every object whose path matches this glob
  -h, --help               help for warm
      --prefix string      warm every object below this path prefix
      --regex string       warm every object whose path matches this RE2 expression
      --timeout duration   how long --wait waits (default 10m0s)
      --wait               wait until the warm-up finishes
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
* [nexoctl cache warm status](nexoctl_cache_warm_status.md)	 - Show the progress of a warm-up on every node

