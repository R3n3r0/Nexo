## nexoctl access rule move

Change the priority of an access rule

```
nexoctl access rule move ALIAS/BUCKET --id ID --priority N [flags]
```

### Examples

```
  nexoctl access rule move local/media --id img-pub --priority 1
```

### Options

```
  -h, --help           help for move
      --id string      id of the rule
      --priority int   new priority (lower first)
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

