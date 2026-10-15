## nexoctl ilm

Manage the lifecycle rules of buckets (expiration, noncurrent versions, uploads)

### Synopsis

Manage the lifecycle (ILM) rules of buckets: the expiration of the objects, of
their noncurrent versions and of the expired delete markers, and the abort of the
incomplete multipart uploads. The server applies the rules in the background;
"ilm apply" applies them at once (Admin API).

The rules travel as the JSON of the Admin API and of the console (ilm rule
export/import), or as the S3 LifecycleConfiguration XML.

```
nexoctl ilm [flags]
```

### Options

```
  -h, --help   help for ilm
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
* [nexoctl ilm apply](nexoctl_ilm_apply.md)	 - Apply the lifecycle rules of a bucket now (Admin API)
* [nexoctl ilm rule](nexoctl_ilm_rule.md)	 - Add, edit, list, remove, export and import lifecycle rules

