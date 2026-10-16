# nexoctl access public set

Apply a public access preset to a bucket

## Synopsis

Apply a public access preset, implemented as access rules for everyone (their ids
start with "preset-"; the other rules are kept):

  private           no rule for everyone (also removes the allow rules for everyone)
  public-read       everyone reads the whole bucket
  public-read-list  everyone reads and lists
  public-prefix     everyone reads the keys of --prefix, --glob or --regex
  public-write      everyone writes (needs --confirm)

```
nexoctl access public set private|public-read|public-read-list|public-prefix|public-write ALIAS/BUCKET [flags]
```

## Examples

```
  nexoctl access public set public-read local/media
  nexoctl access public set public-prefix local/media --glob 'pub/**'
```

## Options

```
      --confirm         confirm public-write
      --glob string     keys of public-prefix (and public-write): a glob (*: one segment, **: any number, ?, [abc], {a,b})
  -h, --help            help for set
      --ignore-case     match the pattern case-insensitively
      --prefix string   keys of public-prefix (and public-write): a literal prefix
      --regex string    keys of public-prefix (and public-write): an RE2 regular expression
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

* [nexoctl access public](nexoctl_access_public.md)	 - Apply and show the public access presets of a bucket

