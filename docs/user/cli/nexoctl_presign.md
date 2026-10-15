## nexoctl presign

Generate a raw presigned URL for any method

### Synopsis

Generate a raw presigned URL (GET, HEAD, PUT or DELETE), signed locally with the
credentials of the alias: it is neither managed nor revocable.
Signed headers (--header) must be sent unchanged with the request.

For GET and HEAD the object is checked first (HEAD with the credentials of the
alias): a missing object exits with code 3 and an object the alias may not
read with code 4, instead of printing a URL that would fail. PUT and DELETE
URLs are signed without a check (the object may not exist yet); the
permissions are evaluated when the URL is used.

```
nexoctl presign [flags] ALIAS/BUCKET/OBJECT
```

### Examples

```
  nexoctl presign --method DELETE --expire 10m local/tmp/old.bin
  nexoctl presign --method PUT --header "Content-Type: image/png" local/img/logo.png
```

### Options

```
      --content-disposition string   GET: Content-Disposition of the response; PUT: of the object
      --content-type string          GET: Content-Type of the response; PUT: of the object
  -E, --expire string                validity of the URL (1s to 7d) (default "7d")
  -H, --header stringArray           signed header "Name: value" (PUT: Content-Type, Cache-Control, x-amz-meta-*, ...)
  -h, --help                         help for presign
  -X, --method string                HTTP method: GET, HEAD, PUT or DELETE (default "GET")
      --version-id string            version of the object (GET, HEAD, DELETE)
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

