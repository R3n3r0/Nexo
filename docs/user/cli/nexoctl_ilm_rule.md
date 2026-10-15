## nexoctl ilm rule

Add, edit, list, remove, export and import lifecycle rules

```
nexoctl ilm rule [flags]
```

### Options

```
  -h, --help   help for rule
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

* [nexoctl ilm](nexoctl_ilm.md)	 - Manage the lifecycle rules of buckets (expiration, noncurrent versions, uploads)
* [nexoctl ilm rule add](nexoctl_ilm_rule_add.md)	 - Add a lifecycle rule to a bucket
* [nexoctl ilm rule edit](nexoctl_ilm_rule_edit.md)	 - Change a lifecycle rule of a bucket
* [nexoctl ilm rule export](nexoctl_ilm_rule_export.md)	 - Print the lifecycle configuration of a bucket (JSON or S3 XML)
* [nexoctl ilm rule import](nexoctl_ilm_rule_import.md)	 - Replace the lifecycle configuration of a bucket (JSON or S3 XML)
* [nexoctl ilm rule list](nexoctl_ilm_rule_list.md)	 - List the lifecycle rules of a bucket
* [nexoctl ilm rule remove](nexoctl_ilm_rule_remove.md)	 - Remove lifecycle rules of a bucket

