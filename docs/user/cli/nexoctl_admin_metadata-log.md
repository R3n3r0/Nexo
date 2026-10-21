# nexoctl admin metadata-log

Inspect and compact the metadata log of the drives

## Synopsis

Inspect and compact the metadata log of the drives (servers started with
--metadata-log): the segments, the share of their bytes still live, the objects
indexed and the memory of the index, the group commits since the start.

```
nexoctl admin metadata-log [flags]
```

## Options

```
  -h, --help   help for metadata-log
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
* [nexoctl admin metadata-log compact](nexoctl_admin_metadata-log_compact.md)	 - Compact the metadata log of the drives of a node now
* [nexoctl admin metadata-log status](nexoctl_admin_metadata-log_status.md)	 - Show the metadata log of every drive of the deployment

