# nexoctl admin tier edit

Replace the credentials of a tier with those of the remote alias

## Synopsis

Replace the credentials (and the region) of a tier with those of REMOTE\_ALIAS, checked first;
the endpoint, the bucket and the prefix of a tier never change.

```
nexoctl admin tier edit ALIAS NAME REMOTE_ALIAS [flags]
```

## Examples

```
  nexoctl admin tier edit myminio COLD archive-rotated
```

## Options

```
  -h, --help                   help for edit
      --storage-class string   new storage class of the remote objects
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

* [nexoctl admin tier](nexoctl_admin_tier.md)	 - Manage the remote tiers of the lifecycle transitions

