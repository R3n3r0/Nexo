# nexoctl ilm rule import

Replace the lifecycle configuration of a bucket (JSON or S3 XML)

## Synopsis

Replace the lifecycle configuration of a bucket with a document read from FILE
or from the standard input ("-" or no FILE): the JSON of "ilm rule export" and of
the console, or the S3 LifecycleConfiguration XML. A document without rules
removes the configuration.

```
nexoctl ilm rule import ALIAS/BUCKET [FILE] [flags]
```

## Examples

```
  nexoctl ilm rule import local/app lifecycle.json
  nexoctl ilm rule export local/app | nexoctl ilm rule import local/copy
```

## Options

```
  -h, --help   help for import
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

