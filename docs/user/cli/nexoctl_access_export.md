# nexoctl access export

Export the access rules of a bucket as YAML

## Synopsis

Export the access rules of a bucket as a YAML document (bucket, rules) to FILE,
or to the standard output without FILE (JSON with --json): the document of
"nexoctl access import", for the GitOps management of the rules.

```
nexoctl access export ALIAS/BUCKET [FILE] [flags]
```

## Examples

```
  nexoctl access export local/media rules.yaml
```

## Options

```
  -h, --help   help for export
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

* [nexoctl access](nexoctl_access.md)	 - Manage the access rules of the buckets (prefix, glob, regex)

