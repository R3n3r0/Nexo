# nexoctl legalhold set

Put objects under legal hold

```
nexoctl legalhold set [flags] ALIAS/BUCKET/OBJECT|PREFIX
```

## Examples

```
  nexoctl legalhold set local/vault/report.pdf
  nexoctl legalhold set -r local/vault/case-42/
```

## Options

```
  -h, --help                help for set
  -r, --recursive           apply to every object under the prefix
      --version-id string   apply to this version of the object
      --versions            apply to every version instead of the latest one
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

* [nexoctl legalhold](nexoctl_legalhold.md)	 - Manage the Object Lock legal hold of objects

