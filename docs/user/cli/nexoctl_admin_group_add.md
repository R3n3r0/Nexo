## nexoctl admin group add

Create a group or add members to it

### Synopsis

Add users to a group, creating the group when it does not exist. --policy attaches
policies to the group.

```
nexoctl admin group add [flags] ALIAS GROUP [MEMBER...]
```

### Examples

```
  nexoctl admin group add local devs alice bob --policy readwrite
```

### Options

```
  -h, --help             help for add
      --policy strings   policies to attach to the group (repeatable or comma-separated)
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

* [nexoctl admin group](nexoctl_admin_group.md)	 - Manage the IAM groups and their members

