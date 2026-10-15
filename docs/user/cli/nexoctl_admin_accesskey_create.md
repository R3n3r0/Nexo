## nexoctl admin accesskey create

Create an access key (the secret key is printed only once)

### Synopsis

Create an access key for a user (by default the user of the alias). The secret
key is generated unless --secret-key is given, and is printed only once: store
it immediately. --expiry accepts a duration (30d, 12h) or a date (RFC 3339 or
2006-01-02); --policy restricts the key with an inline policy document.

```
nexoctl admin accesskey create [flags] ALIAS
```

### Examples

```
  nexoctl admin accesskey create local --expiry 30d --policy photos-ro.json
  nexoctl admin accesskey create local --user alice --name ci --description "CI uploads"
```

### Options

```
      --access-key string    access key ID (default: generated)
      --description string   description of the key
      --expiry string        expiration: a duration (30d) or a date (2026-12-31)
  -h, --help                 help for create
      --name string          name of the key
      --policy string        inline policy document restricting the key (FILE, or - for stdin)
      --secret-key string    secret key (default: generated)
      --user string          parent user of the key (default: the user of the alias)
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

* [nexoctl admin accesskey](nexoctl_admin_accesskey.md)	 - Manage the access keys (service accounts) of the users

