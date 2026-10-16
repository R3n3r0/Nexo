# nexoctl stat

Show the metadata of objects, buckets and files

## Synopsis

Show the metadata of objects (size, ETag, content type, user metadata, tags,
version, Object Lock retention and legal hold, server-side encryption, and with
--checksum the additional checksum), of buckets (versioning, Object Lock,
anonymous access, tags) and of local files. --versions shows every version of
an object.

```
nexoctl stat [flags] TARGET...
```

## Examples

```
  nexoctl stat local/docs/report.pdf
  nexoctl stat --checksum --versions local/docs/report.pdf
  nexoctl stat --enc-c-file keys.txt local/private/secret.txt
  nexoctl stat --json local/docs
```

## Options

```
      --checksum                 show the additional checksum of the object
      --enc-c stringArray        SSE-C key of the objects under a location: "ALIAS/BUCKET[/PREFIX]=BASE64KEY" (32 bytes; repeatable). Prefer --enc-c-file or NEXOCTL_ENC_C: a key in the arguments is visible to other local users and in the shell history
      --enc-c-file stringArray   file of SSE-C keys, one "ALIAS/BUCKET[/PREFIX]=BASE64KEY" per line ("#" comments; repeatable)
  -h, --help                     help for stat
      --version-id string        show this version of the object
      --versions                 show every version of the object
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

