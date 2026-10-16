# nexoctl access rule remove

Remove an access rule

```
nexoctl access rule remove ALIAS/BUCKET --id ID [flags]
```

## Aliases

```
nexoctl access rule rm
```

## Examples

```
  nexoctl access rule rm local/media --id img-pub
```

## Options

```
  -h, --help        help for remove
      --id string   id of the rule
```

## Options inherited from parent commands

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

## SEE ALSO

* [nexoctl access rule](nexoctl_access_rule.md)	 - Add, list, show, remove and reorder the access rules of a bucket

