# nexoctl replicate

Manage the replication of buckets to other servers (Admin API)

## Synopsis

Manage the replication of buckets to buckets of other S3 servers (Nexo or not).

Every new version of an object a rule selects, its tags and, if the rule says
so, its delete markers and deletions are copied to the remote bucket in the
background, with the version id, the time and the ETag of the source; a
version that cannot be sent is retried with an increasing delay. Both buckets
need versioning, and versioning cannot be suspended while rules exist.

Two buckets replicating to each other (active-active) converge: the replicas
are never sent back, and the most recent version of a key wins on both sides.
Run "replicate add" on each side. "replicate resync" sends the versions that
existed before the rule (for the rules with --existing).

The remote bucket is given as REMOTE\_ALIAS/BUCKET: the URL and the credentials
of that alias are stored, sealed, by the source server, which uses them to
write (s3:PutObject, s3:DeleteObject, s3:PutObjectTagging and, on a Nexo
server, s3:ReplicateObject, s3:ReplicateDelete and s3:ReplicateTags).

```
nexoctl replicate [flags]
```

## Options

```
  -h, --help   help for replicate
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

* [nexoctl](nexoctl.md)	 - Nexo command-line client
* [nexoctl replicate add](nexoctl_replicate_add.md)	 - Replicate a bucket to a remote bucket, or change the rule with the same ID
* [nexoctl replicate ls](nexoctl_replicate_ls.md)	 - List the replication rules of a bucket and their remotes
* [nexoctl replicate resync](nexoctl_replicate_resync.md)	 - Send to a remote the versions written before its rules
* [nexoctl replicate rm](nexoctl_replicate_rm.md)	 - Remove replication rules of a bucket
* [nexoctl replicate status](nexoctl_replicate_status.md)	 - Show the replication of a bucket: queue, counters and resync

