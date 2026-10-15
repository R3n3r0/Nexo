## nexoctl event add

Subscribe a notification target to the events of a bucket

```
nexoctl event add [flags] ALIAS/BUCKET ARN
```

### Examples

```
  nexoctl event add local/photos arn:nexo:sqs:us-east-1:_:webhook --event put --suffix .jpg
  nexoctl event add local/logs arn:nexo:sqs:us-east-1:audit:kafka --event put,delete --prefix app/
```

### Options

```
      --event string      events: put, delete, get, replica, ilm or S3 event types (comma-separated) (default "put,delete,get")
  -h, --help              help for add
      --id string         ID of the subscription (default: assigned by the server)
  -p, --ignore-existing   do not fail when the subscription exists
      --prefix string     only the objects whose key starts with the prefix
      --suffix string     only the objects whose key ends with the suffix
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

* [nexoctl event](nexoctl_event.md)	 - Manage the event notifications of buckets

