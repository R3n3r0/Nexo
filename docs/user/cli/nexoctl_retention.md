# nexoctl retention

Manage the Object Lock retention of objects and buckets

## Synopsis

Manage the Object Lock retention (S3 WORM) of object versions and the default
retention of buckets. The bucket must have been created with Object Lock
("nexoctl mb --with-lock").

A GOVERNANCE retention can be shortened or removed with --bypass by the users
allowed to (s3:BypassGovernanceRetention); a COMPLIANCE retention can only be
extended, by anyone, root included. The versions under retention cannot be
deleted until it expires.

```
nexoctl retention [flags]
```

## Options

```
  -h, --help   help for retention
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
* [nexoctl retention clear](nexoctl_retention_clear.md)	 - Remove the retention of objects, or the default retention of a bucket
* [nexoctl retention info](nexoctl_retention_info.md)	 - Show the retention of objects, or the default retention of a bucket
* [nexoctl retention set](nexoctl_retention_set.md)	 - Set the retention of objects, or the default retention of a bucket

