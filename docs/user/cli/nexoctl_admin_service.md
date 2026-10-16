# nexoctl admin service

Restart or stop the server

```
nexoctl admin service [flags]
```

## Options

```
  -h, --help   help for service
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
* [nexoctl admin service restart](nexoctl_admin_service_restart.md)	 - Restart the server process (connections are dropped)
* [nexoctl admin service stop](nexoctl_admin_service_stop.md)	 - Stop the server process (it must be started again by hand)

