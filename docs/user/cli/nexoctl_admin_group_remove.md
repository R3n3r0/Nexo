# nexoctl admin group remove

Remove members from a group, or the group itself

## Synopsis

Remove the named members from a group; without members, remove the group
itself (its members lose the policies of the group).

```
nexoctl admin group remove ALIAS GROUP [MEMBER...] [flags]
```

## Aliases

```
nexoctl admin group rm
```

## Options

```
  -h, --help   help for remove
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

* [nexoctl admin group](nexoctl_admin_group.md)	 - Manage the IAM groups and their members

