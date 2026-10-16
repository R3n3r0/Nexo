# nexoctl admin user add

Create a user

## Synopsis

Create a user with its credentials. A user without policies can do nothing until
a policy is attached to it or to one of its groups.

```
nexoctl admin user add [flags] ALIAS ACCESS_KEY SECRET_KEY
```

## Examples

```
  nexoctl admin user add local alice 'S3cr3t-Passw0rd' --policy readwrite
```

## Options

```
      --disabled         create the user disabled
      --group strings    groups to join (repeatable or comma-separated)
  -h, --help             help for add
      --policy strings   policies to attach (repeatable or comma-separated)
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

* [nexoctl admin user](nexoctl_admin_user.md)	 - Manage the IAM users

