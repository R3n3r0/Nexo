## nexoctl access rule list

List the access rules of a bucket, in evaluation order

### Synopsis

List the access rules of a bucket in the order the policy engine evaluates them: the deny rules first, then the allow rules, each by priority.

```
nexoctl access rule list ALIAS/BUCKET [flags]
```

### Aliases

```
nexoctl access rule ls
```

### Examples

```
  nexoctl access rule ls local/media
```

### Options

```
  -h, --help   help for list
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

* [nexoctl access rule](nexoctl_access_rule.md)	 - Add, list, show, remove and reorder the access rules of a bucket

