# nexoctl admin config get

Print the settings of every subsystem, or of the named ones

```
nexoctl admin config get ALIAS [SUBSYSTEM...] [flags]
```

## Examples

```
  nexoctl admin config get local region logger
```

## Options

```
  -h, --help   help for get
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

