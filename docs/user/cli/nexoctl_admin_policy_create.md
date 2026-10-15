## nexoctl admin policy create

Create a policy from a JSON document (FILE, or - for stdin)

### Synopsis

Create a named policy from a JSON policy document. A malformed document is
rejected with the line and the column of each error. --update replaces the
document of an existing policy (or creates it).

```
nexoctl admin policy create [flags] ALIAS NAME FILE
```

### Examples

```
  nexoctl admin policy create local photos-ro photos-ro.json
  cat policy.json | nexoctl admin policy create --update local photos-ro -
```

### Options

```
  -h, --help     help for create
      --update   replace the document when the policy exists
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

* [nexoctl admin policy](nexoctl_admin_policy.md)	 - Manage the IAM policies (AWS policy language)

