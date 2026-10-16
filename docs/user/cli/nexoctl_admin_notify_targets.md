# nexoctl admin notify targets

List and test the notification targets

```
nexoctl admin notify targets [flags]
```

## Options

```
  -h, --help   help for targets
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

* [nexoctl admin notify](nexoctl_admin_notify.md)	 - Inspect and test the notification targets
* [nexoctl admin notify targets list](nexoctl_admin_notify_targets_list.md)	 - List the notification targets with their state
* [nexoctl admin notify targets test](nexoctl_admin_notify_targets_test.md)	 - Send a test event (s3:TestEvent) to a notification target

