## nexoctl admin group

Manage the IAM groups and their members

```
nexoctl admin group [flags]
```

### Options

```
  -h, --help   help for group
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
* [nexoctl admin group add](nexoctl_admin_group_add.md)	 - Create a group or add members to it
* [nexoctl admin group disable](nexoctl_admin_group_disable.md)	 - Disable groups (the policies of a disabled group do not apply)
* [nexoctl admin group enable](nexoctl_admin_group_enable.md)	 - Enable groups (the policies of a disabled group do not apply)
* [nexoctl admin group info](nexoctl_admin_group_info.md)	 - Show a group
* [nexoctl admin group list](nexoctl_admin_group_list.md)	 - List the groups
* [nexoctl admin group members](nexoctl_admin_group_members.md)	 - List the members of a group
* [nexoctl admin group remove](nexoctl_admin_group_remove.md)	 - Remove members from a group, or the group itself

