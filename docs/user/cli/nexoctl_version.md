## nexoctl version

Print the nexoctl version, or manage bucket versioning

### Synopsis

Without arguments, print the version of nexoctl. The subcommands manage the
versioning of buckets: enable, suspend, info.

```
nexoctl version [flags]
```

### Options

```
  -h, --help   help for version
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
* [nexoctl version enable](nexoctl_version_enable.md)	 - Enable the versioning of a bucket
* [nexoctl version info](nexoctl_version_info.md)	 - Show the versioning state of a bucket
* [nexoctl version suspend](nexoctl_version_suspend.md)	 - Suspend the versioning of a bucket

