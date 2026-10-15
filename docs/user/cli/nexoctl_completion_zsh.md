## nexoctl completion zsh

Generate the autocompletion script for zsh

### Synopsis

Generate the autocompletion script for the zsh shell.

If shell completion is not already enabled in your environment you will need
to enable it.  You can execute the following once:

	echo "autoload -U compinit; compinit" >> ~/.zshrc

To load completions in your current shell session:

	source <(nexoctl completion zsh)

To load completions for every new session, execute once:

#### Linux:

	nexoctl completion zsh > "${fpath[1]}/_nexoctl"

#### macOS:

	nexoctl completion zsh > $(brew --prefix)/share/zsh/site-functions/_nexoctl

You will need to start a new shell for this setup to take effect.


```
nexoctl completion zsh [flags]
```

### Options

```
  -h, --help              help for zsh
      --no-descriptions   disable completion descriptions
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

* [nexoctl completion](nexoctl_completion.md)	 - Generate the autocompletion script for the specified shell

