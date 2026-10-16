# nexoctl admin heal status

Show a healing job, or list the running and recent ones

## Synopsis

Show the state and the outcome of a healing job, or without ID list the running
and recently finished jobs (newest first).

```
nexoctl admin heal status ALIAS [ID] [flags]
```

## Examples

```
  nexoctl admin heal status local
  nexoctl admin heal status local 0192f0f0-0000-7000-8000-000000000001
```

## Options

```
  -h, --help   help for status
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

* [nexoctl admin heal](nexoctl_admin_heal.md)	 - Heal the drives of the server (erasure-coded deployments)

