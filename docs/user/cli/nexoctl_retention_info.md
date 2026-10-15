## nexoctl retention info

Show the retention of objects, or the default retention of a bucket

### Synopsis

Show the retention of object versions, or with a bucket (or --default) the
Object Lock configuration of the bucket and its default retention.

```
nexoctl retention info [flags] ALIAS/BUCKET[/OBJECT|PREFIX]
```

### Examples

```
  nexoctl retention info local/vault/report.pdf
  nexoctl retention info -r --versions local/vault/2026/
  nexoctl retention info local/vault
```

### Options

```
      --default             show the default retention of the bucket
  -h, --help                help for info
  -r, --recursive           apply to every object under the prefix
      --version-id string   apply to this version of the object
      --versions            apply to every version instead of the latest one
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

* [nexoctl retention](nexoctl_retention.md)	 - Manage the Object Lock retention of objects and buckets

