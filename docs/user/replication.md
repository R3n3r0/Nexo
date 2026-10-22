# Bucket replication

A bucket can replicate its objects to a bucket of another server: another Nexo deployment, or any S3 server
that accepts versioned writes. The copy is asynchronous: the write is acknowledged as soon as the source has
stored it, and a background queue sends it to each remote.

## What is replicated

For each new version that a rule selects:

- the data, the metadata, the content type and the tags of the version, with its **version ID, modification
  time and ETag** kept as they are on the source;
- the **delete markers** (unless the rule turns them off) and, if the rule asks for it, the **permanent
  deletions** of versions;
- later changes of the tags.

The objects encrypted with a key the client supplies (SSE-C) are not replicated: the server never has that
key. The ETag of a multipart upload is kept only for an unencrypted version; an encrypted one is stored with
the ETag the remote computes.

Each version shows its state in the `x-amz-replication-status` header of `HEAD` and `GET`: `PENDING`, then
`COMPLETED` (or `FAILED` after the attempts run out) on the source, and `REPLICA` on the remote.

## Requirements

- **Versioning** enabled on both buckets. While a bucket has replication rules, its versioning cannot be
  suspended.
- Credentials for the remote with `s3:PutObject`, `s3:DeleteObject` and `s3:PutObjectTagging` on the remote
  bucket. When the remote is a Nexo server it also checks `s3:ReplicateObject`, `s3:ReplicateDelete` and
  `s3:ReplicateTags`: these actions let the writer set the version ID and the time of the version.
- On the source, `admin:SetBucketTarget` to register a remote and `s3:PutReplicationConfiguration` to set the
  rules (`admin:GetBucketTarget` and `s3:GetReplicationConfiguration` to read them,
  `s3:ResetBucketReplicationState` for a resync).

## Set it up with nexoctl

Create an alias for each server, then replicate a bucket to the bucket of the other alias:

```bash
nexoctl alias set site-a https://a.example.com AKIA... ...
nexoctl alias set site-b https://b.example.com AKIB... ...
nexoctl replicate add site-a/photos site-b/photos
```

`replicate add` stores the URL and the credentials of `site-b` on `site-a` (sealed, never shown again) and adds
a rule. The options select and tune what the rule sends:

| Option | Effect |
| --- | --- |
| `--prefix docs/` | only the keys under the prefix |
| `--tags class=public` | only the objects with these tags (all of them) |
| `--priority N` | when rules to the same remote select the same key, the highest wins |
| `--no-delete-markers` | keep the delete markers on the source only |
| `--deletes` | also delete versions on the remote when they are deleted on the source |
| `--existing` | let `replicate resync` send the versions written before the rule |
| `--disable` | add the rule disabled |
| `--id ID` | name of the rule (default `REMOTE_ALIAS-BUCKET`); the same ID replaces the rule |

```console
$ nexoctl replicate ls site-a/photos
ID                   PRIORITY REMOTE                                   SELECTS                        REPLICATES
site-b-photos        1        https://b.example.com/photos             every key                      delete markers
$ nexoctl replicate status site-a/photos
TARGET                                             STATE    QUEUED  REPLICATED  SIZE      FAILED  LAST ERROR
arn:nexo:replication::85f953355032:photos          online   0       1250        4.1GiB    0       -
```

`nexoctl replicate rm site-a/photos --id site-b-photos` removes a rule (`--all` removes them all) and the
remotes no rule uses any more. What was already replicated stays on the remote.

The **Replication** tab of a bucket in the console does the same: targets, rules, status and resync.

## Active-active

Replicate each bucket to the other one:

```bash
nexoctl replicate add site-a/shared site-b/shared --deletes
nexoctl replicate add site-b/shared site-a/shared --deletes
```

A version written on one side reaches the other one as a replica and is never sent back. When the same key is
written on both sides at about the same time, both sides end up with both versions and the **most recent one
(by modification time) is the current one on both**: the last writer wins. Keep the clocks of the servers in
sync (NTP).

## Existing objects

A rule replicates the versions written after it. To send those written before, add the rule with
`--existing` and start a resync:

```bash
nexoctl replicate resync site-a/photos site-b/photos
```

The resync walks every version of the bucket, asks the remote whether it already has it and queues the ones it
lacks. It runs on one node of the cluster, survives restarts and is followed with `replicate status`.

## Failures

A version that cannot be sent (remote down, credentials refused) stays queued and is retried with an
increasing delay, from one second up to five minutes, for about thirty attempts; then it is marked `FAILED`
and a resync sends it again. The queue is stored on the drives of the node that took the write, so a restart
does not lose it.

## Limitations

- The counters of `replicate status` are those of the node that answers: in a cluster each node sends the
  writes it received.
- A change of metadata alone (other than the tags) is not replicated; a copy onto the same key is.
- The targets and their credentials are not part of `admin cluster export`. They are sealed with a key derived
  from the root credentials: after changing the root password, register the targets again.
- Site replication (IAM, policies and every bucket across several deployments) is a separate feature.

## Admin API and S3 API

The rules are also served by the S3 API (`PUT/GET/DELETE /{bucket}?replication`, with the ARN of a target as the
destination bucket). The Admin API has `/buckets/{bucket}/replication`, `/replication/targets`,
`/replication/status` and `/replication/resync` (see the [Admin API reference](reference/admin-api.md)).
