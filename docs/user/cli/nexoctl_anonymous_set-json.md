# nexoctl anonymous set-json

Replace the bucket policy with a JSON document

## Synopsis

Replace the bucket policy with the document in FILE ("-" reads the standard
input). A document without statements removes the bucket policy.

```
nexoctl anonymous set-json FILE ALIAS/BUCKET [flags]
```

## Examples

```
  nexoctl anonymous set-json policy.json local/website
```

## Options

```
  -h, --help   help for set-json
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

* [nexoctl anonymous](nexoctl_anonymous.md)	 - Manage the anonymous access of buckets and prefixes

