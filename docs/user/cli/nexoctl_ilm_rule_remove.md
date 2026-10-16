# nexoctl ilm rule remove

Remove lifecycle rules of a bucket

```
nexoctl ilm rule remove --id ID... | --all ALIAS/BUCKET [flags]
```

## Aliases

```
nexoctl ilm rule rm
```

## Examples

```
  nexoctl ilm rule rm --id rule-1 local/app
  nexoctl ilm rule rm --all local/app
```

## Options

```
      --all          remove every rule (the lifecycle configuration)
  -h, --help         help for remove
      --id strings   IDs of the rules to remove (repeatable or comma-separated)
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

* [nexoctl ilm rule](nexoctl_ilm_rule.md)	 - Add, edit, list, remove, export and import lifecycle rules

