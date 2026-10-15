## nexoctl dist redirect

Manage the redirections of a distribution

```
nexoctl dist redirect [flags]
```

### Options

```
  -h, --help   help for redirect
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
* [nexoctl dist redirect add](nexoctl_dist_redirect_add.md)	 - Add a redirection (the first that applies wins)
* [nexoctl dist redirect remove](nexoctl_dist_redirect_remove.md)	 - Remove the redirection N (1-based, see dist info)

