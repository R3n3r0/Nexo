# nexoctl admin notify targets test

Send a test event (s3:TestEvent) to a notification target

## Synopsis

Send a test event (s3:TestEvent) to a notification target, named by its ARN or
by "ID:TYPE" (e.g. "\_:webhook" for the default webhook). The command fails when
the target does not receive it.

```
nexoctl admin notify targets test ALIAS TARGET [flags]
```

## Examples

```
  nexoctl admin notify targets test local arn:nexo:sqs:us-east-1:_:webhook
```

## Options

```
  -h, --help   help for test
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

* [nexoctl admin notify targets](nexoctl_admin_notify_targets.md)	 - List and test the notification targets

