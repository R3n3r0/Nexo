# Events and lifecycle

## Event notifications

Nexo sends a notification, in the S3 event format, when the objects of a bucket are created, read
or deleted. The notifications go to **targets** configured on the server (webhook, Kafka, NATS, AMQP, Redis,
PostgreSQL) and every bucket chooses which events to send to which target.

### 1. Configure a target

A target is configured with the `notify_<type>` subsystem (or `notify_<type>:<id>` to have more than one
of the same type); the configuration takes effect immediately on every node.

**Webhook**: a `POST` with the JSON of the event, with `Authorization: Bearer <auth_token>` if set.

```console
$ nexoctl admin config set local notify_webhook:app enable=on endpoint=https://app.example.com/events auth_token=s3cr3t
Updated notify_webhook:app: auth_token, enable, endpoint.
```

The webhook must answer `2xx`; for an HTTPS endpoint with a private CA add the CA to the system trust
store, for mutual TLS set `client_cert` and `client_key`.

**Kafka**: one message per event on the topic, with key `<bucket>/<object>`.

```bash
nexoctl admin config set local notify_kafka:audit enable=on \
  brokers=kafka1:9092,kafka2:9092 topic=nexo-events \
  tls=on sasl=on sasl_mechanism=sha512 sasl_username=nexo sasl_password='…'
```

`sasl_mechanism` is `plain` (default), `sha256` or `sha512`.

**Other types**: `notify_nats` (`address`, `subject`), `notify_amqp` (`url`, `exchange`, `routing_key`),
`notify_redis` (`address`, `key`), `notify_postgres` (`connection_string`, `table`). The keys of each type
are listed by `nexoctl admin config get local notify_nats` and on the **Configuration** page of the console.

Keys common to every target:

| Key | Meaning |
|-----|---------|
| `enable` | `on`/`off` |
| `queue_dir` | subdirectory of the persistent queue (relative to the queues root on the first local drive) |
| `queue_limit` | maximum number of queued events (default 100000) |
| `comment` | free text |

Every node queues on disk the events of the requests it receives and delivers them, retrying
until the target accepts them: an unreachable target does not block writes and does not lose events as long
as the queue is not full. The credentials (`auth_token`, `sasl_password`, …) are masked in `admin config get`
and in the logs.

### 2. Check the target

```console
$ nexoctl admin notify targets list local
ARN                                      TYPE     STATE    QUEUED  ENDPOINT
arn:nexo:sqs:us-east-1:app:webhook       webhook  online   0       https://app.example.com/events
$ nexoctl admin notify targets test local arn:nexo:sqs:us-east-1:app:webhook
Test event delivered to app:webhook.
```

The **ARN** of a target is `arn:nexo:sqs:<region>:<id>:<type>`, with id `_` for a subsystem without an id
(`notify_webhook` → `arn:nexo:sqs:us-east-1:_:webhook`). `QUEUED` shows the events waiting on the node that
answers: a growing number means a target that is not accepting deliveries.

### 3. Subscribe a bucket

```console
$ nexoctl event add local/photos arn:nexo:sqs:us-east-1:app:webhook --event put --suffix .jpg
Subscribed `arn:nexo:sqs:us-east-1:app:webhook` to the s3:ObjectCreated:* events of `local/photos`.
$ nexoctl event add local/photos arn:nexo:sqs:us-east-1:app:webhook --event delete --prefix 2026/
Subscribed `arn:nexo:sqs:us-east-1:app:webhook` to the s3:ObjectRemoved:* events of `local/photos`.
$ nexoctl event list local/photos
arn:nexo:sqs:us-east-1:app:webhook   s3:ObjectCreated:*   Filter: suffix=".jpg"   ID: nexo-a88d586aef2bdffa
arn:nexo:sqs:us-east-1:app:webhook   s3:ObjectRemoved:*   Filter: prefix="2026/"   ID: nexo-336c095ebad866cb
$ nexoctl event rm local/photos arn:nexo:sqs:us-east-1:app:webhook
```

