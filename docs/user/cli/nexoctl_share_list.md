# nexoctl share list

List the active managed share links

## Synopsis

List the active managed share links, the most recent first: your own links, or
with the admin:ManageShares permission every link (--user selects the links of
one user). A BUCKET restricts the list to the links of that bucket.

```
nexoctl share list [flags] ALIAS[/BUCKET]
```

## Aliases

```
nexoctl share ls
```

## Examples

```
  nexoctl share list local
  nexoctl share list --user alice local/photos
```

## Options

```
  -h, --help          help for list
      --user string   list the links of this user (requires admin:ManageShares)
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

* [nexoctl share](nexoctl_share.md)	 - Share objects with links that need no credentials

