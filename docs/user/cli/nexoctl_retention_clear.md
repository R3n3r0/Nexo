## nexoctl retention clear

Remove the retention of objects, or the default retention of a bucket

### Synopsis

Remove the retention of object versions: a GOVERNANCE retention still in force
needs --bypass, a COMPLIANCE one cannot be removed. With --default, remove the
default retention of a bucket (the existing versions keep theirs).

```
nexoctl retention clear [flags] ALIAS/BUCKET[/OBJECT|PREFIX]
```

### Examples

```
  nexoctl retention clear --bypass local/vault/report.pdf
  nexoctl retention clear --default local/vault
```

### Options

```
      --bypass              remove a GOVERNANCE retention still in force (s3:BypassGovernanceRetention)
      --default             remove the default retention of the bucket
  -h, --help                help for clear
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

