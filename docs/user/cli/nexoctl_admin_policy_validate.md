# nexoctl admin policy validate

Validate a policy document without saving it

## Synopsis

Validate an IAM policy document (or with --bucket a bucket policy) with the
server: the errors are printed with their line and column, and the command
fails when the document is invalid.

```
nexoctl admin policy validate [flags] ALIAS FILE
```

## Options

```
      --bucket string   validate a bucket policy of this bucket
  -h, --help            help for validate
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

* [nexoctl admin policy](nexoctl_admin_policy.md)	 - Manage the IAM policies (AWS policy language)

