# nexoctl rm

Remove objects, object versions and incomplete uploads

## Synopsis

Remove objects. In a versioned bucket removing an object adds a delete
marker; --versions removes every version and delete marker, --version-id one
version. Removing a prefix needs --recursive --force. --dry-run only prints what
would be removed.

Versions protected by Object Lock are not removed (exit code 4): --bypass lifts a
GOVERNANCE retention for the users allowed to (s3:BypassGovernanceRetention);
COMPLIANCE retentions and legal holds are never bypassed.

```
nexoctl rm [flags] ALIAS/BUCKET/OBJECT...
```

## Examples

```
  nexoctl rm local/logs/app.log
  nexoctl rm --recursive --force --older-than 30d local/logs/
  nexoctl rm --versions local/docs/report.pdf
  nexoctl rm --versions --bypass local/vault/draft.txt
  nexoctl rm --incomplete --recursive --force local/uploads/
```

## Options

```
      --bypass              remove versions under a GOVERNANCE retention (s3:BypassGovernanceRetention)
      --dry-run             print what would be removed without removing it
      --force               confirm a recursive removal
  -h, --help                help for rm
  -I, --incomplete          abort the incomplete multipart uploads instead
      --newer-than string   remove only the objects newer than the duration
      --older-than string   remove only the objects older than the duration (e.g. 7d, 12h)
  -r, --recursive           remove everything under the prefix (needs --force)
      --version-id string   remove this version
      --versions            remove every version and delete marker
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

* [nexoctl](nexoctl.md)	 - Nexo command-line client

