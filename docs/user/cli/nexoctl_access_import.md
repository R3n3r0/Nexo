## nexoctl access import

Replace the access rules of a bucket with those of a YAML file

### Synopsis

Replace every access rule of the bucket with those of a YAML (or JSON) document:
a mapping with "rules" (the "bucket" member of an export is ignored: the rules
can be imported into another bucket) or a bare list of rules. The server
validates every rule; an invalid one is reported with its position.

```
nexoctl access import ALIAS/BUCKET FILE [flags]
```

### Examples

```
  nexoctl access import local/media-copy rules.yaml
```

### Options

```
  -h, --help   help for import
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

* [nexoctl access](nexoctl_access.md)	 - Manage the access rules of the buckets (prefix, glob, regex)

