## nexoctl admin kms reseal

Re-seal the data keys of the objects under the current master key

### Synopsis

Re-seal the data key of every SSE-S3 and SSE-KMS object under the current master
key of the KMS, after a rotation of the master key (NEXO\_KMS\_SECRET\_KEY, the
previous one in NEXO\_KMS\_SECRET\_KEY\_PREVIOUS): the data is not rewritten. Once
every bucket is re-sealed, the previous master keys can be removed.

```
nexoctl admin kms reseal ALIAS [flags]
```

### Examples

```
  nexoctl admin kms reseal local
  nexoctl admin kms reseal local --bucket photos
```

### Options

```
      --bucket string   re-seal the objects of this bucket only
  -h, --help            help for reseal
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

* [nexoctl admin kms](nexoctl_admin_kms.md)	 - Show the KMS of the server-side encryption and manage its keys

