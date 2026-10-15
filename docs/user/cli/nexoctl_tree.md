## nexoctl tree

Show the objects and prefixes as a tree

```
nexoctl tree [flags] TARGET...
```

### Examples

```
  nexoctl tree -d local/photos
```

### Options

```
  -d, --dirs-only   show only the directories
  -h, --help        help for tree
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

