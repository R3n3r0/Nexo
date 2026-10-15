## nexoctl dist origin-secret

Manage the origin secret of the proxies in front of the cluster

```
nexoctl dist origin-secret [flags]
```

### Options

```
  -h, --help   help for origin-secret
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

* [nexoctl dist](nexoctl_dist.md)	 - Manage the distributions: custom domains of the buckets
* [nexoctl dist origin-secret rotate](nexoctl_dist_origin-secret_rotate.md)	 - Generate a new origin secret (shown once), keeping the previous ones active

