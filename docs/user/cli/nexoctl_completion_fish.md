# nexoctl completion fish

Generate the autocompletion script for fish

## Synopsis

Generate the autocompletion script for the fish shell.

To load completions in your current shell session:

	nexoctl completion fish | source

To load completions for every new session, execute once:

	nexoctl completion fish > ~/.config/fish/completions/nexoctl.fish

You will need to start a new shell for this setup to take effect.


```
nexoctl completion fish [flags]
```

## Options

```
  -h, --help              help for fish
      --no-descriptions   disable completion descriptions
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

* [nexoctl completion](nexoctl_completion.md)	 - Generate the autocompletion script for the specified shell

