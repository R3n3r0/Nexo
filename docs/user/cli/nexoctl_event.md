# nexoctl event

Manage the event notifications of buckets

## Synopsis

Manage the event subscriptions of buckets: the object events (put, delete, get,
or S3 event types such as s3:ObjectCreated:Put) sent to a notification target,
optionally filtered by key prefix and suffix.

The targets are configured on the server (notify\_webhook, notify\_kafka, ... with
"nexoctl admin config set"); "nexoctl admin notify targets ls" prints their ARN.

```
nexoctl event [flags]
```

## Options

```
  -h, --help   help for event
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
* [nexoctl event add](nexoctl_event_add.md)	 - Subscribe a notification target to the events of a bucket
* [nexoctl event list](nexoctl_event_list.md)	 - List the event subscriptions of a bucket
* [nexoctl event remove](nexoctl_event_remove.md)	 - Remove event subscriptions of a bucket

