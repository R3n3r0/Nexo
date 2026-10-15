## nexoctl admin accesskey

Manage the access keys (service accounts) of the users

### Synopsis

Manage access keys: additional credentials of a user (service accounts), with an
optional expiration and an optional inline policy that restricts what the
key can do (the permissions are the intersection with those of the user).

```
nexoctl admin accesskey [flags]
```

### Aliases

```
nexoctl admin access-key
nexoctl admin svcacct
```

### Options

```
  -h, --help   help for accesskey
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

* [nexoctl admin](nexoctl_admin.md)	 - Administer a Nexo server (Admin API)
* [nexoctl admin accesskey create](nexoctl_admin_accesskey_create.md)	 - Create an access key (the secret key is printed only once)
* [nexoctl admin accesskey edit](nexoctl_admin_accesskey_edit.md)	 - Change an access key: secret, status, expiration, inline policy
* [nexoctl admin accesskey info](nexoctl_admin_accesskey_info.md)	 - Show an access key (never its secret)
* [nexoctl admin accesskey list](nexoctl_admin_accesskey_list.md)	 - List access keys (of the user of the alias, or of --user)
* [nexoctl admin accesskey remove](nexoctl_admin_accesskey_remove.md)	 - Remove access keys (their requests fail immediately)

