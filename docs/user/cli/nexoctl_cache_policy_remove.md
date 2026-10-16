# nexoctl cache policy remove

Remove a defined cache policy no setting names

```
nexoctl cache policy remove ALIAS NAME [flags]
```

## Aliases

```
nexoctl cache policy rm
```

## Examples

```
  nexoctl cache policy rm local long
```

## Options

```
  -h, --help   help for remove
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

