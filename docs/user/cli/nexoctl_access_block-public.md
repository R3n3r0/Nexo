# nexoctl access block-public

Block the public access of the cluster or of a bucket

## Synopsis

Block the access rules granting everyone ("\*"), on every bucket (ALIAS) or on
one (ALIAS/BUCKET). In the reject mode (a setting of the cluster) such a rule
is refused when saved and the stored ones are ignored; in the ignore mode they
are accepted and ignored. Deny rules are never affected.

```
nexoctl access block-public [flags]
```

## Options

```
  -h, --help   help for block-public
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

* [nexoctl access](nexoctl_access.md)	 - Manage the access rules of the buckets (prefix, glob, regex)
* [nexoctl access block-public off](nexoctl_access_block-public_off.md)	 - Stop blocking the public access
* [nexoctl access block-public on](nexoctl_access_block-public_on.md)	 - Block the public access
* [nexoctl access block-public status](nexoctl_access_block-public_status.md)	 - Show the block public access

