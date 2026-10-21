# nexoctl admin metadata-log compact

Compact the metadata log of the drives of a node now

## Synopsis

Rewrite now every segment of the metadata log holding garbage, on the drives of the
node that answers the alias (in a cluster, run it against each node). The live
records are copied and flushed before an old segment is removed: no metadata is
lost. The server also compacts on its own the segments less than half live.

```
nexoctl admin metadata-log compact ALIAS [flags]
```

## Examples

```
  nexoctl admin metadata-log compact local
```

## Options

```
  -h, --help   help for compact
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

* [nexoctl admin metadata-log](nexoctl_admin_metadata-log.md)	 - Inspect and compact the metadata log of the drives

