## nexoctl admin user

Manage the IAM users

```
nexoctl admin user [flags]
```

### Options

```
  -h, --help   help for user
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
* [nexoctl admin user add](nexoctl_admin_user_add.md)	 - Create a user
* [nexoctl admin user disable](nexoctl_admin_user_disable.md)	 - Disable users (a disabled user cannot sign any request)
* [nexoctl admin user enable](nexoctl_admin_user_enable.md)	 - Enable users (a disabled user cannot sign any request)
* [nexoctl admin user info](nexoctl_admin_user_info.md)	 - Show a user
* [nexoctl admin user list](nexoctl_admin_user_list.md)	 - List the users
* [nexoctl admin user policy](nexoctl_admin_user_policy.md)	 - Show the policies of a user, direct and inherited from its groups
* [nexoctl admin user remove](nexoctl_admin_user_remove.md)	 - Remove users (with their access keys)

