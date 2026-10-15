## nexoctl cors set

Replace the CORS configuration of a bucket (JSON or S3 XML)

### Synopsis

Replace the CORS configuration of a bucket with a document read from FILE or
from the standard input ("-" or no FILE).

```
nexoctl cors set ALIAS/BUCKET [FILE] [flags]
```

### Examples

```
  nexoctl cors set local/site cors.json
  nexoctl cors set local/site cors.xml
```

### Options

```
  -h, --help   help for set
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

