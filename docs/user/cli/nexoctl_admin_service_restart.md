# nexoctl admin service restart

Restart the server process (connections are dropped)

## Synopsis

Restart the server process (connections are dropped).

The command asks for a confirmation on the terminal; --yes skips it (scripts).

Without --cluster only the node answering the request (behind a load balancer: any node) restarts.
--cluster restarts every node of the deployment: one at a time by default (a node only when one more
node can stop without losing the write quorum of any erasure set, the next one once it is back), or all
together with --all-at-once. The command waits for the job to end; --no-wait returns at once.

```
nexoctl admin service restart [flags] ALIAS
```

## Examples

```
  nexoctl admin service restart --yes local
  nexoctl admin service restart --cluster --yes mycluster
```

## Options

```
      --all-at-once        with --cluster: restart every node together instead of one at a time
      --cluster            restart every node of the deployment
  -h, --help               help for restart
      --no-wait            return once the job started
      --timeout duration   how long to wait for the job (default 30m0s)
  -y, --yes                do not ask for a confirmation
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

* [nexoctl admin service](nexoctl_admin_service.md)	 - Restart or stop the server