`--event` accepts `put`, `delete`, `get`, `replica`, `ilm` or the S3 types (`s3:ObjectCreated:Put`,
`s3:ObjectRemoved:DeleteMarkerCreated`, …), comma-separated. The same configuration can be set with
`PutBucketNotificationConfiguration` from any S3 SDK (`QueueConfiguration` with the target's ARN) or from the
**Events** tab of the bucket in the console.

### The message

The body is the S3 event format (`eventVersion` 2.1), with `EventName` and `Key` added at the top level:

```json
{
  "EventName": "s3:ObjectCreated:Put",
  "Key": "photos/2026/trip.jpg",
  "Records": [
    {
      "eventVersion": "2.1",
      "eventSource": "nexo:s3",
      "awsRegion": "us-east-1",
      "eventTime": "2026-10-15T12:14:35.656Z",
      "eventName": "ObjectCreated:Put",
      "userIdentity": { "principalId": "admin" },
      "requestParameters": { "sourceIPAddress": "127.0.0.1" },
      "responseElements": { "x-amz-request-id": "…" },
      "s3": {
        "s3SchemaVersion": "1.0",
        "configurationId": "nexo-a88d586aef2bdffa",
        "bucket": { "name": "photos", "arn": "arn:aws:s3:::photos" },
        "object": { "key": "2026/trip.jpg", "size": 3, "eTag": "…", "sequencer": "…" }
      }
    }
  ]
}
```

The key inside `Records` is encoded as in S3 (`+` for spaces, `%XX` for special characters, `/` unchanged).
Delivery is **at least once**: a consumer must tolerate duplicates (for example by using `sequencer` and
the ETag).

### Example: automatic thumbnails

1. An HTTP service receives the `s3:ObjectCreated:*` events with suffix `.jpg` of the `photos` bucket.
2. For each event it reads the object with a user that has only `s3:GetObject` on `photos` and writes the
   thumbnail to `thumbnails/` with `s3:PutObject` on that bucket.
3. The `thumbnails` bucket does **not** have notifications to the same service, so no loops are created.

## Lifecycle (ILM)

Lifecycle rules automatically delete expired objects, old versions, delete markers left on their own
and abandoned multipart uploads. The server applies them in the background (the scanner walks the
buckets at every cycle, `nexoctl admin config set local scanner cycle=1m`); `nexoctl ilm apply` applies them
right away. Versions protected by Object Lock are never deleted.

### Common rules

```console
$ nexoctl ilm rule add --prefix logs/ --expire-days 30 local/app
Lifecycle rule "rule-1" added to `local/app`.
$ nexoctl ilm rule add --abort-mpu-days 2 --expired-delete-marker local/app
$ nexoctl ilm rule add --noncurrent-expire-days 7 --newer-noncurrent 3 --id versions local/app
$ nexoctl ilm rule add --prefix tmp/ --size-gt 100MiB --expire-days 1 --id large local/app
$ nexoctl ilm rule add --tags 'status=draft' --expire-days 14 --id drafts local/app
$ nexoctl ilm rule list local/app
ID               STATUS   FILTER                         ACTIONS
rule-1           Enabled  prefix=logs/                   expire after 30d
rule-2           Enabled  -                              remove expired delete markers, abort uploads after 2d
versions         Enabled  -                              noncurrent versions expire after 7d (keep 3 newest)
large            Enabled  prefix=tmp/ size>100.0MiB      expire after 1d
drafts           Enabled  tags=status=draft              expire after 14d
```

| Goal | Rule |
|------|------|
| Logs kept for 30 days | `--prefix logs/ --expire-days 30` |
| Cleanup of interrupted multipart uploads | `--abort-mpu-days 2` (recommended on every bucket with large uploads) |
| Versioned bucket: keep 3 previous versions, for 7 days at most | `--noncurrent-expire-days 7 --newer-noncurrent 3` |
| Delete the delete markers left without versions | `--expired-delete-marker` |
| Expiration on a date | `--expire-date 2027-01-01` |
| Only large or small objects | `--size-gt 100MiB`, `--size-lt 1KiB` |
| Only objects with a tag | `--tags 'status=draft'` (several tags: `'a=1&b=2'`) |

With **versioning** enabled (`nexoctl version enable local/app`), the expiration of a current version creates
a delete marker and the version becomes noncurrent: always pair it with a `--noncurrent-expire-days` rule,
otherwise the space is not freed.

### Edit, apply, export

```console
$ nexoctl ilm rule edit --id rule-1 --expire-days 90 local/app
$ nexoctl ilm rule edit --id drafts --disable local/app
$ nexoctl ilm rule remove --id large local/app        # --all removes every rule
$ nexoctl ilm apply local/app
Lifecycle of `local/app` applied: 2 versions checked, 0 deleted, 0 delete markers created, 0 uploads aborted.
$ nexoctl ilm rule export local/app > lifecycle.json
$ nexoctl ilm rule export --xml local/app > lifecycle.xml
$ nexoctl ilm rule import local/app-copy lifecycle.json
```

`ilm rule import` accepts the JSON of the export and of the console or the S3 `LifecycleConfiguration` XML;
a document without rules removes the configuration. The same rules are managed with
`PutBucketLifecycleConfiguration` from any SDK and from the **Lifecycle** tab of the bucket in the console.
`ilm apply` requires `s3:PutLifecycleConfiguration` and `admin:ApplyLifecycle`.

The `Transition` and `NoncurrentVersionTransition` actions move the data of old versions to a remote tier: see
[Remote tiers](tiering.md). A transition naming no tier is refused (`InvalidStorageClass`).

### Lifecycle events

The deletions performed by the lifecycle generate the `s3:LifecycleExpiration:Delete` and
`s3:LifecycleExpiration:DeleteMarkerCreated` events (not `s3:ObjectRemoved:*`): subscribe a target with
`--event ilm` to receive them, for example to keep a record of what was removed.
