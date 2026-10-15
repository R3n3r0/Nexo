## nexoctl cors get

Print the CORS configuration of a bucket (JSON or S3 XML)

```
nexoctl cors get [--xml] ALIAS/BUCKET [flags]
```

### Aliases

```
nexoctl cors info
```

### Examples

```
  nexoctl cors get local/site > cors.json
  nexoctl cors get --xml local/site
```

### Options

```
  -h, --help   help for get
      --xml    print the S3 CORSConfiguration XML
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

* [nexoctl cors](nexoctl_cors.md)	 - Manage the CORS configuration of buckets

