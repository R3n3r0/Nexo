# nexoctl replicate status

Show the replication of a bucket: queue, counters and resync

## Synopsis

Show the replication of a bucket to each remote: whether the remote answers,
the versions waiting, those replicated and failed since the server started,
with the last error, and the progress of the last resync. In a cluster the
counters are those of the node that answers.

```
nexoctl replicate status ALIAS/BUCKET [flags]
```

## Options

```
  -h, --help   help for status
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

