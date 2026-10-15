## nexoctl du

Summarize the disk usage of prefixes and directories

```
nexoctl du [flags] TARGET...
```

### Examples

```
  nexoctl du local/photos
  nexoctl du --depth 1 local/photos
```

### Options

```
  -d, --depth int   also print the directories down to this depth
  -h, --help        help for du
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

