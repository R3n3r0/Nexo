# nexoctl admin decommission

Drain a server pool so that it can be removed

## Synopsis

Drain a server pool: it takes no new object, and every version of its objects moves to the
other pools, verified before the source copy is removed. Once the status reports "complete",
remove the pool from the command line of every node and restart them. The first pool, which
holds the system documents, cannot be decommissioned.

```
nexoctl admin decommission [flags]
```

## Options

```
  -h, --help   help for decommission
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
* [nexoctl admin decommission cancel](nexoctl_admin_decommission_cancel.md)	 - Cancel the decommission of a pool (it takes new objects again)
* [nexoctl admin decommission start](nexoctl_admin_decommission_start.md)	 - Start the decommission of a pool (its 1-based position in the command line)
* [nexoctl admin decommission status](nexoctl_admin_decommission_status.md)	 - Show the pools, their usage and their decommission

