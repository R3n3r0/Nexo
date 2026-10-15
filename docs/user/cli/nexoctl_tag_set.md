## nexoctl tag set

Replace the tags of a bucket or object

```
nexoctl tag set ALIAS/BUCKET[/OBJECT] "KEY1=VALUE1&KEY2=VALUE2" [flags]
```

### Examples

```
  nexoctl tag set local/docs/report.pdf "project=nexo&status=final"
```

### Options

```
  -h, --help                help for set
      --version-id string   tag this version of the object
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

