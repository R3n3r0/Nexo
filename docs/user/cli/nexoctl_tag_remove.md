## nexoctl tag remove

Remove the tags of a bucket or object

```
nexoctl tag remove ALIAS/BUCKET[/OBJECT] [flags]
```

### Aliases

```
nexoctl tag rm
```

### Options

```
  -h, --help                help for remove
      --version-id string   untag this version of the object
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

* [nexoctl tag](nexoctl_tag.md)	 - Manage the tags of buckets and objects

