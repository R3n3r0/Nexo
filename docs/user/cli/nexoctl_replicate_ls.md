# nexoctl replicate ls

List the replication rules of a bucket and their remotes

```
nexoctl replicate ls ALIAS/BUCKET [flags]
```

## Aliases

```
nexoctl replicate list
```

## Options

```
  -h, --help   help for ls
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

