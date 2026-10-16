# nexoctl dist test

Show how a distribution serves a URL: behavior, access, headers

## Synopsis

Show how the distribution serves a URL without reading the object: the behavior
that matches (the first in order, or the default one) with its access, methods
and cache, the object of the path, a redirection or a refusal, and the headers
of the answer. A path is tested on https and the first exact domain.

```
nexoctl dist test ALIAS ID URL|PATH [--method M] [--ip IP] [flags]
```

## Examples

```
  nexoctl dist test local sito-web https://www.example.com/assets/app.js
```

## Options

```
  -h, --help            help for test
      --ip string       address of the client
      --method string   HTTP method (default GET)
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

* [nexoctl dist](nexoctl_dist.md)	 - Manage the distributions: custom domains of the buckets

