## nexoctl admin lock clear

Release locks on every lock server, whoever holds them

### Synopsis

Release the locks of the given resources ("bucket/key") on every lock server: the
diagnostics of a stuck lock. An operation holding a released lock loses it at its next lease
renewal and stops before its commit.

```
nexoctl admin lock clear ALIAS RESOURCE... [flags]
```

### Examples

```
  nexoctl admin lock clear cluster photos/2024/a.jpg
```

### Options

```
  -h, --help   help for clear
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

* [nexoctl admin lock](nexoctl_admin_lock.md)	 - List and release the namespace locks of a cluster

