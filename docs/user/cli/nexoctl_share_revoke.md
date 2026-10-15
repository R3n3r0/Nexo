## nexoctl share revoke

Revoke managed share links (immediately)

### Synopsis

Revoke managed share links by ID (see "share list"): the service account that
signs a link is removed, so the link stops working at once. --all revokes every
link of the caller, or of --user (requires admin:ManageShares).

```
nexoctl share revoke [flags] ALIAS ID... | --all [--user USER] ALIAS
```

### Examples

```
  nexoctl share revoke local 3fz8k2m1q9xw
  nexoctl share revoke --all --user alice local
```

### Options

```
      --all           revoke every link of the caller (or of --user)
  -h, --help          help for revoke
      --user string   with --all: revoke the links of this user
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

* [nexoctl share](nexoctl_share.md)	 - Share objects with links that need no credentials

