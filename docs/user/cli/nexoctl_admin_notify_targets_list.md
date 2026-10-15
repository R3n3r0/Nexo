## nexoctl admin notify targets list

List the notification targets with their state

```
nexoctl admin notify targets list ALIAS [flags]
```

### Aliases

```
nexoctl admin notify targets ls
```

### Examples

```
  nexoctl admin notify targets ls local
```

### Options

```
  -h, --help   help for list
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

* [nexoctl admin notify targets](nexoctl_admin_notify_targets.md)	 - List and test the notification targets

