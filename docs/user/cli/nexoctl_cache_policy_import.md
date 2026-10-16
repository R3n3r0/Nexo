# nexoctl cache policy import

Create or replace the cache policies of a YAML file

```
nexoctl cache policy import ALIAS FILE [flags]
```

## Examples

```
  nexoctl cache policy import local policies.yaml
```

## Options

```
  -h, --help   help for import
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

