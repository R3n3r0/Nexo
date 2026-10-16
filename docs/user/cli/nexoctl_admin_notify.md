# nexoctl admin notify

Inspect and test the notification targets

## Synopsis

Inspect the notification targets of the server (the notify\_\* subsystems of the
configuration, see "nexoctl admin config set"): their ARN for "nexoctl event
add", their state and the events waiting in their queue on the answering node.

```
nexoctl admin notify [flags]
```

## Options

```
  -h, --help   help for notify
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

* [nexoctl admin](nexoctl_admin.md)	 - Administer a Nexo server (Admin API)
* [nexoctl admin notify targets](nexoctl_admin_notify_targets.md)	 - List and test the notification targets

