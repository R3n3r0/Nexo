# nexoctl ilm rule export

Print the lifecycle configuration of a bucket (JSON or S3 XML)

```
nexoctl ilm rule export [--xml] ALIAS/BUCKET [flags]
```

## Examples

```
  nexoctl ilm rule export local/app > lifecycle.json
  nexoctl ilm rule export --xml local/app
```

## Options

```
  -h, --help   help for export
      --xml    print the S3 LifecycleConfiguration XML
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

