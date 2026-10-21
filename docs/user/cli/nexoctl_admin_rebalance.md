# nexoctl admin rebalance

Move objects from the fullest pools to the emptiest ones

```
nexoctl admin rebalance [flags]
```

## Options

```
  -h, --help   help for rebalance
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
* [nexoctl admin rebalance start](nexoctl_admin_rebalance_start.md)	 - Start a rebalance between the pools
* [nexoctl admin rebalance status](nexoctl_admin_rebalance_status.md)	 - Show the rebalance
* [nexoctl admin rebalance stop](nexoctl_admin_rebalance_stop.md)	 - Stop the rebalance

