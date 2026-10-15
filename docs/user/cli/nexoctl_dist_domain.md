## nexoctl dist domain

Add and remove the domains (aliases) of a distribution

```
nexoctl dist domain [flags]
```

### Options

```
  -h, --help   help for domain
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
* [nexoctl dist domain add](nexoctl_dist_domain_add.md)	 - Add domains (exact or \*.wildcard) to a distribution
* [nexoctl dist domain remove](nexoctl_dist_domain_remove.md)	 - Remove domains from a distribution

