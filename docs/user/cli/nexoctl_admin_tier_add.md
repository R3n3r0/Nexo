# nexoctl admin tier add

Add a remote tier (its bucket is checked with the credentials of the remote alias)

```
nexoctl admin tier add ALIAS NAME REMOTE_ALIAS/BUCKET [flags]
```

## Examples

```
  nexoctl admin tier add myminio COLD archive/cold-data --prefix nexo/
  nexoctl admin tier add myminio GLACIER-IR aws/archive --storage-class GLACIER_IR
```

## Options

```
  -h, --help                   help for add
      --prefix string          prefix of the objects in the remote bucket (ending with '/')
      --storage-class string   storage class of the remote objects (readable at once)
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

