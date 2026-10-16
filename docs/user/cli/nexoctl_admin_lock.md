# nexoctl admin lock

List and release the namespace locks of a cluster

```
nexoctl admin lock [flags]
```

## Options

```
  -h, --help   help for lock
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
* [nexoctl admin lock clear](nexoctl_admin_lock_clear.md)	 - Release locks on every lock server, whoever holds them
* [nexoctl admin lock list](nexoctl_admin_lock_list.md)	 - List the locks held across the lock servers

