## nexoctl mv

Move files and objects

### Synopsis

Move local files and objects: copy (like cp) then remove each source once its
copy succeeded.

```
nexoctl mv [flags] SOURCE... TARGET
```

### Examples

```
  nexoctl mv local/inbox/report.pdf local/archive/
  nexoctl mv --recursive local/inbox/ ./downloads/
```

### Options

```
      --attr string              user metadata of the uploaded objects: "key1=value1;key2=value2"
      --checksum string          additional checksum of the uploaded objects, verified by the server: crc32, crc32c, crc64nvme, sha1, sha256
      --content-type string      content type of the uploaded objects (default: from the file extension)
      --enc-c stringArray        SSE-C key of the objects under a location: "ALIAS/BUCKET[/PREFIX]=BASE64KEY" (32 bytes; repeatable). Prefer --enc-c-file or NEXOCTL_ENC_C: a key in the arguments is visible to other local users and in the shell history
      --enc-c-file stringArray   file of SSE-C keys, one "ALIAS/BUCKET[/PREFIX]=BASE64KEY" per line ("#" comments; repeatable)
      --enc-kms stringArray      encrypt the objects written under a location with SSE-KMS: "ALIAS/BUCKET[/PREFIX][=KEY]" (repeatable)
      --enc-s3 stringArray       encrypt the objects written under a location with SSE-S3: "ALIAS/BUCKET[/PREFIX]" (repeatable)
  -h, --help                     help for mv
      --limit-download string    cap the download bandwidth (e.g. 10MiB, per second)
      --limit-upload string      cap the upload bandwidth (e.g. 10MiB, per second)
      --parallel int             parts of a multipart upload sent at once (default 4)
      --part-size string         part size of the multipart uploads, 5MiB-5GiB (default: computed from the size)
  -r, --recursive                copy directories and prefixes
      --tags string              tags of the uploaded objects: "key1=value1&key2=value2"
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

* [nexoctl](nexoctl.md)	 - Nexo command-line client

