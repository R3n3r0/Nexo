# nexoctl replicate rm

Remove replication rules of a bucket

## Synopsis

Remove a replication rule of a bucket, or all of them with --all. The targets
no rule uses any more are removed with their stored credentials; what was
already replicated stays on the remotes.

```
nexoctl replicate rm ALIAS/BUCKET (--id ID | --all) [flags]
```

## Aliases

```
nexoctl replicate remove
```

## Examples

```
  nexoctl replicate rm local/photos --id backup-photos
  nexoctl replicate rm local/photos --all
```

## Options

```
      --all         remove every rule
  -h, --help        help for rm
      --id string   the ID of the rule to remove
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

* [nexoctl replicate](nexoctl_replicate.md)	 - Manage the replication of buckets to other servers (Admin API)

