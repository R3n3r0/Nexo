# nexoctl admin metadata-log status

Show the metadata log of every drive of the deployment

```
nexoctl admin metadata-log status ALIAS [flags]
```

## Examples

```
  nexoctl admin metadata-log status local
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

* [nexoctl admin metadata-log](nexoctl_admin_metadata-log.md)	 - Inspect and compact the metadata log of the drives

