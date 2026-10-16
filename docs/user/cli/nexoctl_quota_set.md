# nexoctl quota set

Set the hard quota of a bucket

```
nexoctl quota set ALIAS/BUCKET --size SIZE [flags]
```

## Examples

```
  nexoctl quota set local/backups --size 500GiB
```

## Options

```
  -h, --help          help for set
      --size string   limit, e.g. 10GiB, 500MB or bytes
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

* [nexoctl quota](nexoctl_quota.md)	 - Manage the hard quota of buckets (Admin API)

