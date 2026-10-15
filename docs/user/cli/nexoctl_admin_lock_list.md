## nexoctl admin lock list

List the locks held across the lock servers

```
nexoctl admin lock list ALIAS [flags]
```

### Aliases

```
nexoctl admin lock ls
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

* [nexoctl admin lock](nexoctl_admin_lock.md)	 - List and release the namespace locks of a cluster

