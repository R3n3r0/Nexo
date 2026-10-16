# nexoctl admin config set

Change settings of a subsystem (the other keys are kept)

```
nexoctl admin config set ALIAS SUBSYSTEM KEY=VALUE... [flags]
```

## Examples

```
  nexoctl admin config set local region name=eu-south-1 comment="Milan DC"
```

## Options

```
  -h, --help   help for set
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

* [nexoctl admin config](nexoctl_admin_config.md)	 - Read and change the dynamic configuration of the server

