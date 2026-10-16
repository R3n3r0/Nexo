# nexoctl retention set

Set the retention of objects, or the default retention of a bucket

```
nexoctl retention set --mode governance|compliance --validity 30d|1y [flags] ALIAS/BUCKET[/OBJECT|PREFIX]
```

## Examples

```
  nexoctl retention set --mode governance --validity 30d local/vault/report.pdf
  nexoctl retention set --mode compliance --validity 1y -r local/vault/2026/
  nexoctl retention set --default --mode governance --validity 7d local/vault
```

## Options

```
      --bypass              shorten a GOVERNANCE retention (s3:BypassGovernanceRetention)
      --default             set the default retention of the bucket
  -h, --help                help for set
      --mode string         retention mode: governance or compliance
  -r, --recursive           apply to every object under the prefix
      --validity string     retention period: days (30d) or years (1y)
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

* [nexoctl retention](nexoctl_retention.md)	 - Manage the Object Lock retention of objects and buckets

