# nexoctl event remove

Remove event subscriptions of a bucket

## Synopsis

Remove the subscriptions of a target (ARN), narrowed by --event, --prefix,
--suffix or --id when given; --force without ARN removes every subscription.

```
nexoctl event remove [flags] ALIAS/BUCKET [ARN]
```

## Aliases

```
nexoctl event rm
```

## Examples

```
  nexoctl event rm local/photos arn:nexo:sqs:us-east-1:_:webhook
  nexoctl event rm --force local/photos
```

## Options

```
      --event string    only the subscriptions to these events (default "put,delete,get")
      --force           remove every subscription (without ARN)
  -h, --help            help for remove
      --id string       only the subscription with this ID
      --prefix string   only the subscriptions with this prefix filter
      --suffix string   only the subscriptions with this suffix filter
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

* [nexoctl event](nexoctl_event.md)	 - Manage the event notifications of buckets

