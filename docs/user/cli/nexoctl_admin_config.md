# nexoctl admin config

Read and change the dynamic configuration of the server

## Synopsis

Read and change the dynamic configuration of the server, made of subsystems
(api, browser, logger, notify\_webhook, region, scanner...) holding key=value
settings. Sensitive values are shown masked; setting a key to the mask keeps
the stored secret. "export" and "import" save and restore the whole
configuration as JSON.

```
nexoctl admin config [flags]
```

## Options

```
  -h, --help   help for config
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
* [nexoctl admin config export](nexoctl_admin_config_export.md)	 - Export the whole configuration as JSON (stdout or --output)
* [nexoctl admin config get](nexoctl_admin_config_get.md)	 - Print the settings of every subsystem, or of the named ones
* [nexoctl admin config import](nexoctl_admin_config_import.md)	 - Import a configuration exported by "admin config export" (FILE, or - for stdin)
* [nexoctl admin config reset](nexoctl_admin_config_reset.md)	 - Reset a subsystem, or some of its keys, to the defaults
* [nexoctl admin config set](nexoctl_admin_config_set.md)	 - Change settings of a subsystem (the other keys are kept)

