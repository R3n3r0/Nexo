# nexoctl admin decommission start

Start the decommission of a pool (its 1-based position in the command line)

```
nexoctl admin decommission start ALIAS POOL [flags]
```

## Examples

```
  nexoctl admin decommission start cluster 2
```

## Options

```
  -h, --help   help for start
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

* [nexoctl admin decommission](nexoctl_admin_decommission.md)	 - Drain a server pool so that it can be removed

