## nexoctl dist cert list

List the certificates with their status and expiration

```
nexoctl dist cert list ALIAS [flags]
```

### Aliases

```
nexoctl dist cert ls
```

### Options

```
  -h, --help   help for list
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

* [nexoctl dist cert](nexoctl_dist_cert.md)	 - Manage the TLS certificates of the domains of the distributions

