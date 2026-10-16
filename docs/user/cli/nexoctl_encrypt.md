# nexoctl encrypt

Manage the default encryption of buckets

## Synopsis

Manage the default server-side encryption of buckets: sse-s3 (AES256) or sse-kms
(aws:kms, with an optional key ID). It applies to the objects written from then
on; existing objects are not rewritten.

```
nexoctl encrypt [flags]
```

## Options

```
  -h, --help   help for encrypt
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
* [nexoctl encrypt clear](nexoctl_encrypt_clear.md)	 - Remove the default encryption of a bucket
* [nexoctl encrypt info](nexoctl_encrypt_info.md)	 - Show the default encryption of a bucket
* [nexoctl encrypt set](nexoctl_encrypt_set.md)	 - Set the default encryption of a bucket

