# nexoctl cache policy info

Show a cache policy and the Cache-Control it sends

```
nexoctl cache policy info ALIAS NAME [flags]
```

## Examples

```
  nexoctl cache policy info local aggressive
```

## Options

```
  -h, --help   help for info
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

* [nexoctl cache policy](nexoctl_cache_policy.md)	 - Manage the reusable cache policies

