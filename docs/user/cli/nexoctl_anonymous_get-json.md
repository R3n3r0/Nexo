# nexoctl anonymous get-json

Print the bucket policy

## Synopsis

Print the bucket policy document (an empty policy when the bucket has none).

```
nexoctl anonymous get-json ALIAS/BUCKET [flags]
```

## Options

```
  -h, --help   help for get-json
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

