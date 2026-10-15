## nexoctl dist origin-secret rotate

Generate a new origin secret (shown once), keeping the previous ones active

### Synopsis

Generate a new origin secret for the proxies in front of the cluster: the
requests without one of the active secrets in the header are refused. The new
secret is printed once (the server stores it hashed); the previous one stays
active (--keep N, default 1) until the proxies are updated, then --drop-previous
on the next rotation.

```
nexoctl dist origin-secret rotate ALIAS ID [--header NAME] [--keep N] [--drop-previous] [flags]
```

### Examples

```
  nexoctl dist origin-secret rotate local sito-web
```

### Options

```
      --drop-previous   keep no previous secret
      --header string   header of the secret (default: the current one, or X-Origin-Secret)
  -h, --help            help for rotate
      --keep int        previous secrets kept active (default 1)
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

* [nexoctl dist origin-secret](nexoctl_dist_origin-secret.md)	 - Manage the origin secret of the proxies in front of the cluster

