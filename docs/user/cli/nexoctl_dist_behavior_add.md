# nexoctl dist behavior add

Add a behavior for the paths of a pattern

## Synopsis

Add a behavior: how the paths of a pattern are served. The behaviors are evaluated
in order and the first match wins; the fields a behavior does not set inherit
from the default behavior. The paths include their leading "/".

```
nexoctl dist behavior add ALIAS ID (--prefix|--glob|--regex PATTERN) [--access A] [--cache-control CC] ... [flags]
```

## Examples

```
  nexoctl dist behavior add local website --glob '/assets/**' --cache-control 'public, max-age=31536000, immutable'
  nexoctl dist behavior add local website --regex '^/downloads/.*\.zip$' --access signed --key-group rilasci
  nexoctl dist behavior add local website --prefix /internal/ --access deny --position 1
  nexoctl dist behavior add local website --prefix /photos/ --negotiate-images
```

## Options

```
      --access string          access: public, signed (URLs/cookies of --key-group), authenticated (SigV4) or deny
      --cache-control string   Cache-Control sent to the browsers
      --glob string            paths of the behavior: a glob (*: one segment, **: any number, ?, [abc], {a,b})
      --header stringArray     response header 'Name: value' (repeatable)
  -h, --help                   help for add
      --ignore-case            match the pattern case-insensitively
      --index string           index document appended to the paths ending with /
      --key-group string       key group of the signed URLs and cookies
      --methods stringArray    allowed methods (default GET,HEAD,OPTIONS)
      --negotiate-images       serve the variants of the images (media worker image rule) at their own URL:
                               format from Accept or ?format=, width from ?w=, ?format=original for the original (website mode)
      --position int           1-based position among the behaviors (default: last)
      --prefix string          paths of the behavior: a literal prefix
      --regex string           paths of the behavior: an RE2 regular expression
      --spa string             path served (200) instead of a missing object (single page application)
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

* [nexoctl dist behavior](nexoctl_dist_behavior.md)	 - Manage the behaviors per path of a distribution

