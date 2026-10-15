## nexoctl dist keygroup remove

Remove a key group no distribution uses

```
nexoctl dist keygroup remove ALIAS ID [flags]
```

### Aliases

```
nexoctl dist keygroup rm
```

### Options

```
  -h, --help   help for remove
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

* [nexoctl dist keygroup](nexoctl_dist_keygroup.md)	 - Manage the key groups of the signed URLs and cookies

