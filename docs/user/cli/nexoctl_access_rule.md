## nexoctl access rule

Add, list, show, remove and reorder the access rules of a bucket

```
nexoctl access rule [flags]
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

* [nexoctl access](nexoctl_access.md)	 - Manage the access rules of the buckets (prefix, glob, regex)
* [nexoctl access rule add](nexoctl_access_rule_add.md)	 - Add an access rule to a bucket
* [nexoctl access rule info](nexoctl_access_rule_info.md)	 - Show an access rule
* [nexoctl access rule list](nexoctl_access_rule_list.md)	 - List the access rules of a bucket, in evaluation order
* [nexoctl access rule move](nexoctl_access_rule_move.md)	 - Change the priority of an access rule
* [nexoctl access rule remove](nexoctl_access_rule_remove.md)	 - Remove an access rule

