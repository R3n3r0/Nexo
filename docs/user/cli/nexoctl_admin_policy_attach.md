# nexoctl admin policy attach

Attach policies to a user or a group

```
nexoctl admin policy attach [flags] ALIAS POLICY... (--user USER | --group GROUP)
```

## Examples

```
  nexoctl admin policy attach local readonly --user alice
```

## Options

```
      --group string   the group
  -h, --help           help for attach
      --user string    the user
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

