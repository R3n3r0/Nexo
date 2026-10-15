## nexoctl admin service stop

Stop the server process (it must be started again by hand)

### Synopsis

Stop the server process (it must be started again by hand).

The command asks for a confirmation on the terminal; --yes skips it (scripts).

```
nexoctl admin service stop [flags] ALIAS
```

### Examples

```
  nexoctl admin service stop --yes local
```

### Options

```
  -h, --help   help for stop
  -y, --yes    do not ask for a confirmation
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

* [nexoctl admin service](nexoctl_admin_service.md)	 - Restart or stop the server

