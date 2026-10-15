## nexoctl admin service restart

Restart the server process (connections are dropped)

### Synopsis

Restart the server process (connections are dropped).

The command asks for a confirmation on the terminal; --yes skips it (scripts).

```
nexoctl admin service restart [flags] ALIAS
```

### Examples

```
  nexoctl admin service restart --yes local
```

### Options

```
  -h, --help   help for restart
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

