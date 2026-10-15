## nexoctl head

Print the first lines of objects and files

```
nexoctl head [flags] TARGET...
```

### Examples

```
  nexoctl head -n 5 local/logs/app.log
```

### Options

```
      --enc-c stringArray        SSE-C key of the objects under a location: "ALIAS/BUCKET[/PREFIX]=BASE64KEY" (32 bytes; repeatable). Prefer --enc-c-file or NEXOCTL_ENC_C: a key in the arguments is visible to other local users and in the shell history
      --enc-c-file stringArray   file of SSE-C keys, one "ALIAS/BUCKET[/PREFIX]=BASE64KEY" per line ("#" comments; repeatable)
  -h, --help                     help for head
  -n, --lines int                number of lines (default 10)
      --version-id string        print this version of the object
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

