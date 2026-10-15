## nexoctl ilm apply

Apply the lifecycle rules of a bucket now (Admin API)

### Synopsis

Apply the lifecycle rules of a bucket at once instead of waiting for the
background scanner: expired versions and delete markers are removed and stale
uploads aborted. Versions protected by Object Lock are kept. Needs
s3:PutLifecycleConfiguration and admin:ApplyLifecycle.

```
nexoctl ilm apply ALIAS/BUCKET [flags]
```

### Examples

```
  nexoctl ilm apply local/app
```

### Options

```
  -h, --help   help for apply
```

### Options inherited from parent commands

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

### SEE ALSO

* [nexoctl ilm](nexoctl_ilm.md)	 - Manage the lifecycle rules of buckets (expiration, noncurrent versions, uploads)

