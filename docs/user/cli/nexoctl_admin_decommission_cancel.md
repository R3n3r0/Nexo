# nexoctl admin decommission cancel

Cancel the decommission of a pool (it takes new objects again)

```
nexoctl admin decommission cancel ALIAS POOL [flags]
```

## Options

```
  -h, --help   help for cancel
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

