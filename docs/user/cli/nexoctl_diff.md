## nexoctl diff

List the differences between two directories or prefixes

### Synopsis

Compare two directories or prefixes by name, size and ETag:
  <  only in FIRST
  >  only in SECOND
  !  different size or content

```
nexoctl diff FIRST SECOND [flags]
```

### Examples

```
  nexoctl diff ./site local/website
```

### Options

```
  -h, --help   help for diff
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

* [nexoctl](nexoctl.md)	 - Nexo command-line client

