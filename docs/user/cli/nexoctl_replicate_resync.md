# nexoctl replicate resync

Send to a remote the versions written before its rules

## Synopsis

Walk every version of the bucket and send to the remote those its rules with
--existing select and the remote does not have yet. It runs in the background
on the server: follow it with "replicate status".

```
nexoctl replicate resync ALIAS/BUCKET REMOTE_ALIAS/BUCKET [flags]
```

## Examples

```
  nexoctl replicate resync local/photos backup/photos
```

## Options

```
  -h, --help   help for resync
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

