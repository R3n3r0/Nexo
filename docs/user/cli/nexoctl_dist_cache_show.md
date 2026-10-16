# nexoctl dist cache show

Show the cache settings of a distribution and the effective policy of its domains

## Synopsis

Show the cache settings of a distribution, of its domains and paths, and the
effective policy (with the level each part comes from) of a path (default /)
of each exact domain, or of the domains given.

```
nexoctl dist cache show ALIAS ID [--domain HOST]... [--path PATH] [flags]
```

## Aliases

```
nexoctl dist cache info
```

## Examples

```
  nexoctl dist cache show local sito-web --path /assets/app.js
```

## Options

```
      --domain stringArray   show the effective policy of this domain (repeatable; default the exact domains)
  -h, --help                 help for show
      --path string          path whose effective policy is shown (default "/")
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

* [nexoctl dist cache](nexoctl_dist_cache.md)	 - Manage the cache settings of a distribution, its domains and paths

