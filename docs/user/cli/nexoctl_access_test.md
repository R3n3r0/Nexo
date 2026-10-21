# nexoctl access test

Test the access rules on keys (who may do what)

## Synopsis

Evaluate requests on keys with every source of the policy engine (IAM, bucket
policy, access rules) without performing them: the rules that match, the outcome
of the rules and the final decision with the rule that determined it. The keys
are matched decoded and normalized, as the server does. The principal is
anonymous (default), user:NAME, accesskey:KEY or distribution:ID; testing another
user needs admin:SimulatePolicy. The command does not fail on a denial.

```
nexoctl access test ALIAS/BUCKET/KEY... [--principal P] [--action A] [--ip IP] [--host HOST] [flags]
```

## Examples

```
  nexoctl access test local/media/img/2026/a.jpg --principal anonymous --ip 1.2.3.4 --host cdn.example.com
  nexoctl access test local/media/private/a.jpg local/media/pub/b.jpg --principal user:mario --action write -v
```

## Options

```
      --action string         action: read (default), stream, list, write, delete, tag or an action
      --distribution string   distribution serving the requests
      --header stringArray    request header 'Name: value' (repeatable)
  -h, --help                  help for test
      --host string           host the requests are addressed to
      --ip string             address of the client
      --method string         HTTP method (default: the one of the action)
      --principal string      who makes the requests: anonymous, user:NAME, accesskey:KEY, distribution:ID
      --referer string        Referer of the requests
      --secure string         the requests use HTTPS (true) or not (false)
  -v, --verbose               show every rule with the reason it does not apply
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

* [nexoctl access](nexoctl_access.md)	 - Manage the access rules of the buckets (prefix, glob, regex)

