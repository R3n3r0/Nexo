# nexoctl admin kms

Show the KMS of the server-side encryption and manage its keys

## Synopsis

Show the KMS that seals the keys of the SSE-S3 and SSE-KMS objects (the builtin
KMS of NEXO\_KMS\_SECRET\_KEY or an external KES server) and manage its keys: a
key created here is usable as SSE-KMS key ("nexoctl encrypt set sse-kms KEY",
"nexoctl cp --enc-kms").

```
nexoctl admin kms [flags]
```

## Options

```
  -h, --help   help for kms
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

* [nexoctl admin](nexoctl_admin.md)	 - Administer a Nexo server (Admin API)
* [nexoctl admin kms key](nexoctl_admin_kms_key.md)	 - Create and list the keys of the KMS
* [nexoctl admin kms reseal](nexoctl_admin_kms_reseal.md)	 - Re-seal the data keys of the objects under the current master key
* [nexoctl admin kms status](nexoctl_admin_kms_status.md)	 - Show the state of the KMS

